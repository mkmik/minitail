package app

import (
	"cmp"
	"context"
	"fmt"
	"net/netip"
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
	SetArgs(args []string)
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

	// cmdMu serializes Start, Stop and Reload, which the menu and the control
	// socket can call at the same time.
	cmdMu sync.Mutex

	mu sync.Mutex
	// file is the config file as last read, which Reload replaces;
	// opts.Config.File is only the one minitail started with. advertised is
	// what it asks to advertise.
	file          config.File
	advertised    []netip.Prefix
	advertisedErr error

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
	// preferences have already been re-applied.
	appliedFor int
	// applyErr is the last failure to apply the config file, by `tailscale
	// set` or `tailscale up`, kept so it can be shown rather than only logged.
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
		file:       opts.Config.File,
		subs:       map[int]chan View{},
		appliedFor: -1,
		poke:       make(chan struct{}, 1),
	}
	c.advertised, c.advertisedErr = c.file.AdvertisedRoutes()
	if c.advertisedErr != nil {
		// Said once here; the view keeps showing it.
		opts.Logf("reading advertised routes from the config file: %v", c.advertisedErr)
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
// immediately, which is what the LaunchAgent wants, and shuts it down before
// returning.
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
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()
	c.mu.Lock()
	already := c.want
	c.want = true
	c.appliedFor = -1
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
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()
	c.mu.Lock()
	already := !c.want
	c.want = false
	c.status, c.statusErr, c.applyErr = nil, nil, nil
	c.mu.Unlock()
	if !already {
		c.opts.Daemon.Stop()
	}
	c.refresh()
}

// Reload rereads the config file and applies it without restarting minitail.
// A file minitail cannot parse is refused and the node left as it was. The
// [up] flags are reapplied by the polling loop, as after Start. tailscaled is
// restarted only if its own flags changed, because that drops every
// connection routed through it.
func (c *Controller) Reload(ctx context.Context) error {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()

	cfg := c.opts.Config
	if _, err := cfg.Load(); err != nil {
		return err
	}
	advertised, err := cfg.File.AdvertisedRoutes()
	if err != nil {
		return fmt.Errorf("%s: %w", cfg.Path, err)
	}

	c.mu.Lock()
	restart := c.want && !slices.Equal(c.file.Tailscaled, cfg.File.Tailscaled)
	c.file, c.advertised, c.advertisedErr = cfg.File, advertised, nil
	c.appliedFor, c.applyErr = -1, nil
	c.mu.Unlock()

	c.opts.Logf("reloaded %s", cfg.Path)
	c.opts.Daemon.SetArgs(cfg.TailscaledArgs())
	if restart {
		c.opts.Logf("restarting tailscaled %s", strings.Join(cfg.TailscaledArgs(), " "))
		c.opts.Daemon.Stop()
		c.opts.Daemon.Start(ctx)
	}
	c.refresh()
	c.Poke()
	return nil
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
	c.applyErr = nil
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
		if err != nil {
			// Drop the previous answer too: a daemon that has stopped
			// answering must not keep showing as serving.
			st = nil
		}
		c.mu.Lock()
		c.status, c.statusErr = st, err
		c.mu.Unlock()
	}

	prev := c.View()
	cur := c.refresh()
	c.react(ctx, prev, cur)
}

// input snapshots everything Derive needs. Caller must not hold c.mu.
func (c *Controller) input() Input {
	c.mu.Lock()
	in := Input{
		WantRunning: c.want,
		Status:      c.status,
		StatusErr:   c.statusErr,
		// A file minitail cannot read the routes out of is shown in the
		// same place as one tailscaled rejected.
		ApplyErr:   cmp.Or(c.advertisedErr, c.applyErr),
		Advertised: c.advertised,
	}
	c.mu.Unlock()

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
	defer c.mu.Unlock()
	if sameView(c.view, v) {
		return v
	}
	c.view = v
	// Delivered under the lock, so a subscriber cannot close its channel
	// between this send and its unsubscribe. The send never blocks.
	for _, ch := range c.subs {
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

	case StateBadConfig:
		c.applyPrefs(ctx) // retried every poll, so a transient failure heals
		if prev.State != StateBadConfig {
			c.notify(cur.Title(), cur.Detail)
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
	upArgs := c.file.Up
	c.mu.Unlock()

	go func() {
		err := c.opts.Tailscale.Up(ctx, upArgs)
		c.mu.Lock()
		c.upInFlight = false
		changed := errDetail("", err) != errDetail("", c.applyErr)
		if ctx.Err() == nil {
			c.applyErr = err
		}
		c.mu.Unlock()
		switch {
		case ctx.Err() != nil: // shutting down
		case err == nil:
			c.Poke() // show the logged-in state without waiting for the tick
		case changed:
			// Left to the next tick to retry: poking here would run `up` in
			// a tight loop for as long as the file keeps a flag it rejects.
			// Logged once per distinct error for the same reason.
			c.opts.Logf("tailscale up: %v", err)
		}
	}()
}

// applyPrefs re-applies the configured preferences once per tailscaled
// generation. tailscaled persists preferences, so a node logged in before the
// config file was edited would otherwise come back advertising the old routes.
//
// A failure is shown in the view and retried on the next poll, which is what
// lets a transient one (the daemon not answering yet) heal by itself; it is
// logged only when it changes, so a flag the daemon rejects does not fill the
// log until the file is fixed.
func (c *Controller) applyPrefs(ctx context.Context) {
	restarts, _, _ := c.opts.Daemon.Stats()
	c.mu.Lock()
	if c.appliedFor == restarts {
		c.mu.Unlock()
		return
	}
	c.appliedFor = restarts
	upArgs := c.file.Up
	c.mu.Unlock()

	err := c.opts.Tailscale.Apply(ctx, upArgs)

	c.mu.Lock()
	if ctx.Err() != nil || !c.want || c.appliedFor != restarts {
		// Shutting down, or Stop/Start/Reload ran meanwhile: the result
		// belongs to a generation that is over.
		c.mu.Unlock()
		return
	}
	changed := errDetail("", err) != errDetail("", c.applyErr)
	c.applyErr = err
	if err != nil {
		c.appliedFor = -1 // retry on the next poll
	}
	c.mu.Unlock()
	if err != nil && changed {
		c.opts.Logf("applying %s: %v", c.opts.Config.Path, err)
	}
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
