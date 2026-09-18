package app

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mkmik/minitail/internal/config"
	"github.com/mkmik/minitail/internal/supervisor"
	"github.com/mkmik/minitail/internal/tsctl"
)

// Notifier posts a desktop notification. It is injected so the controller can
// be tested without a GUI session.
type Notifier interface {
	Notify(title, body string) error
}

// Opener opens a URL in the user's default browser. Also injected.
type Opener interface {
	Open(url string) error
}

// NotifyFunc adapts a function to Notifier.
type NotifyFunc func(title, body string) error

// Notify implements Notifier.
func (f NotifyFunc) Notify(title, body string) error { return f(title, body) }

// OpenFunc adapts a function to Opener.
type OpenFunc func(url string) error

// Open implements Opener.
func (f OpenFunc) Open(url string) error { return f(url) }

// Daemon is the part of supervisor.Supervisor the controller uses.
type Daemon interface {
	Start(ctx context.Context)
	Stop()
	Running() bool
	Stats() (restarts int, givenUp bool, lastErr error)
}

// Options configures a Controller.
type Options struct {
	Config    config.Config
	Daemon    Daemon
	Tailscale tsctl.Client
	Notifier  Notifier
	Opener    Opener
	Logf      func(format string, args ...any)
}

// Controller owns the daemon lifecycle and the polling loop, and publishes a
// View whenever anything changes. It has no dependency on the systray.
type Controller struct {
	opts Options

	mu        sync.Mutex
	want      bool
	status    *tsctl.Status
	statusErr error
	view      View
	subs      map[int]chan View
	nextSub   int

	// openedAuthURL is the last auth URL sent to the browser, so a pending
	// login does not reopen a tab on every poll.
	openedAuthURL string
	// appliedFor is the restart generation for which the configured
	// preferences have already been re-applied, and applyAttempts counts the
	// tries within that generation.
	appliedFor    int
	applyAttempts int
	// applyErr is the last failure to apply the config file, kept so it can
	// be shown rather than only logged.
	applyErr   error
	upInFlight bool

	poke chan struct{}
}

// NewController returns a Controller in the stopped state.
func NewController(opts Options) *Controller {
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if opts.Notifier == nil {
		opts.Notifier = NotifyFunc(func(string, string) error { return nil })
	}
	if opts.Opener == nil {
		opts.Opener = OpenFunc(func(string) error { return nil })
	}
	c := &Controller{
		opts:       opts,
		subs:       map[int]chan View{},
		appliedFor: -1,
		poke:       make(chan struct{}, 1),
	}
	c.view = Derive(c.input())
	return c
}

// View returns the current rendered state.
func (c *Controller) View() View {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.view
}

// Subscribe returns a channel of views and a function to unsubscribe. The
// channel receives the current view immediately and every change after it.
func (c *Controller) Subscribe() (<-chan View, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextSub
	c.nextSub++
	ch := make(chan View, 8)
	ch <- c.view
	c.subs[id] = ch
	return ch, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if sub, ok := c.subs[id]; ok {
			delete(c.subs, id)
			close(sub)
		}
	}
}

// Run drives the controller until ctx is cancelled. It starts the daemon
// immediately, which is what the LaunchAgent wants.
func (c *Controller) Run(ctx context.Context) {
	c.Start(ctx)
	defer c.Stop()

	ticker := time.NewTicker(c.opts.Config.PollInterval)
	defer ticker.Stop()
	for {
		c.pollOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-c.poke:
		}
	}
}

// Start asks for the daemon to be running.
func (c *Controller) Start(ctx context.Context) {
	c.mu.Lock()
	already := c.want
	c.want = true
	c.appliedFor = -1
	c.applyAttempts = 0
	c.applyErr = nil
	c.openedAuthURL = ""
	c.mu.Unlock()
	if !already {
		c.opts.Daemon.Start(ctx)
	}
	c.refresh()
}

// Stop shuts the daemon down and leaves it down.
func (c *Controller) Stop() {
	c.mu.Lock()
	already := !c.want
	c.want = false
	c.status, c.statusErr = nil, nil
	c.mu.Unlock()
	if !already {
		c.opts.Daemon.Stop()
	}
	c.refresh()
}

// Reauthenticate forgets the current login and starts a fresh one. The polling
// loop notices the resulting NeedsLogin state and surfaces the new auth URL.
func (c *Controller) Reauthenticate(ctx context.Context) error {
	if err := c.opts.Tailscale.Logout(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	c.openedAuthURL = ""
	c.appliedFor = -1
	c.mu.Unlock()
	c.Poke()
	return nil
}

// Poke asks the polling loop to run immediately instead of waiting for the
// next tick.
func (c *Controller) Poke() {
	select {
	case c.poke <- struct{}{}:
	default:
	}
}

func (c *Controller) pollOnce(ctx context.Context) {
	c.mu.Lock()
	want := c.want
	c.mu.Unlock()

	if want && c.opts.Daemon.Running() {
		st, err := c.opts.Tailscale.Status(ctx)
		c.mu.Lock()
		if err != nil {
			c.statusErr = err
		} else {
			c.status, c.statusErr = st, nil
		}
		c.mu.Unlock()
	}

	prev := c.View()
	cur := c.refresh()
	c.react(ctx, prev, cur)
}

// input snapshots everything Derive needs. Caller must not hold c.mu.
func (c *Controller) input() Input {
	c.mu.Lock()
	want, status, statusErr, applyErr := c.want, c.status, c.statusErr, c.applyErr
	c.mu.Unlock()

	advertised, err := c.opts.Config.File.AdvertisedRoutes()
	if err != nil {
		// A malformed --advertise-routes is worth saying out loud, but it
		// must not stop the rest of the status from rendering.
		c.opts.Logf("reading advertised routes from the config file: %v", err)
	}

	in := Input{
		WantRunning: want,
		Status:      status,
		StatusErr:   statusErr,
		ApplyErr:    applyErr,
		Advertised:  advertised,
	}
	if err != nil {
		// A config file minitail cannot even read the routes out of is worth
		// showing in the same place as one tailscaled rejected.
		in.ApplyErr = err
	}
	if c.opts.Daemon != nil {
		in.DaemonRunning = c.opts.Daemon.Running()
		in.Restarts, in.GivenUp, in.DaemonErr = c.opts.Daemon.Stats()
	}
	return in
}

// refresh recomputes the view and publishes it if it changed.
func (c *Controller) refresh() View {
	v := Derive(c.input())

	c.mu.Lock()
	changed := !sameView(c.view, v)
	c.view = v
	var subs []chan View
	if changed {
		for _, ch := range c.subs {
			subs = append(subs, ch)
		}
	}
	c.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- v:
		default: // a slow subscriber gets the next update instead
		}
	}
	return v
}

// react performs the side effects a transition calls for. It is small on
// purpose: everything it decides is derived from the two views plus the
// controller's own "already did this" bookkeeping.
func (c *Controller) react(ctx context.Context, prev, cur View) {
	switch cur.State {
	case StateNeedsLogin:
		c.startLogin(ctx)
		if cur.AuthURL != "" {
			c.mu.Lock()
			fresh := c.openedAuthURL != cur.AuthURL
			if fresh {
				c.openedAuthURL = cur.AuthURL
			}
			c.mu.Unlock()
			if fresh {
				c.opts.Logf("login required: %s", cur.AuthURL)
				c.notify("minitail: login required", "Opening the Tailscale login page for this exit node.")
				if err := c.opts.Opener.Open(cur.AuthURL); err != nil {
					c.opts.Logf("opening auth URL: %v", err)
				}
			}
		}

	case StateDown:
		// tailscaled has state but the node was brought down; bring it back up.
		c.startLogin(ctx)

	case StateNotApproved:
		c.applyPrefs(ctx)
		if prev.State != StateNotApproved {
			c.notify("minitail: approval needed",
				"Connected, but "+strings.Join(cur.PendingRoutes, ", ")+
					" still needs approval in the Tailscale admin console.")
		}

	case StateServing:
		c.applyPrefs(ctx)
		if prev.State != StateServing {
			body := cur.Detail
			if body == "" {
				body = "This Mac is now serving its tailnet."
			}
			c.notify("minitail: "+strings.ToLower(cur.Summary), body)
		}

	case StateError:
		if prev.State != StateError {
			c.notify("minitail: tailscaled failed", cur.Detail)
		}
	}
}

// startLogin runs `tailscale up` in the background. It blocks until the login
// completes, so it gets its own goroutine and a guard against overlapping runs.
func (c *Controller) startLogin(ctx context.Context) {
	c.mu.Lock()
	if c.upInFlight {
		c.mu.Unlock()
		return
	}
	c.upInFlight = true
	c.mu.Unlock()

	go func() {
		err := c.opts.Tailscale.Up(ctx)
		c.mu.Lock()
		c.upInFlight = false
		c.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			c.opts.Logf("tailscale up: %v", err)
		}
		c.Poke()
	}()
}

// maxApplyAttempts bounds the retries of `tailscale set` within one tailscaled
// generation. A flag the daemon rejects is a mistake in the config file, and
// retrying it every poll would only fill the log; after this many tries the
// error is surfaced instead.
const maxApplyAttempts = 3

// applyPrefs re-applies the configured preferences once per tailscaled
// generation. tailscaled persists preferences, so a node logged in before the
// config file was edited would otherwise come back advertising the old routes.
func (c *Controller) applyPrefs(ctx context.Context) {
	restarts, _, _ := c.opts.Daemon.Stats()
	c.mu.Lock()
	if c.appliedFor == restarts {
		c.mu.Unlock()
		return
	}
	c.appliedFor = restarts
	c.mu.Unlock()

	err := c.opts.Tailscale.Apply(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.applyAttempts, c.applyErr = 0, nil
		return
	}
	c.applyAttempts++
	c.applyErr = err
	c.opts.Logf("applying the config file: %v", err)
	if c.applyAttempts < maxApplyAttempts {
		c.appliedFor = -1 // retry on the next poll
		return
	}
	c.opts.Logf("giving up on the config file after %d attempts; fix %s and restart",
		c.applyAttempts, c.opts.Config.Path)
}

func (c *Controller) notify(title, body string) {
	if err := c.opts.Notifier.Notify(title, body); err != nil {
		c.opts.Logf("notify: %v", err)
	}
}

// sameView compares the fields that matter for rendering.
func sameView(a, b View) bool {
	if a.State != b.State || a.Summary != b.Summary || a.Detail != b.Detail ||
		a.AuthURL != b.AuthURL || a.TailnetIP != b.TailnetIP ||
		a.TailnetName != b.TailnetName || a.Hostname != b.Hostname ||
		a.ConfigErr != b.ConfigErr ||
		a.Userspace != b.Userspace || a.Restarts != b.Restarts ||
		a.CanStart != b.CanStart || a.CanStop != b.CanStop {
		return false
	}
	return slices.Equal(a.Health, b.Health) &&
		slices.Equal(a.Routes, b.Routes) &&
		slices.Equal(a.PendingRoutes, b.PendingRoutes)
}

// compile-time check that the real supervisor satisfies Daemon.
var _ Daemon = (*supervisor.Supervisor)(nil)
