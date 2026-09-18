package config_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/mkmik/minitail/internal/config"
)

// tailscaled's own defaults on macOS. Sharing any of them would put this
// daemon in conflict with a system Tailscale install, which is the failure
// this project is built to avoid.
const (
	systemSocket = "/var/run/tailscaled.socket"
	systemPort   = 41641
)

func TestDefaultsAreIsolated(t *testing.T) {
	t.Setenv("MINITAIL_STATE_DIR", "/tmp/minitail-test")
	cfg := config.Default()

	if cfg.Port == systemPort {
		t.Errorf("Port = %d, which collides with a system tailscaled", cfg.Port)
	}
	if cfg.TailscaledSocket == systemSocket {
		t.Errorf("TailscaledSocket = %q, which collides with a system tailscaled", cfg.TailscaledSocket)
	}
	if !strings.HasPrefix(cfg.TailscaledSocket, cfg.StateDir) {
		t.Errorf("TailscaledSocket = %q, want it inside the state dir %q", cfg.TailscaledSocket, cfg.StateDir)
	}
	if cfg.ControlSocket == cfg.TailscaledSocket {
		t.Error("minitail's control socket must not be tailscaled's socket")
	}
	if strings.HasPrefix(cfg.StateDir, "/Library") || strings.HasPrefix(cfg.StateDir, "/var") {
		t.Errorf("StateDir = %q, want a per-user path", cfg.StateDir)
	}
}

func TestStateDirHonoursXDG(t *testing.T) {
	t.Setenv("MINITAIL_STATE_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	if got, want := config.Default().StateDir, "/tmp/xdg/minitail"; got != want {
		t.Errorf("StateDir = %q, want %q", got, want)
	}
}

// TestTailscaledArgs pins the flags that make this a non-interfering exit
// node. If any of them is dropped, tailscaled starts creating a utun and
// installing routes again.
func TestTailscaledArgs(t *testing.T) {
	t.Setenv("MINITAIL_STATE_DIR", "/tmp/minitail-test")
	cfg := config.Default()
	args := cfg.TailscaledArgs()

	if !slices.Contains(args, "--tun=userspace-networking") {
		t.Errorf("args %v must contain --tun=userspace-networking", args)
	}
	for _, want := range []string{
		"--statedir=/tmp/minitail-test",
		"--socket=/tmp/minitail-test/tailscaled.sock",
		"--port=41642",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("args %v must contain %q", args, want)
		}
	}
	// Nothing should ask for a TUN device by name.
	for _, a := range args {
		if strings.HasPrefix(a, "--tun=") && a != "--tun=userspace-networking" {
			t.Errorf("unexpected tun flag %q", a)
		}
	}
}

func TestResolveRejectsMissingBinaries(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.TailscaledPath = "/nonexistent/tailscaled" // provided, so not looked up
	err := cfg.Resolve()
	if err == nil {
		t.Skip("a tailscale CLI exists in one of the fallback directories on this machine")
	}
	if !strings.Contains(err.Error(), "tailscale") {
		t.Errorf("error = %v, want it to name the missing binary", err)
	}
}

func TestResolveFillsDerivedPaths(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateDir = dir
	cfg.TailscaledSocket = ""
	cfg.ControlSocket = ""
	cfg.TailscaledPath = "/bin/true"
	cfg.TailscalePath = "/bin/true"
	if err := cfg.Resolve(); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.TailscaledSocket != dir+"/tailscaled.sock" {
		t.Errorf("TailscaledSocket = %q", cfg.TailscaledSocket)
	}
	if cfg.ControlSocket != dir+"/minitail.sock" {
		t.Errorf("ControlSocket = %q", cfg.ControlSocket)
	}
	if err := cfg.EnsureStateDir(); err != nil {
		t.Fatalf("EnsureStateDir: %v", err)
	}
}
