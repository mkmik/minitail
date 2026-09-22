package tsctl

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"slices"
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
	_, err := c.run(ctx, c.args(append([]string{"set"}, setArgs(c.opts.UpArgs)...)...)...)
	return err
}

// Logout runs `tailscale logout`, which also removes the node from the tailnet.
func (c *CLI) Logout(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	_, err := c.run(ctx, c.args("logout")...)
	return err
}

// upOnlyFlags are accepted by `tailscale up` but rejected by `tailscale set`,
// per cmd/tailscale/cli/{up,set}.go at v1.102.4. Names have no dashes, so the
// "-flag" and "--flag" spellings both match.
var upOnlyFlags = map[string]bool{
	"login-server":   true,
	"auth-key":       true,
	"authkey":        true, // the spelling before 1.40
	"audience":       true,
	"client-id":      true,
	"client-secret":  true,
	"id-token":       true,
	"advertise-tags": true,
	"host-routes":    true,
	"force-reauth":   true,
	"reset":          true,
	"timeout":        true,
	"qr":             true,
	"qr-format":      true,
	"json":           true,
}

// secretFlags carry credentials, which must not reach the log.
var secretFlags = map[string]bool{
	"auth-key":      true,
	"authkey":       true,
	"client-secret": true,
	"id-token":      true,
}

// flagName returns the name of a flag argument without dashes or value, so
// "--auth-key=x", "-auth-key=x" and "--auth-key" all give "auth-key". A value
// argument gives "".
func flagName(arg string) (name string, hasValue bool) {
	if !strings.HasPrefix(arg, "-") {
		return "", false
	}
	name, _, hasValue = strings.Cut(arg, "=")
	return strings.TrimLeft(name, "-"), hasValue
}

// setArgs turns the configured up flags into the argv for `tailscale set`.
//
// Flags only `up` takes are dropped, together with a value given as its own
// argument. The two advertisement flags are always passed, because `set`
// changes only the preferences it is given: without an explicit value, a
// route removed from the file would stay advertised by the node while the
// view, which reads the file, says it is gone.
func setArgs(up []string) []string {
	var out []string
	routes, exitNode := false, false
	for i := 0; i < len(up); i++ {
		name, hasValue := flagName(up[i])
		switch name {
		case "advertise-routes":
			routes = true
		case "advertise-exit-node":
			exitNode = true
		}
		if !upOnlyFlags[name] {
			out = append(out, up[i])
			continue
		}
		// Neither command takes positional arguments, so a following
		// argument that is not a flag can only be this flag's value.
		if !hasValue && i+1 < len(up) && !strings.HasPrefix(up[i+1], "-") {
			i++
		}
	}
	if !routes {
		out = append(out, "--advertise-routes=")
	}
	if !exitNode {
		out = append(out, "--advertise-exit-node=false")
	}
	return out
}

// Redact replaces the value of every credential flag in args, so that a
// command line can be logged.
func Redact(args []string) []string {
	out := slices.Clone(args)
	for i := 0; i < len(out); i++ {
		name, hasValue := flagName(out[i])
		if !secretFlags[name] {
			continue
		}
		switch {
		case hasValue:
			flag, _, _ := strings.Cut(out[i], "=")
			out[i] = flag + "=<redacted>"
		case i+1 < len(out) && !strings.HasPrefix(out[i+1], "-"):
			i++
			out[i] = "<redacted>"
		}
	}
	return out
}

func (c *CLI) run(ctx context.Context, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, c.opts.Binary, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	c.opts.Logf("running %s %s", c.opts.Binary, strings.Join(Redact(args), " "))
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
