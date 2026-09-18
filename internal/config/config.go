// Package config holds the paths and flags that keep this daemon isolated
// from any system-wide Tailscale installation.
package config

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// DefaultPort is the UDP port tailscaled listens on for WireGuard traffic.
// It is deliberately not tailscaled's own default (41641) so that this daemon
// can run alongside a system Tailscale install without colliding.
const DefaultPort = 41642

// Config is the fully resolved runtime configuration.
type Config struct {
	// StateDir is the tailscaled --statedir. Note this is a directory, not a
	// state file; tailscaled stores tailscaled.state and its certs inside it.
	StateDir string

	// TailscaledSocket is the tailscaled --socket path. Every `tailscale` CLI
	// invocation must be pointed at it explicitly.
	TailscaledSocket string

	// ControlSocket is minitail's own status socket, used by `minitail status`
	// to query a running supervisor.
	ControlSocket string

	// Port is the tailscaled --port UDP port.
	Port int

	// Hostname is the name this node registers under.
	Hostname string

	// TailscaledPath and TailscalePath locate the open source Tailscale
	// binaries (Homebrew's `tailscale` formula ships both).
	TailscaledPath string
	TailscalePath  string

	// ControlURL, when set, overrides the Tailscale coordination server.
	// Used by the integration tests to point at Headscale.
	ControlURL string

	// AuthKey, when set, makes the first login non-interactive.
	AuthKey string

	// AdvertiseRoutes are extra subnets to advertise as a subnet router,
	// beyond the exit node advertisement. Empty by default.
	//
	// This exists because a Tailscale exit node deliberately refuses to
	// forward traffic to subnets that are configured directly on one of its
	// own interfaces (the "guest wifi" rule in ipnlocal.shrinkDefaultRoute):
	// peers get internet access, not LAN access. Networks reached through a
	// gateway are unaffected. Container runtimes such as OrbStack attach
	// their bridges directly to the host, so reaching those container IPs
	// through this node requires naming their subnets here.
	AdvertiseRoutes string

	// PollInterval is how often the supervisor polls `tailscale status`.
	PollInterval time.Duration

	// Verbose enables tailscaled's verbose logging.
	Verbose bool
}

// Default returns a Config with the user-settable fields filled in. The socket
// paths and the binary locations are derived by Resolve, which every caller
// runs after flag parsing.
func Default() Config {
	host, _ := os.Hostname()
	if host == "" {
		host = "mac"
	}
	return Config{
		StateDir:     defaultStateDir(),
		Port:         DefaultPort,
		Hostname:     host + "-exit",
		PollInterval: 2 * time.Second,
	}
}

func defaultStateDir() string {
	if d := os.Getenv("MINITAIL_STATE_DIR"); d != "" {
		return d
	}
	// The PRD calls for ~/.config/minitail rather than macOS's
	// ~/Library/Application Support, so this does not use os.UserConfigDir.
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(os.TempDir(), "minitail")
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "minitail")
}

// RegisterFlags binds the user-settable subset of Config to fs.
func (c *Config) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.StateDir, "state-dir", c.StateDir, "directory for this node's isolated tailscaled state")
	fs.StringVar(&c.TailscaledSocket, "socket", "", "tailscaled LocalAPI socket path (default <state-dir>/tailscaled.sock)")
	fs.IntVar(&c.Port, "port", c.Port, "UDP port for tailscaled WireGuard traffic")
	fs.StringVar(&c.Hostname, "hostname", c.Hostname, "hostname to register in the tailnet")
	fs.StringVar(&c.TailscaledPath, "tailscaled", "", "path to the tailscaled binary (default: found on PATH)")
	fs.StringVar(&c.TailscalePath, "tailscale", "", "path to the tailscale CLI binary (default: found on PATH)")
	fs.StringVar(&c.ControlURL, "control-url", "", "coordination server URL (default: Tailscale's)")
	fs.StringVar(&c.AuthKey, "auth-key", "", "pre-authentication key for non-interactive login")
	fs.StringVar(&c.AdvertiseRoutes, "advertise-routes", c.AdvertiseRoutes,
		"comma-separated subnets to also advertise as a subnet router, for LANs attached directly to this machine (e.g. OrbStack bridges) that an exit node would otherwise not forward to")
	fs.DurationVar(&c.PollInterval, "poll-interval", c.PollInterval, "how often to poll tailscaled for status")
	fs.BoolVar(&c.Verbose, "verbose", c.Verbose, "enable verbose tailscaled logging")
}

// extraBinDirs are searched after PATH. A LaunchAgent starts with a minimal
// PATH that does not include either Homebrew prefix.
var extraBinDirs = []string{
	"/opt/homebrew/bin", // Homebrew on Apple silicon
	"/usr/local/bin",    // Homebrew on Intel
	"/usr/bin",
	"/usr/sbin",
}

// Resolve fills in defaults that depend on other fields, looks up the
// Tailscale binaries, and validates the result.
//
// The paths are derived before the binaries are looked up, so a caller that
// only needs the socket paths (`minitail status`) can use them even when the
// lookup fails because Tailscale is not installed.
func (c *Config) Resolve() error {
	if c.StateDir == "" {
		return fmt.Errorf("state dir must not be empty")
	}
	abs, err := filepath.Abs(c.StateDir)
	if err != nil {
		return fmt.Errorf("resolving state dir: %w", err)
	}
	c.StateDir = abs
	if c.TailscaledSocket == "" {
		c.TailscaledSocket = filepath.Join(c.StateDir, "tailscaled.sock")
	}
	// Always derived from the state dir, never carried over: -state-dir has to
	// move minitail's own control socket too, or `minitail status
	// -state-dir=X` would query the default instance rather than X's.
	c.ControlSocket = filepath.Join(c.StateDir, "minitail.sock")
	if c.AuthKey == "" {
		c.AuthKey = os.Getenv("MINITAIL_AUTH_KEY")
	}
	if c.ControlURL == "" {
		c.ControlURL = os.Getenv("MINITAIL_CONTROL_URL")
	}
	if c.AdvertiseRoutes == "" {
		c.AdvertiseRoutes = os.Getenv("MINITAIL_ADVERTISE_ROUTES")
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.TailscaledPath == "" {
		if c.TailscaledPath, err = lookPath("tailscaled"); err != nil {
			return err
		}
	}
	if c.TailscalePath == "" {
		if c.TailscalePath, err = lookPath("tailscale"); err != nil {
			return err
		}
	}
	return nil
}

// EnsureStateDir creates the state directory with owner-only permissions.
// tailscaled stores a private node key there.
func (c *Config) EnsureStateDir() error {
	return os.MkdirAll(c.StateDir, 0o700)
}

func lookPath(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	for _, dir := range extraBinDirs {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%q not found on PATH or in %v; install it with `brew install tailscale`", name, extraBinDirs)
}

// TailscaledArgs returns the argv for the supervised tailscaled process.
//
// --tun=userspace-networking is the whole point of this project: in that mode
// tailscaled implements its network stack in-process with gVisor netstack and
// never asks the OS for a TUN device, a route, or a DNS change.
func (c *Config) TailscaledArgs() []string {
	args := []string{
		"--tun=userspace-networking",
		"--statedir=" + c.StateDir,
		"--socket=" + c.TailscaledSocket,
		fmt.Sprintf("--port=%d", c.Port),
		// No SOCKS5/HTTP proxy: egress is only for tailnet peers using this
		// node as their exit node.
		"--socks5-server=",
		"--outbound-http-proxy-listen=",
	}
	if c.Verbose {
		args = append(args, "--verbose=1")
	}
	return args
}
