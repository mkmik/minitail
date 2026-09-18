//go:build integration

package integration

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// minitailView mirrors app.View, decoded from `minitail status --json`.
type minitailView struct {
	State     string   `json:"State"`
	Summary   string   `json:"Summary"`
	Detail    string   `json:"Detail"`
	AuthURL   string   `json:"AuthURL"`
	TailnetIP string   `json:"TailnetIP"`
	Userspace bool     `json:"Userspace"`
	Restarts  int      `json:"Restarts"`
	Health    []string `json:"Health"`
}

// status reads minitail's own view of the world from inside the exit node.
func (c *compose) status() (minitailView, error) {
	out, err := c.exec("exitnode", "minitail", "status", "--json")
	if err != nil {
		return minitailView{}, err
	}
	var v minitailView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return minitailView{}, err
	}
	return v, nil
}

func (c *compose) mustStatus() minitailView {
	c.t.Helper()
	v, err := c.status()
	if err != nil {
		c.t.Fatalf("reading minitail status: %v", err)
	}
	return v
}

// waitForState blocks until minitail reports the given state.
func (c *compose) waitForState(state string, timeout time.Duration) minitailView {
	return c.waitForView("minitail state "+state, timeout, func(v minitailView) bool {
		return v.State == state
	})
}

// waitForView blocks until minitail's reported view satisfies cond.
func (c *compose) waitForView(what string, timeout time.Duration, cond func(minitailView) bool) minitailView {
	c.t.Helper()
	var final minitailView
	c.eventually(what, timeout, func() (bool, string) {
		v, err := c.status()
		if err != nil {
			return false, "status error: " + err.Error()
		}
		final = v
		return cond(v), v.State + ": " + v.Summary + " / " + v.Detail
	})
	return final
}

// headscaleNode is the subset of `headscale nodes list -o json` used here.
type headscaleNode struct {
	ID              uint64   `json:"id"`
	Name            string   `json:"name"`
	GivenName       string   `json:"given_name"`
	IPAddresses     []string `json:"ip_addresses"`
	Online          bool     `json:"online"`
	AvailableRoutes []string `json:"available_routes"`
	ApprovedRoutes  []string `json:"approved_routes"`
	SubnetRoutes    []string `json:"subnet_routes"`
}

func (c *compose) nodes() []headscaleNode {
	c.t.Helper()
	out := c.mustHeadscale("nodes", "list", "-o", "json")
	var nodes []headscaleNode
	if err := json.Unmarshal([]byte(out), &nodes); err != nil {
		c.t.Fatalf("parsing headscale node list: %v\n%s", err, out)
	}
	return nodes
}

func (c *compose) nodeNamed(name string) headscaleNode {
	c.t.Helper()
	for _, n := range c.nodes() {
		if n.Name == name || n.GivenName == name {
			return n
		}
	}
	c.t.Fatalf("no node named %q in %+v", name, c.nodes())
	return headscaleNode{}
}

// TestExitNodeEndToEnd is the whole PRD walked through in one scenario: a
// fresh state directory, an interactive login, an unapproved advertisement,
// approval, and finally a TCP connection from another device that could not
// have been made without routing through the exit node.
//
// It runs as one test because the topology takes minutes to build; the
// subtests are ordered stages of a single story.
func TestExitNodeEndToEnd(t *testing.T) {
	c := newCompose(t)
	c.waitForHeadscale()
	c.mustHeadscale("users", "create", headscaleUser)

	var exitNodeIP string

	t.Run("NeedsLoginSurfacesAuthURL", func(t *testing.T) {
		// A fresh --statedir means tailscaled has no node key, so minitail
		// must report NeedsLogin and surface the URL rather than sit silent.
		// The state arrives slightly before the URL, so wait for both.
		v := c.waitForView("minitail to surface a login URL", 2*time.Minute, func(v minitailView) bool {
			return v.State == "needs-login" && v.AuthURL != ""
		})
		if !strings.Contains(v.AuthURL, "/register/") {
			t.Errorf("auth URL %q does not look like a Headscale registration URL", v.AuthURL)
		}

		// Complete the login the way a user clicking the link would.
		key := v.AuthURL[strings.LastIndex(v.AuthURL, "/")+1:]
		c.mustHeadscale("nodes", "register", "--user", headscaleUser, "--key", key)
	})

	t.Run("ComesUpInUserspaceMode", func(t *testing.T) {
		// Running, but not yet approved as an exit node.
		v := c.waitForState("not-approved", 2*time.Minute)
		if !v.Userspace {
			t.Error("minitail reports a TUN device; tailscaled must be in userspace-networking mode")
		}
		if v.TailnetIP == "" {
			t.Fatalf("no tailnet address assigned: %+v", v)
		}
		exitNodeIP = v.TailnetIP
		t.Logf("exit node is %s", exitNodeIP)
	})

	t.Run("CreatesNoInterfaceOrRoute", func(t *testing.T) {
		// The central claim: bringing the exit node up changed nothing about
		// the host's network configuration.
		assertUnchanged(t, c, "interfaces", "/baseline/links.txt",
			"ip -o link show | awk '{print $2}' | sort")
		assertUnchanged(t, c, "IPv4 routes", "/baseline/routes4.txt",
			"ip route show | sort")
		assertUnchanged(t, c, "IPv6 routes", "/baseline/routes6.txt",
			"ip -6 route show | sort")

		// No tailscale-looking interface appeared under any name.
		links := c.mustExec("exitnode", "sh", "-c", "ip -o link show")
		for _, needle := range []string{"tailscale", "utun", "tun0"} {
			if strings.Contains(links, needle) {
				t.Errorf("found an interface matching %q:\n%s", needle, links)
			}
		}
	})

	t.Run("LeavesSystemTailscaleStateAlone", func(t *testing.T) {
		// minitail points tailscaled at its own --statedir, so a system-wide
		// state file must be untouched.
		got := c.mustExec("exitnode", "cat", "/var/lib/tailscale/tailscaled.state")
		want := c.mustExec("exitnode", "cat", "/baseline/decoy.state")
		if got != want {
			t.Errorf("the system Tailscale state file changed:\ngot  %q\nwant %q", got, want)
		}
		// And minitail's own state landed where it was told to.
		if _, err := c.exec("exitnode", "test", "-f", "/var/lib/minitail/tailscaled.state"); err != nil {
			t.Error("minitail did not write its state into its own --statedir")
		}
	})

	t.Run("AdvertisementIsVisibleButUnapproved", func(t *testing.T) {
		node := c.nodeNamed(exitNodeHostname)
		if !hasExitRoutes(node.AvailableRoutes) {
			t.Errorf("control plane does not see the exit node advertisement: %+v", node)
		}
		if hasExitRoutes(node.ApprovedRoutes) {
			t.Errorf("routes are already approved, so the unapproved state was not exercised: %+v", node)
		}
		// minitail must say so rather than looking healthy.
		if v := c.mustStatus(); v.State != "not-approved" || v.Detail == "" {
			t.Errorf("expected an explained not-approved state, got %+v", v)
		}
	})

	t.Run("ApprovalMakesItAnExitNode", func(t *testing.T) {
		node := c.nodeNamed(exitNodeHostname)
		c.mustHeadscale("nodes", "approve-routes", "--identifier", strconv.FormatUint(node.ID, 10), "--routes", "0.0.0.0/0,::/0")
		v := c.waitForState("exit-node", 2*time.Minute)
		if v.TailnetIP != exitNodeIP {
			t.Errorf("tailnet address changed from %s to %s", exitNodeIP, v.TailnetIP)
		}
	})

	t.Run("ClientCannotReachDestinationDirectly", func(t *testing.T) {
		// Establish the baseline the end-to-end assertion depends on: without
		// the exit node the destination is simply unreachable.
		out, err := c.exec("client", "curl", "-s", "-m", "5", destURL)
		if err == nil {
			t.Fatalf("the client reached the destination without an exit node, so the test proves nothing: %q", out)
		}
	})

	t.Run("ClientReachesDestinationThroughExitNode", func(t *testing.T) {
		c.loginClient()
		c.mustExec("client", "tailscale", "--socket=/var/run/tailscale/client.sock",
			"set", "--exit-node="+exitNodeIP, "--exit-node-allow-lan-access=true")

		// This is the core end-to-end assertion: a TCP connection from the
		// client to a destination only the exit node can route to.
		c.eventually("the client to reach the destination through the exit node", 2*time.Minute, func() (bool, string) {
			out, err := c.exec("client", "curl", "-s", "-m", "10", destURL)
			if err != nil {
				return false, "curl failed: " + out
			}
			return strings.Contains(out, "reached-the-destination"), out
		})

		// Egress re-dials from an ordinary host socket, so the destination
		// sees the exit node's own address rather than the client's. This is
		// what "netstack is a proxy, not a NAT" means in practice, and it is
		// also why the exit node's routing table is what decides reachability.
		seen := c.mustExec("client", "curl", "-s", "-m", "10", peerURL)
		if seen != exitNodeTransitIP {
			t.Errorf("destination saw source %q, want the exit node's own address %q", seen, exitNodeTransitIP)
		}
	})

	t.Run("DirectlyAttachedLanIsNotForwarded", func(t *testing.T) {
		// Tailscale exit nodes are deliberately "guest wifi": they forward to
		// the internet, not to LANs configured directly on their own
		// interfaces (ipnlocal.shrinkDefaultRoute). The router's transit
		// address sits on such a LAN, so it must stay unreachable even though
		// the exit node itself can reach it and the ACL permits it.
		//
		// This is why minitail has --advertise-routes: a directly attached
		// container bridge (OrbStack) needs to be advertised explicitly.
		if _, err := c.exec("exitnode", "ping", "-c", "1", "-W", "2", routerTransitIP); err != nil {
			t.Fatalf("the exit node cannot reach %s itself, so this proves nothing", routerTransitIP)
		}
		out, err := c.exec("client", "curl", "-s", "-m", "5", routerTransitURL)
		if err == nil {
			t.Errorf("the client reached the exit node's own LAN through it: %q", out)
		}
	})

	t.Run("SupervisorRestartsTailscaled", func(t *testing.T) {
		before := c.mustStatus()
		c.mustExec("exitnode", "sh", "-c", "kill -9 $(pidof tailscaled)")
		c.eventually("the supervisor to restart tailscaled", 2*time.Minute, func() (bool, string) {
			v, err := c.status()
			if err != nil {
				return false, "status error: " + err.Error()
			}
			return v.Restarts > before.Restarts, v.State + " restarts=" + strconv.Itoa(v.Restarts)
		})
		// And it comes all the way back to serving traffic.
		c.waitForState("exit-node", 3*time.Minute)
	})
}

// loginClient registers the client node with a pre-auth key and brings it up.
func (c *compose) loginClient() {
	c.t.Helper()
	users := c.mustHeadscale("users", "list", "-o", "json")
	var list []struct {
		ID   uint64 `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(users), &list); err != nil {
		c.t.Fatalf("parsing headscale users: %v\n%s", err, users)
	}
	var userID uint64
	for _, u := range list {
		if u.Name == headscaleUser {
			userID = u.ID
		}
	}
	if userID == 0 {
		c.t.Fatalf("no user %q in %s", headscaleUser, users)
	}

	out := c.mustHeadscale("preauthkeys", "create", "--user", strconv.FormatUint(userID, 10), "--expiration", "24h", "-o", "json")
	var key struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(out), &key); err != nil {
		c.t.Fatalf("parsing preauth key: %v\n%s", err, out)
	}

	c.mustExec("client", "tailscale", "--socket=/var/run/tailscale/client.sock", "up",
		"--login-server=http://headscale:8080",
		"--auth-key="+key.Key,
		"--hostname=client",
		"--accept-dns=false",
		"--accept-routes=false")
}

// assertUnchanged compares a baseline file captured before minitail started
// against the live output of the same command.
func assertUnchanged(t *testing.T, c *compose, what, baselineFile, command string) {
	t.Helper()
	want := c.mustExec("exitnode", "cat", baselineFile)
	got := c.mustExec("exitnode", "sh", "-c", command)
	if got != want {
		t.Errorf("%s changed after starting the exit node:\n--- before ---\n%s\n--- after ---\n%s", what, want, got)
	}
}

func hasExitRoutes(routes []string) bool {
	var v4, v6 bool
	for _, r := range routes {
		switch r {
		case "0.0.0.0/0":
			v4 = true
		case "::/0":
			v6 = true
		}
	}
	return v4 && v6
}
