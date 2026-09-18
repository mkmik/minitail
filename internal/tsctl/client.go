package tsctl

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Client is the operations minitail performs against its tailscaled instance.
// It is an interface so the controller can be unit tested without a daemon.
type Client interface {
	// Status returns the current backend status.
	Status(ctx context.Context) (*Status, error)
	// Up brings the node up, blocking until login completes. While it blocks,
	// Status reports NeedsLogin together with an AuthURL.
	Up(ctx context.Context) error
	// Apply re-applies the configured preferences to an already logged-in
	// node, so an edited config file takes effect without a re-login.
	Apply(ctx context.Context) error
	// Logout removes the node from the tailnet and clears local state.
	Logout(ctx context.Context) error
}

// Options configures a CLI-backed Client.
type Options struct {
	// Binary is the path to the `tailscale` CLI.
	Binary string
	// GlobalArgs precede the subcommand; minitail uses them to point the CLI
	// at this instance's socket.
	GlobalArgs []string
	// UpArgs are the flags from the config file's [up] section.
	UpArgs []string
	// Timeout bounds short-lived commands such as `status`.
	Timeout time.Duration
	// Logf receives command-level diagnostics.
	Logf func(format string, args ...any)
}

// CLI is a Client backed by the `tailscale` command line tool.
type CLI struct {
	opts Options
}

var _ Client = (*CLI)(nil)

// New returns a Client that shells out to the tailscale CLI.
func New(opts Options) *CLI {
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	return &CLI{opts: opts}
}

func (c *CLI) args(rest ...string) []string {
	return append(append([]string{}, c.opts.GlobalArgs...), rest...)
}

// Status runs `tailscale status --json`.
func (c *CLI) Status(ctx context.Context) (*Status, error) {
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	// --peers=false keeps the response small; minitail only looks at self.
	out, err := c.run(ctx, c.args("status", "--json", "--peers=false")...)
	if err != nil {
		return nil, err
	}
	return ParseStatus(out)
}

// Up runs `tailscale up`. It blocks until the node is authenticated, which for
// an interactive login means until the user visits the auth URL.
func (c *CLI) Up(ctx context.Context) error {
	// No timeout: an interactive login legitimately takes as long as the user
	// takes. The caller cancels ctx to give up.
	_, err := c.run(ctx, c.args(append([]string{"up"}, c.opts.UpArgs...)...)...)
	return err
}

// Apply runs `tailscale set` with the same flags, which is how an edited
// config file reaches a node that is already logged in.
func (c *CLI) Apply(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	// `tailscale set` rejects flags that only `up` understands, so anything
	// it cannot take is dropped here rather than failing the whole call.
	args := setArgs(c.opts.UpArgs)
	if len(args) == 0 {
		return nil
	}
	_, err := c.run(ctx, c.args(append([]string{"set"}, args...)...)...)
	return err
}

// Logout runs `tailscale logout`, which also removes the node from the tailnet.
func (c *CLI) Logout(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	_, err := c.run(ctx, c.args("logout")...)
	return err
}

// upOnlyFlags are accepted by `tailscale up` but not by `tailscale set`.
var upOnlyFlags = map[string]bool{
	"--login-server": true,
	"--auth-key":     true,
	"--authkey":      true,
	"--force-reauth": true,
	"--reset":        true,
	"--timeout":      true,
	"--qr":           true,
	"--json":         true,
}

// setArgs filters the configured up flags down to those `tailscale set` takes.
func setArgs(up []string) []string {
	var out []string
	for _, a := range up {
		name, _, _ := strings.Cut(a, "=")
		if upOnlyFlags[name] {
			continue
		}
		out = append(out, a)
	}
	return out
}

func (c *CLI) run(ctx context.Context, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, c.opts.Binary, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	c.opts.Logf("running %s %s", c.opts.Binary, strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg != "" {
			return nil, fmt.Errorf("tailscale %s: %w: %s", subcommand(args), err, msg)
		}
		return nil, fmt.Errorf("tailscale %s: %w", subcommand(args), err)
	}
	return stdout.Bytes(), nil
}

// subcommand names the command for an error message: the first argument that
// is not a global flag.
func subcommand(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return "command"
}
