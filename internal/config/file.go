package config

import (
	"fmt"
	"net/netip"
	"strings"
)

// File is the parsed config file: the flags to pass to each command.
type File struct {
	// Tailscaled are appended to the tailscaled command line.
	Tailscaled []string
	// Up are passed to `tailscale up`, and to `tailscale set` when the node
	// is already logged in.
	Up []string
}

// managedFlags are supplied by minitail itself. Setting them in the config
// file would break the isolation from a system Tailscale install that is the
// whole point, so it is an error rather than a silent override.
var managedFlags = []string{"--statedir", "--state", "--socket"}

// ParseFile reads minitail.conf.
//
// The format is deliberately minimal: one command-line argument per line under
// a [section] header, with # comments. Lines are not shell-parsed, so a value
// containing spaces needs no quoting and quotes are not stripped. Usually an
// argument is "--flag=value", but the two-token "--flag" then "value" form
// works too, because each line becomes its own argv element.
func ParseFile(text string) (File, error) {
	var f File
	var section string
	for n, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		lineno := n + 1

		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return File{}, fmt.Errorf("line %d: unterminated section header %q", lineno, line)
			}
			section = line[1 : len(line)-1]
			switch section {
			case "tailscaled", "up":
			default:
				return File{}, fmt.Errorf("line %d: unknown section [%s]; expected [tailscaled] or [up]", lineno, section)
			}
			continue
		}

		if section == "" {
			return File{}, fmt.Errorf("line %d: %q appears before any [section] header", lineno, line)
		}
		if section == "tailscaled" {
			// A value line cannot be a flag name, so checking every line is
			// enough to catch both "--socket=x" and "--socket" then "x".
			if name, _, _ := strings.Cut(line, "="); containsFold(managedFlags, name) {
				return File{}, fmt.Errorf("line %d: %s is managed by minitail and must not be set here", lineno, name)
			}
			f.Tailscaled = append(f.Tailscaled, line)
			continue
		}
		f.Up = append(f.Up, line)
	}
	return f, nil
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// exitRoutes are what --advertise-exit-node expands to.
var exitRoutes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/0"),
	netip.MustParsePrefix("::/0"),
}

// AdvertisedRoutes returns the routes the [up] section advertises.
//
// This is the one thing minitail reads out of the config rather than passing
// through: comparing it against the routes the control plane has approved is
// what lets minitail say "waiting for approval" instead of looking healthy
// while no peer can actually use the node.
func (f File) AdvertisedRoutes() ([]netip.Prefix, error) {
	var routes []netip.Prefix
	for i, arg := range f.Up {
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--advertise-exit-node":
			// Absent or "=true"; anything else turns it off.
			if !hasValue || isTrue(value) {
				routes = append(routes, exitRoutes...)
			}
		case "--advertise-routes":
			if !hasValue {
				// tailscale also accepts "--advertise-routes 10.0.0.0/8".
				if i+1 >= len(f.Up) {
					return nil, fmt.Errorf("--advertise-routes has no value")
				}
				value = f.Up[i+1]
			}
			for _, s := range strings.Split(value, ",") {
				s = strings.TrimSpace(s)
				if s == "" {
					continue
				}
				p, err := netip.ParsePrefix(s)
				if err != nil {
					return nil, fmt.Errorf("--advertise-routes: %q is not a CIDR prefix: %w", s, err)
				}
				routes = append(routes, p.Masked())
			}
		}
	}
	return routes, nil
}

func isTrue(s string) bool {
	return strings.EqualFold(s, "true") || s == "1"
}

// DefaultFile is the config seeded on first run. The advertised route is a
// placeholder, which is why the comment says so loudly.
func DefaultFile(hostname string) string {
	return `# minitail configuration.
#
# minitail runs tailscaled and ` + "`tailscale up`" + ` with exactly the flags below. It
# adds only --statedir and --socket, which point this instance at its own
# state and keep it separate from any system-wide Tailscale install; set those
# here and minitail will refuse to start.
#
# One argument per line. Blank lines and # comments are ignored. Lines are not
# shell-parsed, so a value containing spaces needs no quoting.
#
# After editing, restart minitail:
#
#     brew services restart minitail
#
# or use Stop then Start in the menu bar.

[tailscaled]
# Userspace networking is the point of minitail: tailscaled implements its
# network stack in-process, so it creates no interface, installs no route and
# never touches this Mac's DNS configuration. Subnet routing still works —
# traffic is proxied out through ordinary host sockets, which means it follows
# whatever routing table this Mac currently has.
--tun=userspace-networking
# Not tailscaled's own default of 41641, so this can run beside a system
# install without the two fighting over the port.
--port=41642

[up]
# The subnets to advertise, comma-separated. THIS IS A PLACEHOLDER: replace it
# with the networks you actually want reachable through this Mac. Routes must
# also be approved in the admin console before peers can use them, and minitail
# tells you when they have not been.
--advertise-routes=10.0.0.0/8
# Do not reroute this Mac's own traffic, and do not let Tailscale take over its
# DNS. Both are off because something else on this machine owns the network.
--accept-routes=false
--accept-dns=false
--hostname=` + hostname + `-minitail
`
}
