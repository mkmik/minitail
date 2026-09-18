// Package config locates minitail's per-user directory and reads the config
// file that holds the flags it passes to tailscaled and to `tailscale up`.
//
// minitail deliberately has no UI for Tailscale's own options: the config file
// is the interface. The only flags minitail supplies itself are --statedir and
// --socket, which are what keep this instance isolated from any system-wide
// Tailscale install.
package config

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// FileName is the config file's name inside the minitail directory.
const FileName = "minitail.conf"

// Config is the fully resolved runtime configuration.
type Config struct {
	// Dir is minitail's per-user directory: the config file, the control
	// socket, tailscaled's state and its log all live here.
	Dir string

	// Path is the config file.
	Path string

	// StateDir is tailscaled's --statedir. Managed by minitail.
	StateDir string

	// Socket is tailscaled's --socket, and the one the tailscale CLI is
	// pointed at. Managed by minitail.
	Socket string

	// ControlSocket is minitail's own status socket, used by
	// `minitail status`.
	ControlSocket string

	// TailscaledLog receives the supervised daemon's output.
	TailscaledLog string

	// TailscaledPath and TailscalePath locate the open source Tailscale
	// binaries (Homebrew's `tailscale` formula ships both).
	TailscaledPath string
	TailscalePath  string

	// PollInterval is how often the supervisor polls `tailscale status`.
	PollInterval time.Duration

	// File holds what was read from the config file.
	File File
}

// Default returns a Config with the directory resolved but nothing loaded.
func Default() Config {
	dir := defaultDir()
	return Config{
		Dir:           dir,
		Path:          filepath.Join(dir, FileName),
		StateDir:      filepath.Join(dir, "tailscaled"),
		Socket:        filepath.Join(dir, "tailscaled.sock"),
		ControlSocket: filepath.Join(dir, "minitail.sock"),
		TailscaledLog: filepath.Join(dir, "tailscaled.log"),
		PollInterval:  2 * time.Second,
	}
}

func defaultDir() string {
	if d := os.Getenv("MINITAIL_DIR"); d != "" {
		return d
	}
	// ~/.config rather than macOS's ~/Library/Application Support, so the
	// config file is somewhere a person editing dotfiles expects it.
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

// RegisterFlags binds minitail's own options to fs. Tailscale's options live
// in the config file, not here.
func (c *Config) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.Dir, "dir", c.Dir, "minitail's directory: config file, control socket, tailscaled state and log")
	fs.StringVar(&c.TailscaledPath, "tailscaled", "", "path to the tailscaled binary (default: found on PATH)")
	fs.StringVar(&c.TailscalePath, "tailscale", "", "path to the tailscale CLI binary (default: found on PATH)")
	fs.DurationVar(&c.PollInterval, "poll-interval", c.PollInterval, "how often to poll tailscaled for status")
}

// extraBinDirs are searched after PATH. A LaunchAgent starts with a minimal
// PATH that does not include either Homebrew prefix.
var extraBinDirs = []string{
	"/opt/homebrew/bin", // Homebrew on Apple silicon
	"/usr/local/bin",    // Homebrew on Intel
	"/usr/bin",
	"/usr/sbin",
}

// Resolve derives the paths that depend on Dir and looks up the Tailscale
// binaries. It does not read the config file; call Load for that.
//
// The paths are derived before the binaries are looked up, so a caller that
// only needs the socket paths (`minitail status`) can use them even when the
// lookup fails because Tailscale is not installed.
func (c *Config) Resolve() error {
	if c.Dir == "" {
		return fmt.Errorf("minitail directory must not be empty")
	}
	abs, err := filepath.Abs(c.Dir)
	if err != nil {
		return fmt.Errorf("resolving minitail directory: %w", err)
	}
	c.Dir = abs
	c.Path = filepath.Join(c.Dir, FileName)
	c.StateDir = filepath.Join(c.Dir, "tailscaled")
	c.Socket = filepath.Join(c.Dir, "tailscaled.sock")
	c.ControlSocket = filepath.Join(c.Dir, "minitail.sock")
	c.TailscaledLog = filepath.Join(c.Dir, "tailscaled.log")
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

// EnsureDirs creates minitail's directory and tailscaled's state directory
// with owner-only permissions. tailscaled stores a private node key there.
func (c *Config) EnsureDirs() error {
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return err
	}
	return os.MkdirAll(c.StateDir, 0o700)
}

// Load reads the config file, seeding it with the default first if it does not
// exist. It reports whether it created the file.
func (c *Config) Load() (seeded bool, err error) {
	if err := c.EnsureDirs(); err != nil {
		return false, err
	}
	data, err := os.ReadFile(c.Path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		if err := os.WriteFile(c.Path, []byte(DefaultFile(hostname())), 0o600); err != nil {
			return false, fmt.Errorf("seeding %s: %w", c.Path, err)
		}
		seeded = true
		if data, err = os.ReadFile(c.Path); err != nil {
			return seeded, err
		}
	default:
		return false, err
	}

	f, err := ParseFile(string(data))
	if err != nil {
		return seeded, fmt.Errorf("%s: %w", c.Path, err)
	}
	c.File = f
	return seeded, nil
}

// TailscaledArgs returns the argv for the supervised tailscaled process: the
// two flags minitail manages, followed by whatever the config file asks for.
func (c *Config) TailscaledArgs() []string {
	args := []string{
		"--statedir=" + c.StateDir,
		"--socket=" + c.Socket,
	}
	return append(args, c.File.Tailscaled...)
}

// TailscaleArgs returns the argv for a `tailscale` CLI invocation, pointed at
// this instance's socket.
func (c *Config) TailscaleArgs(rest ...string) []string {
	return append([]string{"--socket=" + c.Socket}, rest...)
}

// UpArgs returns the flags for `tailscale up` and `tailscale set`.
func (c *Config) UpArgs() []string { return c.File.Up }

func hostname() string {
	h, _ := os.Hostname()
	if h == "" {
		return "mac"
	}
	// macOS hostnames are often "Name's MacBook Pro.local"; keep the first
	// label and make it safe for a tailnet machine name.
	h, _, _ = strings.Cut(h, ".")
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
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
