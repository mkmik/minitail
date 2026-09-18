package app_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/mkmik/minitail/internal/app"
	"github.com/mkmik/minitail/internal/config"
	"github.com/mkmik/minitail/internal/tsctl"
)

// fakeDaemon stands in for the tailscaled supervisor.
type fakeDaemon struct {
	mu       sync.Mutex
	running  bool
	restarts int
	givenUp  bool
	starts   int
	stops    int
}

func (d *fakeDaemon) Start(context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.starts++
	d.running = true
}

func (d *fakeDaemon) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stops++
	d.running = false
}

func (d *fakeDaemon) Running() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running
}

func (d *fakeDaemon) Stats() (int, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.restarts, d.givenUp, nil
}

// fakeTailscale records the commands the controller would have run.
type fakeTailscale struct {
	mu        sync.Mutex
	status    *tsctl.Status
	ups       int
	sets      int
	logouts   int
	statusErr error
}

func (f *fakeTailscale) Status(context.Context) (*tsctl.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, f.statusErr
}

func (f *fakeTailscale) Up(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ups++
	return nil
}

func (f *fakeTailscale) Apply(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets++
	return nil
}

func (f *fakeTailscale) Logout(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logouts++
	return nil
}

func (f *fakeTailscale) setStatus(st *tsctl.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = st
}

func (f *fakeTailscale) counts() (ups, sets, logouts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ups, f.sets, f.logouts
}

// recorder captures notifications and browser opens.
type recorder struct {
	mu     sync.Mutex
	notifs []string
	opened []string
}

func (r *recorder) Notify(title, body string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notifs = append(r.notifs, title)
	return nil
}

func (r *recorder) Open(url string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opened = append(r.opened, url)
	return nil
}

func (r *recorder) snapshot() (notifs, opened []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.notifs...), append([]string(nil), r.opened...)
}

type harness struct {
	ctrl *app.Controller
	dae  *fakeDaemon
	ts   *fakeTailscale
	rec  *recorder
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{dae: &fakeDaemon{}, ts: &fakeTailscale{}, rec: &recorder{}}
	cfg := config.Default()
	cfg.PollInterval = time.Millisecond
	// The controller reads only the advertised routes out of the config.
	f, err := config.ParseFile("[up]\n--advertise-routes=10.0.0.0/8\n")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	cfg.File = f
	h.ctrl = app.NewController(app.Options{
		Config:    cfg,
		Daemon:    h.dae,
		Tailscale: h.ts,
		Notifier:  app.NotifyFunc(h.rec.Notify),
		Opener:    app.OpenFunc(h.rec.Open),
	})
	return h
}

// runUntil drives the controller until cond holds or the deadline passes.
func (h *harness) runUntil(t *testing.T, what string, cond func(app.View) bool) app.View {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.ctrl.Run(ctx)

	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s; last view: %+v", what, h.ctrl.View())
		case <-time.After(time.Millisecond):
			if v := h.ctrl.View(); cond(v) {
				return v
			}
		}
	}
}

// TestControllerOpensAuthURLOnce is the NeedsLogin transition: the URL must be
// surfaced exactly once, not on every poll.
func TestControllerOpensAuthURLOnce(t *testing.T) {
	h := newHarness(t)
	h.ts.setStatus(loadFixture(t, "needs-login"))

	h.runUntil(t, "needs-login", func(v app.View) bool {
		return v.State == app.StateNeedsLogin
	})
	// Let several more polls go by.
	time.Sleep(50 * time.Millisecond)

	notifs, opened := h.rec.snapshot()
	if len(opened) != 1 {
		t.Fatalf("opened %v, want exactly one auth URL", opened)
	}
	if opened[0] != "https://login.tailscale.com/a/8f2c41d0b93e" {
		t.Errorf("opened %q, want the fixture's auth URL", opened[0])
	}
	if len(notifs) != 1 {
		t.Errorf("notifications = %v, want exactly one", notifs)
	}
	if ups, _, _ := h.ts.counts(); ups == 0 {
		t.Error("expected the controller to run `tailscale up` to request a login")
	}
}

// TestControllerReAssertsAdvertisement covers the case where tailscaled comes
// back with stored preferences that predate the current config file.
func TestControllerReAssertsAdvertisement(t *testing.T) {
	h := newHarness(t)
	h.ts.setStatus(loadFixture(t, "running-not-approved"))

	v := h.runUntil(t, "not-approved", func(v app.View) bool {
		return v.State == app.StateNotApproved
	})
	if v.Detail == "" {
		t.Error("expected the not-approved state to explain itself")
	}
	time.Sleep(50 * time.Millisecond)

	_, sets, _ := h.ts.counts()
	if sets != 1 {
		t.Errorf("Apply called %d times, want exactly 1 per daemon generation", sets)
	}
	notifs, _ := h.rec.snapshot()
	if len(notifs) != 1 {
		t.Errorf("notifications = %v, want one approval warning", notifs)
	}
}

func TestControllerReachesServingState(t *testing.T) {
	h := newHarness(t)
	h.ts.setStatus(loadFixture(t, "running-approved"))

	v := h.runUntil(t, "serving", func(v app.View) bool {
		return v.State == app.StateServing
	})
	if !v.Userspace {
		t.Error("expected the view to report userspace networking")
	}
	if v.TailnetIP != "100.101.102.103" {
		t.Errorf("TailnetIP = %q", v.TailnetIP)
	}
	if !slices.Equal(v.Routes, []string{"10.0.0.0/8"}) {
		t.Errorf("Routes = %v, want the configured route", v.Routes)
	}
	notifs, _ := h.rec.snapshot()
	if len(notifs) != 1 {
		t.Errorf("notifications = %v, want one 'routing' notice", notifs)
	}
}

// TestControllerRecoversFromNotApproved walks the approval transition the way
// the real admin console does.
func TestControllerRecoversFromNotApproved(t *testing.T) {
	h := newHarness(t)
	h.ts.setStatus(loadFixture(t, "running-not-approved"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	views, unsubscribe := h.ctrl.Subscribe()
	defer unsubscribe()
	go h.ctrl.Run(ctx)

	waitFor := func(want app.State) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case v := <-views:
				if v.State == want {
					return
				}
			case <-deadline:
				t.Fatalf("timed out waiting for %s; last view: %+v", want, h.ctrl.View())
			}
		}
	}

	waitFor(app.StateNotApproved)
	h.ts.setStatus(loadFixture(t, "running-approved"))
	waitFor(app.StateServing)
}

func TestControllerStopIsSticky(t *testing.T) {
	h := newHarness(t)
	h.ts.setStatus(loadFixture(t, "running-approved"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.ctrl.Run(ctx)

	deadline := time.After(5 * time.Second)
	for h.ctrl.View().State != app.StateServing {
		select {
		case <-deadline:
			t.Fatal("never reached the serving state")
		case <-time.After(time.Millisecond):
		}
	}

	h.ctrl.Stop()
	if got := h.ctrl.View().State; got != app.StateStopped {
		t.Fatalf("State = %q after Stop, want %q", got, app.StateStopped)
	}
	time.Sleep(20 * time.Millisecond)
	if got := h.ctrl.View().State; got != app.StateStopped {
		t.Fatalf("State = %q after further polls, want it to stay %q", got, app.StateStopped)
	}
	if h.dae.Running() {
		t.Error("expected the daemon to have been stopped")
	}
}

func TestControllerReauthenticate(t *testing.T) {
	h := newHarness(t)
	h.ts.setStatus(loadFixture(t, "running-approved"))
	if err := h.ctrl.Reauthenticate(context.Background()); err != nil {
		t.Fatalf("Reauthenticate: %v", err)
	}
	if _, _, logouts := h.ts.counts(); logouts != 1 {
		t.Errorf("logouts = %d, want 1", logouts)
	}
}
