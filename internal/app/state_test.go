package app_test

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mkmik/minitail/internal/app"
	"github.com/mkmik/minitail/internal/tsctl"
)

// subnet is what the fixtures advertise, matching the seeded config.
var subnet = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}

var exitRoutes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/0"),
	netip.MustParsePrefix("::/0"),
}

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
		fixture    string
		advertised []netip.Prefix
		want       app.State
		wantIP     string
		attention  bool
	}{
		{fixture: "stopped", advertised: subnet, want: app.StateDown},
		{fixture: "needs-login", advertised: subnet, want: app.StateNeedsLogin, attention: true},
		{fixture: "connecting", advertised: subnet, want: app.StateConnecting, wantIP: "100.101.102.103"},
		{
			fixture: "running-not-approved", advertised: subnet,
			want: app.StateNotApproved, wantIP: "100.101.102.103", attention: true,
		},
		{
			fixture: "running-approved", advertised: subnet,
			want: app.StateServing, wantIP: "100.101.102.103",
		},
		// An exit node is the same machinery with the default routes.
		{
			fixture: "running-exit-node", advertised: exitRoutes,
			want: app.StateServing, wantIP: "100.101.102.103",
		},
		// ...and an exit node fixture does not satisfy a subnet advertisement.
		{
			fixture: "running-exit-node", advertised: subnet,
			want: app.StateNotApproved, wantIP: "100.101.102.103", attention: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.fixture+"/"+strings.Join(prefixes(tt.advertised), ","), func(t *testing.T) {
			st := loadFixture(t, tt.fixture)
			got := app.Derive(app.Input{
				WantRunning: true, DaemonRunning: true, Status: st, Advertised: tt.advertised,
			})
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
	got := app.Derive(app.Input{WantRunning: true, DaemonRunning: true, Status: st, Advertised: subnet})
	if got.AuthURL != "https://login.tailscale.com/a/8f2c41d0b93e" {
		t.Errorf("AuthURL = %q, want the URL from the fixture", got.AuthURL)
	}
}

// TestDeriveNamesPendingRoutes is what keeps the app from looking healthy
// while no peer can route anything through it.
func TestDeriveNamesPendingRoutes(t *testing.T) {
	st := loadFixture(t, "running-not-approved")
	two := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("192.168.7.0/24"),
	}
	got := app.Derive(app.Input{WantRunning: true, DaemonRunning: true, Status: st, Advertised: two})
	if !slices.Equal(got.PendingRoutes, []string{"10.0.0.0/8", "192.168.7.0/24"}) {
		t.Errorf("PendingRoutes = %v, want both routes", got.PendingRoutes)
	}
	if !strings.Contains(got.Summary, "10.0.0.0/8") {
		t.Errorf("Summary = %q, want it to name the pending routes", got.Summary)
	}

	// With more than two, naming them all would not fit a menu bar line.
	many := append(slices.Clone(two), netip.MustParsePrefix("172.20.0.0/16"))
	got = app.Derive(app.Input{WantRunning: true, DaemonRunning: true, Status: st, Advertised: many})
	if !strings.Contains(got.Summary, "3 routes") {
		t.Errorf("Summary = %q, want a count for three routes", got.Summary)
	}
}

// TestDerivePartialApproval covers the case where the admin approved some of
// the advertised routes but not all.
func TestDerivePartialApproval(t *testing.T) {
	st := loadFixture(t, "running-approved") // has 10.0.0.0/8 approved
	both := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("192.168.7.0/24"),
	}
	got := app.Derive(app.Input{WantRunning: true, DaemonRunning: true, Status: st, Advertised: both})
	if got.State != app.StateNotApproved {
		t.Fatalf("State = %q, want %q", got.State, app.StateNotApproved)
	}
	if !slices.Equal(got.PendingRoutes, []string{"192.168.7.0/24"}) {
		t.Errorf("PendingRoutes = %v, want only the unapproved one", got.PendingRoutes)
	}
}

// TestDeriveNoRoutesAdvertised: connected, but not doing minitail's job.
func TestDeriveNoRoutesAdvertised(t *testing.T) {
	st := loadFixture(t, "running-approved")
	got := app.Derive(app.Input{WantRunning: true, DaemonRunning: true, Status: st})
	if got.State != app.StateServing {
		t.Errorf("State = %q, want %q", got.State, app.StateServing)
	}
	if !strings.Contains(got.Detail, "advertise-routes") {
		t.Errorf("Detail = %q, want it to point at the config file", got.Detail)
	}
}

func TestDeriveLifecycle(t *testing.T) {
	running := loadFixture(t, "running-approved")

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
	st := loadFixture(t, "running-approved")
	st.BackendState = "SomethingNew"
	v := app.Derive(app.Input{WantRunning: true, DaemonRunning: true, Status: st})
	if v.State != app.StateConnecting {
		t.Errorf("State = %q, want %q for an unrecognised backend state", v.State, app.StateConnecting)
	}
}

func prefixes(ps []netip.Prefix) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	return out
}
