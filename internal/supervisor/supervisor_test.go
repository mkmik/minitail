package supervisor_test

import (
	"context"
	"testing"
	"time"

	"github.com/mkmik/minitail/internal/supervisor"
)

func TestBackoffDelay(t *testing.T) {
	b := supervisor.Backoff{Min: time.Second, Max: 30 * time.Second, Factor: 2}
	want := []time.Duration{
		time.Second,      // first failure
		time.Second,      // 1 -> same as first
		2 * time.Second,  //
		4 * time.Second,  //
		8 * time.Second,  //
		16 * time.Second, //
		30 * time.Second, // capped
		30 * time.Second,
	}
	for i, w := range want {
		if got := b.Delay(i); got != w {
			t.Errorf("Delay(%d) = %v, want %v", i, got, w)
		}
	}
}

func TestBackoffGiveUp(t *testing.T) {
	never := supervisor.Backoff{Min: time.Second, Max: time.Second, Factor: 2}
	if never.ShouldGiveUp(100) {
		t.Error("GiveUpAfter=0 must never give up")
	}
	b := supervisor.Backoff{Min: time.Second, Max: time.Second, Factor: 2, GiveUpAfter: 3}
	if b.ShouldGiveUp(2) {
		t.Error("should not give up before the limit")
	}
	if !b.ShouldGiveUp(3) {
		t.Error("should give up at the limit")
	}
}

// fastBackoff keeps the process-level tests quick.
var fastBackoff = supervisor.Backoff{
	Min:         5 * time.Millisecond,
	Max:         20 * time.Millisecond,
	Factor:      2,
	StableAfter: time.Hour, // never reset, so failure counts are predictable
}

func TestSupervisorRestartsOnExit(t *testing.T) {
	s := supervisor.New(supervisor.Options{
		Path:    "/bin/sh",
		Args:    []string{"-c", "exit 1"},
		Backoff: fastBackoff,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	defer s.Stop()

	waitFor(t, "at least three restarts", func() bool {
		restarts, _, _ := s.Stats()
		return restarts >= 3
	})
}

func TestSupervisorGivesUp(t *testing.T) {
	b := fastBackoff
	b.GiveUpAfter = 2
	s := supervisor.New(supervisor.Options{
		Path:    "/bin/sh",
		Args:    []string{"-c", "exit 3"},
		Backoff: b,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	defer s.Stop()

	waitFor(t, "the supervisor to give up", func() bool {
		_, givenUp, _ := s.Stats()
		return givenUp
	})
	restarts, _, lastErr := s.Stats()
	if restarts != 2 {
		t.Errorf("restarts = %d, want 2", restarts)
	}
	if lastErr == nil {
		t.Error("expected the exit error to be recorded")
	}
	if s.Running() {
		t.Error("nothing should be running after giving up")
	}
}

// TestSupervisorRestartsAfterKill is the supervision property the menu bar
// depends on: tailscaled dying must not leave the node down.
func TestSupervisorRestartsAfterKill(t *testing.T) {
	s := supervisor.New(supervisor.Options{
		Path:    "/bin/sh",
		Args:    []string{"-c", "sleep 30"},
		Backoff: fastBackoff,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	defer s.Stop()

	waitFor(t, "the first start", s.Running)
	if err := s.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	waitFor(t, "a restart after the kill", func() bool {
		restarts, _, _ := s.Stats()
		return restarts >= 1 && s.Running()
	})
}

func TestSupervisorStopIsFinal(t *testing.T) {
	s := supervisor.New(supervisor.Options{
		Path:    "/bin/sh",
		Args:    []string{"-c", "sleep 30"},
		Backoff: fastBackoff,
	})
	s.Start(context.Background())
	waitFor(t, "the first start", s.Running)

	s.Stop()
	if s.Running() {
		t.Fatal("still running after Stop")
	}
	restarts, _, _ := s.Stats()
	time.Sleep(50 * time.Millisecond)
	if after, _, _ := s.Stats(); after != restarts {
		t.Errorf("restarts went from %d to %d after Stop; it must stay stopped", restarts, after)
	}
}

func TestSupervisorStopWithoutStart(t *testing.T) {
	supervisor.New(supervisor.Options{Path: "/bin/true"}).Stop()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
