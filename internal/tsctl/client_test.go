package tsctl

import (
	"slices"
	"testing"
)

func TestSetArgs(t *testing.T) {
	tests := []struct {
		name string
		up   []string
		want []string
	}{
		{
			name: "flags set takes pass through",
			up:   []string{"--advertise-routes=10.0.0.0/8", "--accept-dns=false", "--hostname=mac"},
			want: []string{"--advertise-routes=10.0.0.0/8", "--accept-dns=false", "--hostname=mac", "--advertise-exit-node=false"},
		},
		{
			name: "up-only flags are dropped in either spelling",
			up: []string{
				"--login-server=https://hs.example", "-auth-key=tskey-abc", "--advertise-tags=tag:router",
				"--reset", "--advertise-routes=10.0.0.0/8",
			},
			want: []string{"--advertise-routes=10.0.0.0/8", "--advertise-exit-node=false"},
		},
		{
			name: "a value on its own line goes with its flag",
			up:   []string{"--login-server", "https://hs.example", "--advertise-routes", "10.0.0.0/8"},
			want: []string{"--advertise-routes", "10.0.0.0/8", "--advertise-exit-node=false"},
		},
		{
			name: "advertisements missing from the file are withdrawn",
			up:   []string{"--accept-dns=false"},
			want: []string{"--accept-dns=false", "--advertise-routes=", "--advertise-exit-node=false"},
		},
		{
			name: "an exit node keeps its flag",
			up:   []string{"--advertise-exit-node"},
			want: []string{"--advertise-exit-node", "--advertise-routes="},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := setArgs(tt.up); !slices.Equal(got, tt.want) {
				t.Errorf("setArgs(%q)\n got %q\nwant %q", tt.up, got, tt.want)
			}
		})
	}
}

func TestRedact(t *testing.T) {
	in := []string{"--socket=/x.sock", "up", "--auth-key=tskey-abc", "--client-secret", "s3cret", "--hostname=mac"}
	want := []string{"--socket=/x.sock", "up", "--auth-key=<redacted>", "--client-secret", "<redacted>", "--hostname=mac"}
	if got := Redact(in); !slices.Equal(got, want) {
		t.Errorf("Redact(%q)\n got %q\nwant %q", in, got, want)
	}
	if in[2] != "--auth-key=tskey-abc" {
		t.Error("Redact modified its argument")
	}
}
