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
	// SetExitNode re-asserts the exit node advertisement on an already
	// logged-in node whose stored preferences may predate it.
	SetExitNode(ctx context.Context) error
	// Logout removes the node from the tailnet and clears local state.
	Logout(ctx context.Context) error
}

// Options configures a CLI-backed Client.
type Options struct {
	// Binary is the path to the `tailscale` CLI.
	Binary string
	// Socket is the tailscaled LocalAPI socket to talk to.
	Socket string
	// Hostname is the name to register under.
	Hostname string
	// ControlURL overrides the coordination server when non-empty.
	ControlURL string
	// AuthKey makes login non-interactive when non-empty.
	AuthKey string
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
	return append([]string{"--socket=" + c.opts.Socket}, rest...)
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

// upFlags are shared by Up and SetExitNode.
//
// --accept-routes=false and --accept-dns=false are belt and braces: in
// userspace-networking mode tailscaled cannot install routes or touch the
// resolver anyway, but stating it means a future change of --tun mode cannot
// silently start doing so.
func (c *CLI) upFlags() []string {
	return []string{
		"--advertise-exit-node",
		"--accept-routes=false",
		"--accept-dns=false",
		"--advertise-routes=", // exit node only, never a subnet router
	}
}

// Up runs `tailscale up`. It blocks until the node is authenticated, which for
// an interactive login means until the user visits the auth URL.
func (c *CLI) Up(ctx context.Context) error {
	args := c.args("up")
	args = append(args, c.upFlags()...)
	args = append(args, "--hostname="+c.opts.Hostname)
	if c.opts.ControlURL != "" {
		args = append(args, "--login-server="+c.opts.ControlURL)
	}
	if c.opts.AuthKey != "" {
		args = append(args, "--auth-key="+c.opts.AuthKey)
	}
	// No timeout: an interactive login legitimately takes as long as the user
	// takes. The caller cancels ctx to give up.
	_, err := c.run(ctx, args...)
	return err
}

// SetExitNode runs `tailscale set` to re-apply the exit node advertisement
// without re-authenticating.
func (c *CLI) SetExitNode(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	args := c.args("set")
	args = append(args, c.upFlags()...)
	_, err := c.run(ctx, args...)
	return err
}

// Logout runs `tailscale logout`, which also removes the node from the tailnet.
func (c *CLI) Logout(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	_, err := c.run(ctx, c.args("logout")...)
	return err
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
			return nil, fmt.Errorf("tailscale %s: %w: %s", args[1], err, msg)
		}
		return nil, fmt.Errorf("tailscale %s: %w", args[1], err)
	}
	return stdout.Bytes(), nil
}
