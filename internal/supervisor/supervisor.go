// Package supervisor keeps a child process alive with exponential backoff.
package supervisor

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"
	"time"
)

// Backoff decides how long to wait before restarting a process that exited.
// It is a value type with no state so it can be unit tested directly.
type Backoff struct {
	// Min is the delay after the first failure.
	Min time.Duration
	// Max caps the delay.
	Max time.Duration
	// Factor multiplies the delay after each consecutive failure.
	Factor int
	// GiveUpAfter is the number of consecutive failures after which the
	// supervisor stops trying. Zero means never give up.
	GiveUpAfter int
	// StableAfter is how long a process must run before its start counts as
	// successful and the failure streak resets.
	StableAfter time.Duration
}

// DefaultBackoff is tuned for tailscaled: quick first retry, capped at 30s,
// and never permanently giving up, because the cause is usually transient
// (a laptop waking from sleep, a socket left behind by a previous run).
var DefaultBackoff = Backoff{
	Min:         time.Second,
	Max:         30 * time.Second,
	Factor:      2,
	GiveUpAfter: 0,
	StableAfter: time.Minute,
}

// Delay returns how long to wait after the given number of consecutive
// failures. failures is 1 for the first failure.
func (b Backoff) Delay(failures int) time.Duration {
	if failures <= 1 {
		return b.Min
	}
	factor := b.Factor
	if factor < 1 {
		factor = 1
	}
	d := b.Min
	for i := 1; i < failures; i++ {
		d *= time.Duration(factor)
		if d >= b.Max {
			return b.Max
		}
	}
	return d
}

// ShouldGiveUp reports whether the supervisor should stop restarting.
func (b Backoff) ShouldGiveUp(failures int) bool {
	return b.GiveUpAfter > 0 && failures >= b.GiveUpAfter
}

// Options configures a Supervisor.
type Options struct {
	// Path and Args are the command to run. Args excludes argv[0].
	Path string
	Args []string
	// Env, when non-nil, replaces the child's environment.
	Env []string
	// Stdout and Stderr receive the child's output.
	Stdout, Stderr io.Writer
	// Backoff controls restart timing. The zero value means DefaultBackoff.
	Backoff Backoff
	// Logf receives supervision events.
	Logf func(format string, args ...any)
	// OnChange is called whenever the running state changes. It must not block.
	OnChange func()
}

// Supervisor runs one child process and restarts it if it exits.
type Supervisor struct {
	opts Options

	mu       sync.Mutex
	cmd      *exec.Cmd
	running  bool
	restarts int
	failures int
	givenUp  bool
	lastErr  error

	stop   context.CancelFunc
	doneCh chan struct{}
}

// New returns a Supervisor that is not yet running.
func New(opts Options) *Supervisor {
	if opts.Backoff == (Backoff{}) {
		opts.Backoff = DefaultBackoff
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if opts.OnChange == nil {
		opts.OnChange = func() {}
	}
	return &Supervisor{opts: opts}
}

// Start launches the child and keeps it running until Stop is called or ctx is
// cancelled. Calling Start on an already-started Supervisor is a no-op.
func (s *Supervisor) Start(ctx context.Context) {
	s.mu.Lock()
	if s.doneCh != nil {
		s.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.stop, s.doneCh = cancel, done
	s.failures, s.givenUp, s.lastErr = 0, false, nil
	s.mu.Unlock()

	go s.supervise(runCtx, done)
}

// Stop terminates the child and waits for the supervision loop to exit.
// It is safe to call on a Supervisor that was never started.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	cancel, done := s.stop, s.doneCh
	s.stop, s.doneCh = nil, nil
	s.mu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// Running reports whether the child process is currently alive.
func (s *Supervisor) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Stats reports supervision counters for display and for tests.
func (s *Supervisor) Stats() (restarts int, givenUp bool, lastErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restarts, s.givenUp, s.lastErr
}

// Kill sends SIGKILL to the current child without stopping supervision, so the
// supervisor restarts it. It exists for the integration tests.
func (s *Supervisor) Kill() error {
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return errors.New("supervisor: no running process")
	}
	return cmd.Process.Kill()
}

func (s *Supervisor) supervise(ctx context.Context, done chan struct{}) {
	defer close(done)

	for {
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		err := s.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}

		ran := time.Since(started)
		s.mu.Lock()
		s.restarts++
		if ran >= s.opts.Backoff.StableAfter {
			// The process ran long enough to count as a successful start, so
			// this exit begins a fresh failure streak rather than continuing
			// a previous one.
			s.failures = 0
		}
		s.failures++
		failures := s.failures
		s.lastErr = err
		giveUp := s.opts.Backoff.ShouldGiveUp(failures)
		s.givenUp = giveUp
		s.mu.Unlock()

		s.opts.Logf("%s exited after %v (failure %d): %v", s.opts.Path, ran.Round(time.Millisecond), failures, err)
		s.opts.OnChange()
		if giveUp {
			s.opts.Logf("giving up after %d consecutive failures", failures)
			return
		}

		delay := s.opts.Backoff.Delay(failures)
		s.opts.Logf("restarting in %v", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (s *Supervisor) runOnce(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, s.opts.Path, s.opts.Args...)
	cmd.Stdout = s.opts.Stdout
	cmd.Stderr = s.opts.Stderr
	if s.opts.Env != nil {
		cmd.Env = s.opts.Env
	}
	// Give the child a chance to shut down cleanly when ctx is cancelled;
	// exec.CommandContext kills it outright otherwise.
	cmd.Cancel = func() error { return cmd.Process.Signal(interruptSignal) }
	cmd.WaitDelay = 5 * time.Second

	if err := cmd.Start(); err != nil {
		return err
	}
	s.mu.Lock()
	s.cmd, s.running = cmd, true
	s.mu.Unlock()
	s.opts.OnChange()

	err := cmd.Wait()

	s.mu.Lock()
	s.cmd, s.running = nil, false
	s.mu.Unlock()
	s.opts.OnChange()
	return err
}
