package app_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mkmik/minitail/internal/app"
	"github.com/mkmik/minitail/internal/tsctl"
)

// loadFixture parses a recorded `tailscale status --json` capture.
func loadFixture(t *testing.T, name string) *tsctl.Status {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	st, err := tsctl.ParseStatus(b)
	if err != nil {
		t.Fatalf("parsing fixture %s: %v", name, err)
	}
	return st
}

func TestDeriveFromFixtures(t *testing.T) {
	tests := []struct {
		fixture   string
		want      app.State
		wantIP    string
		attention bool
	}{
		{fixture: "stopped", want: app.StateDown, attention: false},
		{fixture: "needs-login", want: app.StateNeedsLogin, attention: true},
		{fixture: "connecting", want: app.StateConnecting, wantIP: "100.101.102.103"},
		{fixture: "running-not-approved", want: app.StateNotApproved, wantIP: "100.101.102.103", attention: true},
		{fixture: "running-exit-node", want: app.StateExitNode, wantIP: "100.101.102.103"},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			st := loadFixture(t, tt.fixture)
			got := app.Derive(app.Input{WantRunning: true, DaemonRunning: true, Status: st})
			if got.State != tt.want {
				t.Errorf("State = %q, want %q", got.State, tt.want)
			}
			if got.TailnetIP != tt.wantIP {
				t.Errorf("TailnetIP = %q, want %q", got.TailnetIP, tt.wantIP)
			}
			if got.State.NeedsAttention() != tt.attention {
				t.Errorf("NeedsAttention() = %v, want %v", got.State.NeedsAttention(), tt.attention)
			}
			if got.Summary == "" {
				t.Error("Summary is empty")
			}
			// Every fixture was captured in userspace-networking mode, which
			// is the property the whole project exists to preserve.
			if !got.Userspace {
				t.Error("Userspace = false, want true (TUN must stay false)")
			}
		})
	}
}

func TestDeriveSurfacesAuthURL(t *testing.T) {
	st := loadFixture(t, "needs-login")
	got := app.Derive(app.Input{WantRunning: true, DaemonRunning: true, Status: st})
	if got.AuthURL != "https://login.tailscale.com/a/8f2c41d0b93e" {
		t.Errorf("AuthURL = %q, want the URL from the fixture", got.AuthURL)
	}
}

// TestDeriveDistinguishesApproval is the assertion that keeps the app from
// looking healthy while no peer can actually select it as an exit node.
func TestDeriveDistinguishesApproval(t *testing.T) {
	notApproved := loadFixture(t, "running-not-approved")
	if notApproved.ExitNodeApproved() {
		t.Fatal("fixture running-not-approved reports an approved exit node")
	}
	approved := loadFixture(t, "running-exit-node")
	if !approved.ExitNodeApproved() {
		t.Fatal("fixture running-exit-node reports an unapproved exit node")
	}
}

func TestDeriveLifecycle(t *testing.T) {
	running := loadFixture(t, "running-exit-node")

	tests := []struct {
		name string
		in   app.Input
		want app.State
	}{
		{
			name: "user has not started it",
			in:   app.Input{WantRunning: false, DaemonRunning: true, Status: running},
			want: app.StateStopped,
		},
		{
			name: "daemon not up yet",
			in:   app.Input{WantRunning: true},
			want: app.StateStarting,
		},
		{
			name: "daemon up but no status yet",
			in:   app.Input{WantRunning: true, DaemonRunning: true},
			want: app.StateStarting,
		},
		{
			name: "status poll failing",
			in:   app.Input{WantRunning: true, DaemonRunning: true, StatusErr: errors.New("connection refused")},
			want: app.StateStarting,
		},
		{
			name: "supervisor gave up",
			in:   app.Input{WantRunning: true, GivenUp: true, DaemonErr: errors.New("exec format error")},
			want: app.StateError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := app.Derive(tt.in); got.State != tt.want {
				t.Errorf("State = %q, want %q", got.State, tt.want)
			}
		})
	}
}

func TestDeriveIncludesErrorDetail(t *testing.T) {
	v := app.Derive(app.Input{WantRunning: true, GivenUp: true, DaemonErr: errors.New("exec format error")})
	if v.Detail == "" || v.Summary == "" {
		t.Fatalf("expected a summary and detail, got %+v", v)
	}
	if want := "exec format error"; !strings.Contains(v.Detail, want) {
		t.Errorf("Detail = %q, want it to mention %q", v.Detail, want)
	}
}

func TestDeriveUnknownBackendState(t *testing.T) {
	st := loadFixture(t, "running-exit-node")
	st.BackendState = "SomethingNew"
	v := app.Derive(app.Input{WantRunning: true, DaemonRunning: true, Status: st})
	if v.State != app.StateConnecting {
		t.Errorf("State = %q, want %q for an unrecognised backend state", v.State, app.StateConnecting)
	}
}
