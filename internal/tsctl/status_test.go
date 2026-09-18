package tsctl_test

import (
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/mkmik/minitail/internal/tsctl"
)

func TestParseStatusRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "not json", "{}", "null"} {
		if _, err := tsctl.ParseStatus([]byte(in)); err == nil {
			t.Errorf("ParseStatus(%q) succeeded, want an error", in)
		}
	}
}

// TestParseStatusIgnoresUnknownFields guards the version matrix: a newer
// tailscaled adds fields, and minitail must not care.
func TestParseStatusIgnoresUnknownFields(t *testing.T) {
	in := `{"BackendState":"Running","SomethingBrandNew":{"a":1},"TUN":false,
	        "Self":{"HostName":"x","AllowedIPs":["10.0.0.0/8"],"FutureField":42}}`
	st, err := tsctl.ParseStatus([]byte(in))
	if err != nil {
		t.Fatalf("ParseStatus: %v", err)
	}
	if pending := st.PendingRoutes(prefixes("10.0.0.0/8")); len(pending) != 0 {
		t.Errorf("PendingRoutes = %v, want none", pending)
	}
}

// TestPendingRoutes is the signal that separates "connected" from "connected
// and actually usable by peers": a route counts only once the control plane
// has put it in the self node's AllowedIPs.
func TestPendingRoutes(t *testing.T) {
	tests := []struct {
		name       string
		json       string
		advertised []netip.Prefix
		want       []netip.Prefix
	}{
		{
			name:       "approved",
			json:       `{"BackendState":"Running","Self":{"AllowedIPs":["100.1.2.3/32","10.0.0.0/8"]}}`,
			advertised: prefixes("10.0.0.0/8"),
		},
		{
			name:       "not approved",
			json:       `{"BackendState":"Running","Self":{"AllowedIPs":["100.1.2.3/32"]}}`,
			advertised: prefixes("10.0.0.0/8"),
			want:       prefixes("10.0.0.0/8"),
		},
		{
			name:       "partially approved",
			json:       `{"BackendState":"Running","Self":{"AllowedIPs":["100.1.2.3/32","10.0.0.0/8"]}}`,
			advertised: prefixes("10.0.0.0/8", "192.168.7.0/24"),
			want:       prefixes("192.168.7.0/24"),
		},
		{
			name:       "a wider approval does not cover a narrower advertisement",
			json:       `{"BackendState":"Running","Self":{"AllowedIPs":["0.0.0.0/0"]}}`,
			advertised: prefixes("10.0.0.0/8"),
			want:       prefixes("10.0.0.0/8"),
		},
		{
			name:       "exit node routes",
			json:       `{"BackendState":"Running","Self":{"AllowedIPs":["0.0.0.0/0","::/0"]}}`,
			advertised: prefixes("0.0.0.0/0", "::/0"),
		},
		{
			name:       "nothing advertised",
			json:       `{"BackendState":"Running","Self":{"AllowedIPs":["100.1.2.3/32"]}}`,
			advertised: nil,
		},
		{
			name:       "no self node yet",
			json:       `{"BackendState":"NeedsLogin"}`,
			advertised: prefixes("10.0.0.0/8"),
			want:       prefixes("10.0.0.0/8"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := tsctl.ParseStatus([]byte(tt.json))
			if err != nil {
				t.Fatalf("ParseStatus: %v", err)
			}
			if got := st.PendingRoutes(tt.advertised); !slices.Equal(got, tt.want) {
				t.Errorf("PendingRoutes() = %v, want %v", got, tt.want)
			}
		})
	}
}

func prefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

func TestFirstIPv4(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{
			name: "prefers the v4 address",
			json: `{"BackendState":"Running","TailscaleIPs":["fd7a:115c:a1e0::1","100.64.0.1"]}`,
			want: "100.64.0.1",
		},
		{
			name: "falls back to the self node",
			json: `{"BackendState":"Running","Self":{"TailscaleIPs":["100.64.0.2"]}}`,
			want: "100.64.0.2",
		},
		{
			name: "v6 only",
			json: `{"BackendState":"Running","TailscaleIPs":["fd7a:115c:a1e0::1"]}`,
			want: "fd7a:115c:a1e0::1",
		},
		{
			name: "none",
			json: `{"BackendState":"NeedsLogin"}`,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := tsctl.ParseStatus([]byte(tt.json))
			if err != nil {
				t.Fatalf("ParseStatus: %v", err)
			}
			if got := st.FirstIPv4(); got != tt.want {
				t.Errorf("FirstIPv4() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFixturesReportUserspace is the assertion that ties the fixtures back to
// the reason this project exists: TUN must be false in every captured state.
func TestFixturesReportUserspace(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("..", "app", "testdata", "*.json"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no fixtures found: %v", err)
	}
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		st, err := tsctl.ParseStatus(b)
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		if st.TUN {
			t.Errorf("%s: TUN = true, want false in userspace-networking mode", m)
		}
	}
}
