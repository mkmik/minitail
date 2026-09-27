package app_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
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
	args     []string
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

func (d *fakeDaemon) SetArgs(args []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.args = args
}

// lifecycle returns the Start and Stop counts, and the last arguments set.
func (d *fakeDaemon) lifecycle() (starts, stops int, args []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.starts, d.stops, d.args
}

// fakeTailscale records the commands the controller would have run.
type fakeTailscale struct {
	mu        sync.Mutex
	status    *tsctl.Status
	ups       int
	sets      int
	logouts   int
	statusErr error
	upErr     error
	applyErr  error
	applied   []string // the flags of the last Apply
}

func (f *fakeTailscale) Status(context.Context) (*tsctl.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, f.statusErr
}

func (f *fakeTailscale) Up(ctx context.Context, _ []string) error {
	f.mu.Lock()
	f.ups++
	err := f.upErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	// A real `up` blocks until the user completes the login in the browser.
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeTailscale) failUp(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upErr = err
}

func (f *fakeTailscale) Apply(_ context.Context, upArgs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets++
	f.applied = upArgs
	return f.applyErr
}

func (f *fakeTailscale) lastApplied() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applied
}

func (f *fakeTailscale) failApply(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applyErr = err
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

// recorder captures notifications, browser opens and log lines.
type recorder struct {
	mu     sync.Mutex
	notifs []string
	opened []string
	logs   []string
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

func (r *recorder) Logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

func (r *recorder) snapshot() (notifs, opened []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.notifs), slices.Clone(r.opened)
}

// logCount returns how many log lines contain substr.
func (r *recorder) logCount(substr string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, l := range r.logs {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

type harness struct {
	ctrl *app.Controller
	dae  *fakeDaemon
	ts   *fakeTailscale
	rec  *recorder
	path string // the config file Reload reads
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{dae: &fakeDaemon{}, ts: &fakeTailscale{}, rec: &recorder{}}
	cfg := config.Default()
	cfg.Dir = t.TempDir() // never the real config file
	_ = cfg.Resolve()     // for the paths; the binaries are not needed
	cfg.PollInterval = time.Millisecond
	h.path = cfg.Path
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
		Logf:      h.rec.Logf,
	})
	return h
}

// run starts the polling loop for the rest of the test.
func (h *harness) run(t *testing.T) {
	t.Helper()
	go h.ctrl.Run(t.Context())
}

// waitFor blocks until cond holds for the current view, or fails the test.
func (h *harness) waitFor(t *testing.T, what string, cond func(app.View) bool) app.View {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if v := h.ctrl.View(); cond(v) {
			return v
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s; last view: %+v", what, h.ctrl.View())
		case <-time.After(time.Millisecond):
		}
	}
}

// runUntil starts the polling loop and waits for cond. The loop keeps running
// after it returns, so a test can check what further polls do.
func (h *harness) runUntil(t *testing.T, what string, cond func(app.View) bool) app.View {
	t.Helper()
	h.run(t)
	return h.waitFor(t, what, cond)
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
	if ups, _, _ := h.ts.counts(); ups != 1 {
		t.Errorf("Up called %d times, want exactly one `tailscale up` while it waits for the login", ups)
	}
}

// TestControllerPacesFailingLogin: a flag `tailscale up` rejects must be shown
// and retried at the poll interval, not in a tight loop, because unlike a
// pending login a failed `up` returns at once.
func TestControllerPacesFailingLogin(t *testing.T) {
	h := newHarness(t)
	h.ts.setStatus(loadFixture(t, "needs-login"))
	h.ts.failUp(errors.New("flag provided but not defined: -nonsense"))

	v := h.runUntil(t, "the up error to surface", func(v app.View) bool {
		return v.ConfigErr != ""
	})
	if v.State != app.StateNeedsLogin || !strings.Contains(v.Detail, "not defined") {
		t.Errorf("view = %+v, want needs-login with the rejected flag in Detail", v)
	}
	time.Sleep(50 * time.Millisecond)

	// At most one `up` per 1ms poll; a loop that pokes itself after every
	// failure runs thousands in this time.
	if ups, _, _ := h.ts.counts(); ups > 100 {
		t.Errorf("Up called %d times in 50ms, want it paced by the poll interval", ups)
	}
	if n := h.rec.logCount("tailscale up:"); n != 1 {
		t.Errorf("logged the failure %d times, want once", n)
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

	views, unsubscribe := h.ctrl.Subscribe()
	defer unsubscribe()
	h.run(t)

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

	h.runUntil(t, "serving", func(v app.View) bool {
		return v.State == app.StateServing
	})

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

// TestControllerSurfacesConfigErrors covers the most likely failure of a
// hand-edited config file: a flag tailscaled rejects. It must be shown rather
// than retried into the log. And because a daemon that is not answering yet
// fails the same call, the failure must also heal once `set` works again.
func TestControllerSurfacesConfigErrors(t *testing.T) {
	h := newHarness(t)
	h.ts.setStatus(loadFixture(t, "running-approved"))
	h.ts.failApply(errors.New("flag provided but not defined: -nonsense"))

	v := h.runUntil(t, "the config error to surface", func(v app.View) bool {
		return v.State == app.StateBadConfig
	})
	if !strings.Contains(v.Detail, "not defined") {
		t.Errorf("Detail = %q, want it to carry the rejected flag", v.Detail)
	}
	if v.PendingRoutes != nil {
		t.Errorf("PendingRoutes = %v, want none: the node never got them", v.PendingRoutes)
	}

	// Many more polls, each retrying `set`, must add up to one log line.
	time.Sleep(50 * time.Millisecond)
	if _, sets, _ := h.ts.counts(); sets < 3 {
		t.Errorf("Apply called %d times, want it retried on every poll", sets)
	}
	if n := h.rec.logCount("applying"); n != 1 {
		t.Errorf("logged the failure %d times, want once", n)
	}
	if notifs, _ := h.rec.snapshot(); !slices.Contains(notifs, "minitail: Config file rejected") {
		t.Errorf("notifications = %v, want one about the config file", notifs)
	}

	h.ts.failApply(nil)
	h.waitFor(t, "recovery once `set` works", func(v app.View) bool {
		return v.State == app.StateServing && v.ConfigErr == ""
	})
}

// TestControllerReload applies an edited config file to a running node: its
// routes by `tailscale set`, and tailscaled's own flags by a restart, which a
// change to [up] alone must not cause.
func TestControllerReload(t *testing.T) {
	h := newHarness(t)
	h.ts.setStatus(loadFixture(t, "running-approved"))
	h.runUntil(t, "serving", func(v app.View) bool {
		return v.State == app.StateServing
	})
	reload := func(file string) error {
		t.Helper()
		if err := os.WriteFile(h.path, []byte(file), 0o600); err != nil {
			t.Fatal(err)
		}
		return h.ctrl.Reload(t.Context())
	}

	if err := reload("[up]\n--advertise-routes=10.0.0.0/8,nonsense\n"); err == nil {
		t.Error("Reload accepted a route that is not a CIDR prefix")
	}
	if v := h.ctrl.View(); v.State != app.StateServing {
		t.Errorf("State = %q after a refused reload, want it untouched", v.State)
	}

	const routes = "--advertise-routes=10.0.0.0/8,192.168.0.0/16"
	if err := reload("[up]\n" + routes + "\n"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	v := h.waitFor(t, "the new route to be set", func(app.View) bool {
		return slices.Contains(h.ts.lastApplied(), routes)
	})
	if !slices.Equal(v.PendingRoutes, []string{"192.168.0.0/16"}) {
		t.Errorf("PendingRoutes = %v, want the new route awaiting approval", v.PendingRoutes)
	}
	if _, stops, _ := h.dae.lifecycle(); stops != 0 {
		t.Errorf("tailscaled stopped %d times for a change to [up], want none", stops)
	}

	if err := reload("[tailscaled]\n--port=41643\n[up]\n" + routes + "\n"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	starts, stops, args := h.dae.lifecycle()
	if starts != 2 || stops != 1 {
		t.Errorf("tailscaled started %d and stopped %d times, want one restart", starts, stops)
	}
	if !slices.Contains(args, "--port=41643") {
		t.Errorf("tailscaled args = %q, want the new flag", args)
	}
}

func TestControllerReauthenticate(t *testing.T) {
	h := newHarness(t)
	h.ts.setStatus(loadFixture(t, "running-approved"))
	if err := h.ctrl.Reauthenticate(t.Context()); err != nil {
		t.Fatalf("Reauthenticate: %v", err)
	}
	if _, _, logouts := h.ts.counts(); logouts != 1 {
		t.Errorf("logouts = %d, want 1", logouts)
	}
}
