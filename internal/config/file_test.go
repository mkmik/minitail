package config_test

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/mkmik/minitail/internal/config"
)

func TestParseFile(t *testing.T) {
	f, err := config.ParseFile(`
# a comment
[tailscaled]
--tun=userspace-networking   # trailing comment
--port=41642

[up]

--advertise-routes=10.0.0.0/8,192.168.7.0/24
--hostname=my mac
`)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if want := []string{"--tun=userspace-networking", "--port=41642"}; !slices.Equal(f.Tailscaled, want) {
		t.Errorf("Tailscaled = %v, want %v", f.Tailscaled, want)
	}
	want := []string{"--advertise-routes=10.0.0.0/8,192.168.7.0/24", "--hostname=my mac"}
	if !slices.Equal(f.Up, want) {
		t.Errorf("Up = %v, want %v", f.Up, want)
	}
}

func TestParseFileErrors(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"flag before a section", "--tun=userspace-networking\n", "before any [section]"},
		{"unknown section", "[tailscale]\n--socket=x\n", "unknown section"},
		{"unterminated header", "[up\n", "unterminated"},
		// Letting the file set these would break the isolation from a system
		// Tailscale install that is the point of the project.
		{"managed socket", "[tailscaled]\n--socket=/tmp/x.sock\n", "managed by minitail"},
		{"managed statedir", "[tailscaled]\n--statedir=/tmp/x\n", "managed by minitail"},
		{"managed state", "[tailscaled]\n--state=/tmp/x.state\n", "managed by minitail"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.ParseFile(tt.in)
			if err == nil {
				t.Fatalf("ParseFile(%q) succeeded, want an error", tt.in)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestManagedFlagsAllowedInUp checks the restriction is scoped: --socket means
// something different to `tailscale up`, and is not minitail's business there.
func TestManagedFlagsAllowedInUp(t *testing.T) {
	if _, err := config.ParseFile("[up]\n--advertise-routes=10.0.0.0/8\n"); err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
}

func TestAdvertisedRoutes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []netip.Prefix
	}{
		{
			name: "none",
			in:   "[up]\n--accept-dns=false\n",
		},
		{
			name: "comma separated",
			in:   "[up]\n--advertise-routes=10.0.0.0/8, 192.168.7.0/24\n",
			want: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("192.168.7.0/24")},
		},
		{
			name: "two-token form",
			in:   "[up]\n--advertise-routes\n10.0.0.0/8\n",
			want: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		},
		{
			name: "host bits are masked off, as tailscale does",
			in:   "[up]\n--advertise-routes=10.1.2.3/8\n",
			want: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		},
		{
			name: "exit node expands to both default routes",
			in:   "[up]\n--advertise-exit-node\n",
			want: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")},
		},
		{
			name: "exit node explicitly off",
			in:   "[up]\n--advertise-exit-node=false\n",
		},
		{
			name: "subnets and exit node together",
			in:   "[up]\n--advertise-routes=10.0.0.0/8\n--advertise-exit-node=true\n",
			want: []netip.Prefix{
				netip.MustParsePrefix("10.0.0.0/8"),
				netip.MustParsePrefix("0.0.0.0/0"),
				netip.MustParsePrefix("::/0"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := config.ParseFile(tt.in)
			if err != nil {
				t.Fatalf("ParseFile: %v", err)
			}
			got, err := f.AdvertisedRoutes()
			if err != nil {
				t.Fatalf("AdvertisedRoutes: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("AdvertisedRoutes() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAdvertisedRoutesRejectsGarbage(t *testing.T) {
	f, err := config.ParseFile("[up]\n--advertise-routes=not-a-cidr\n")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if _, err := f.AdvertisedRoutes(); err == nil {
		t.Error("AdvertisedRoutes() accepted a non-CIDR value")
	}
}
