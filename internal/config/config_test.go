package config_test

import (
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mkmik/minitail/internal/config"
)

// tailscaled's own defaults on macOS. Sharing either would put this instance
// in conflict with a system Tailscale install, which is the failure minitail
// exists to avoid.
const (
	systemSocket = "/var/run/tailscaled.socket"
	systemPort   = "41641"
)

func loaded(t *testing.T, contents string) config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Dir = dir
	cfg.TailscaledPath, cfg.TailscalePath = "/bin/true", "/bin/true"
	if err := cfg.Resolve(); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if contents != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfg.Path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cfg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestPathsAreIsolated(t *testing.T) {
	cfg := loaded(t, "")

	if cfg.Socket == systemSocket {
		t.Errorf("Socket = %q, which collides with a system tailscaled", cfg.Socket)
	}
	for _, p := range []string{cfg.Socket, cfg.StateDir, cfg.ControlSocket, cfg.Path, cfg.TailscaledLog} {
		if !strings.HasPrefix(p, cfg.Dir) {
			t.Errorf("%q is outside minitail's directory %q", p, cfg.Dir)
		}
	}
	if cfg.ControlSocket == cfg.Socket {
		t.Error("minitail's control socket must not be tailscaled's socket")
	}
	// The seeded config must not pick tailscaled's default port either.
	if slices.Contains(cfg.TailscaledArgs(), "--port="+systemPort) {
		t.Errorf("the seeded config uses tailscaled's default port %s", systemPort)
	}
}

func TestDirHonoursEnvAndXDG(t *testing.T) {
	t.Setenv("MINITAIL_DIR", "/tmp/explicit")
	if got := config.Default().Dir; got != "/tmp/explicit" {
		t.Errorf("Dir = %q, want the MINITAIL_DIR override", got)
	}
	t.Setenv("MINITAIL_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	if got, want := config.Default().Dir, "/tmp/xdg/minitail"; got != want {
		t.Errorf("Dir = %q, want %q", got, want)
	}
}

// TestSeededConfig pins what a fresh install gets: userspace networking and a
// placeholder subnet route.
func TestSeededConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Dir = dir
	cfg.TailscaledPath, cfg.TailscalePath = "/bin/true", "/bin/true"
	if err := cfg.Resolve(); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	seeded, err := cfg.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !seeded {
		t.Fatal("Load did not report seeding a fresh directory")
	}
	if _, err := os.Stat(cfg.Path); err != nil {
		t.Fatalf("the config file was not written: %v", err)
	}

	args := cfg.TailscaledArgs()
	if !slices.Contains(args, "--tun=userspace-networking") {
		t.Errorf("tailscaled args %v must contain --tun=userspace-networking", args)
	}
	// The two flags minitail manages are always present, whatever the file says.
	for _, want := range []string{"--statedir=" + cfg.StateDir, "--socket=" + cfg.Socket} {
		if !slices.Contains(args, want) {
			t.Errorf("tailscaled args %v must contain %q", args, want)
		}
	}

	routes, err := cfg.File.AdvertisedRoutes()
	if err != nil {
		t.Fatalf("AdvertisedRoutes: %v", err)
	}
	if want := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}; !slices.Equal(routes, want) {
		t.Errorf("seeded routes = %v, want the %v placeholder", routes, want)
	}
	for _, want := range []string{"--accept-dns=false", "--accept-routes=false"} {
		if !slices.Contains(cfg.UpArgs(), want) {
			t.Errorf("up args %v must contain %q", cfg.UpArgs(), want)
		}
	}

	// A second Load must not overwrite an edited file.
	if err := os.WriteFile(cfg.Path, []byte("[up]\n--advertise-routes=192.168.0.0/24\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if seeded, err = cfg.Load(); err != nil || seeded {
		t.Fatalf("second Load: seeded=%v err=%v, want false/nil", seeded, err)
	}
	routes, _ = cfg.File.AdvertisedRoutes()
	if want := []netip.Prefix{netip.MustParsePrefix("192.168.0.0/24")}; !slices.Equal(routes, want) {
		t.Errorf("routes after editing = %v, want %v", routes, want)
	}
}

func TestSeededConfigParsesBackCleanly(t *testing.T) {
	if _, err := config.ParseFile(config.DefaultFile("host")); err != nil {
		t.Fatalf("the seeded config does not parse: %v", err)
	}
	if !strings.Contains(config.DefaultFile("host"), "--hostname=host-minitail") {
		t.Error("the seeded config should name the node after the machine")
	}
}

func TestResolveRejectsMissingBinaries(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cfg := config.Default()
	cfg.Dir = t.TempDir()
	cfg.TailscaledPath = "/nonexistent/tailscaled" // provided, so not looked up
	err := cfg.Resolve()
	if err == nil {
		t.Skip("a tailscale CLI exists in one of the fallback directories on this machine")
	}
	if !strings.Contains(err.Error(), "tailscale") {
		t.Errorf("error = %v, want it to name the missing binary", err)
	}
}

// TestResolveMovesEverythingWithDir guards `minitail status -dir=X`: it must
// talk to X's instance, not the default one.
func TestResolveMovesEverythingWithDir(t *testing.T) {
	t.Setenv("MINITAIL_DIR", "/tmp/minitail-default")
	other := t.TempDir()

	cfg := config.Default()
	cfg.Dir = other
	cfg.TailscaledPath, cfg.TailscalePath = "/bin/true", "/bin/true"
	if err := cfg.Resolve(); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, tc := range []struct{ got, want string }{
		{cfg.Path, filepath.Join(other, config.FileName)},
		{cfg.ControlSocket, filepath.Join(other, "minitail.sock")},
		{cfg.Socket, filepath.Join(other, "tailscaled.sock")},
		{cfg.StateDir, filepath.Join(other, "tailscaled")},
	} {
		if tc.got != tc.want {
			t.Errorf("path = %q, want %q", tc.got, tc.want)
		}
	}
}

func TestTailscaleArgsPointAtOurSocket(t *testing.T) {
	cfg := loaded(t, "")
	args := cfg.TailscaleArgs("status", "--json")
	if want := "--socket=" + cfg.Socket; args[0] != want {
		t.Errorf("args[0] = %q, want %q", args[0], want)
	}
	if !slices.Equal(args[1:], []string{"status", "--json"}) {
		t.Errorf("args = %v, want the subcommand preserved", args)
	}
}
