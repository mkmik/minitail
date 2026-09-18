package tsctl_test

import (
	"os"
	"path/filepath"
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
	        "Self":{"HostName":"x","ExitNodeOption":true,"FutureField":42}}`
	st, err := tsctl.ParseStatus([]byte(in))
	if err != nil {
		t.Fatalf("ParseStatus: %v", err)
	}
	if !st.ExitNodeApproved() {
		t.Error("ExitNodeApproved() = false, want true")
	}
}

// TestExitNodeApprovedFallsBackToAllowedIPs covers tailscaled builds that do
// not populate ExitNodeOption for the self node: the approved exit routes
// still show up in AllowedIPs, which is what ExitNodeOption is derived from.
func TestExitNodeApprovedFallsBackToAllowedIPs(t *testing.T) {
	tests := []struct {
		name string
		json string
		want bool
	}{
		{
			name: "both default routes approved",
			json: `{"BackendState":"Running","Self":{"AllowedIPs":["100.1.2.3/32","0.0.0.0/0","::/0"]}}`,
			want: true,
		},
		{
			name: "only IPv4 approved",
			json: `{"BackendState":"Running","Self":{"AllowedIPs":["100.1.2.3/32","0.0.0.0/0"]}}`,
			want: false,
		},
		{
			name: "no routes approved",
			json: `{"BackendState":"Running","Self":{"AllowedIPs":["100.1.2.3/32"]}}`,
			want: false,
		},
		{
			name: "a subnet route is not an exit route",
			json: `{"BackendState":"Running","Self":{"AllowedIPs":["10.0.0.0/8","192.168.0.0/16"]}}`,
			want: false,
		},
		{
			name: "no self node",
			json: `{"BackendState":"NeedsLogin"}`,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := tsctl.ParseStatus([]byte(tt.json))
			if err != nil {
				t.Fatalf("ParseStatus: %v", err)
			}
			if got := st.ExitNodeApproved(); got != tt.want {
				t.Errorf("ExitNodeApproved() = %v, want %v", got, tt.want)
			}
		})
	}
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
