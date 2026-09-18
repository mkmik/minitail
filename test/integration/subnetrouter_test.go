//go:build integration

package integration

import (
	"encoding/json"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// minitailView mirrors app.View, decoded from `minitail status --json`.
type minitailView struct {
	State         string   `json:"State"`
	Summary       string   `json:"Summary"`
	Detail        string   `json:"Detail"`
	AuthURL       string   `json:"AuthURL"`
	TailnetIP     string   `json:"TailnetIP"`
	Routes        []string `json:"Routes"`
	PendingRoutes []string `json:"PendingRoutes"`
	Userspace     bool     `json:"Userspace"`
	Restarts      int      `json:"Restarts"`
	Health        []string `json:"Health"`
}

// status reads minitail's own view of the world from inside its container.
func (c *compose) status() (minitailView, error) {
	out, err := c.exec("minitail", "minitail", "status", "--json")
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

// TestSubnetRouter walks the whole thing through in one scenario: a fresh
// state directory, a config file, an interactive login, unapproved routes,
// approval, and finally a tailnet client reaching two networks and a DNS
// server that it has no route to on its own.
//
// It runs as one test because the topology takes minutes to build; the
// subtests are ordered stages of a single story.
func TestSubnetRouter(t *testing.T) {
	c := newCompose(t)
	c.waitForHeadscale()
	c.mustHeadscale("users", "create", headscaleUser)

	t.Run("SeedsAConfigFile", func(t *testing.T) {
		// A fresh install must produce a usable config on its own, with the
		// placeholder route documented in the README.
		out := c.mustExec("minitail", "minitail", "config", "init", "-dir", "/tmp/fresh-config")
		if !strings.Contains(out, "created") {
			t.Errorf("config init said %q, want it to report creating the file", out)
		}
		seeded := c.mustExec("minitail", "cat", "/tmp/fresh-config/minitail.conf")
		for _, want := range []string{
			"--tun=userspace-networking",
			"--advertise-routes=10.0.0.0/8",
			"--accept-dns=false",
		} {
			if !strings.Contains(seeded, want) {
				t.Errorf("the seeded config is missing %q:\n%s", want, seeded)
			}
		}
		// The managed flags must not be in the file, and must be in the command.
		if strings.Contains(seeded, "--socket=") || strings.Contains(seeded, "--statedir=") {
			t.Errorf("the seeded config sets a flag minitail manages:\n%s", seeded)
		}
		shown := c.mustExec("minitail", "minitail", "config", "show", "-dir", "/tmp/fresh-config")
		for _, want := range []string{"--statedir=/tmp/fresh-config/tailscaled", "--socket=/tmp/fresh-config/tailscaled.sock"} {
			if !strings.Contains(shown, want) {
				t.Errorf("config show is missing %q:\n%s", want, shown)
			}
		}
	})

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
		// Running, but the routes are not approved yet.
		v := c.waitForState("not-approved", 2*time.Minute)
		if !v.Userspace {
			t.Error("minitail reports a TUN device; tailscaled must be in userspace-networking mode")
		}
		if v.TailnetIP == "" {
			t.Fatalf("no tailnet address assigned: %+v", v)
		}
		want := []string{advertisedSubnet, advertisedLAN}
		if !slices.Equal(v.Routes, want) {
			t.Errorf("Routes = %v, want the configured %v", v.Routes, want)
		}
		if !slices.Equal(v.PendingRoutes, want) {
			t.Errorf("PendingRoutes = %v, want all of %v", v.PendingRoutes, want)
		}
		t.Logf("node is %s advertising %v", v.TailnetIP, v.Routes)
	})

	t.Run("CreatesNoInterfaceOrRoute", func(t *testing.T) {
		// The central claim: bringing the subnet router up changed nothing
		// about the host's own network configuration.
		assertUnchanged(t, c, "interfaces", "/baseline/links.txt",
			"ip -o link show | awk '{print $2}' | sort")
		assertUnchanged(t, c, "IPv4 routes", "/baseline/routes4.txt",
			"ip route show | sort")
		assertUnchanged(t, c, "IPv6 routes", "/baseline/routes6.txt",
			"ip -6 route show | sort")

		// No tailscale-looking interface appeared under any name.
		links := c.mustExec("minitail", "sh", "-c", "ip -o link show")
		for _, needle := range []string{"tailscale", "utun", "tun0"} {
			if strings.Contains(links, needle) {
				t.Errorf("found an interface matching %q:\n%s", needle, links)
			}
		}
	})

	t.Run("LeavesSystemTailscaleStateAlone", func(t *testing.T) {
		// minitail points tailscaled at its own --statedir, so a system-wide
		// state file must be untouched.
		got := c.mustExec("minitail", "cat", "/var/lib/tailscale/tailscaled.state")
		want := c.mustExec("minitail", "cat", "/baseline/decoy.state")
		if got != want {
			t.Errorf("the system Tailscale state file changed:\ngot  %q\nwant %q", got, want)
		}
		// And minitail's own state landed where it was told to.
		if _, err := c.exec("minitail", "test", "-f", "/var/lib/minitail/tailscaled/tailscaled.state"); err != nil {
			t.Error("minitail did not write its state into its own --statedir")
		}
	})

	t.Run("AdvertisementIsVisibleButUnapproved", func(t *testing.T) {
		node := c.nodeNamed(nodeHostname)
		for _, want := range []string{advertisedSubnet, advertisedLAN} {
			if !slices.Contains(node.AvailableRoutes, want) {
				t.Errorf("control plane does not see %s advertised: %+v", want, node)
			}
			if slices.Contains(node.ApprovedRoutes, want) {
				t.Errorf("%s is already approved, so the unapproved state was not exercised", want)
			}
		}
		// minitail must say so rather than looking healthy.
		if v := c.mustStatus(); v.State != "not-approved" || v.Detail == "" {
			t.Errorf("expected an explained not-approved state, got %+v", v)
		}
	})

	t.Run("PartialApprovalIsStillPending", func(t *testing.T) {
		// Approving one of two routes must not make minitail look finished.
		node := c.nodeNamed(nodeHostname)
		c.approveRoutes(node.ID, advertisedSubnet)
		v := c.waitForView("minitail to report one route still pending", 2*time.Minute, func(v minitailView) bool {
			return slices.Equal(v.PendingRoutes, []string{advertisedLAN})
		})
		if v.State != "not-approved" {
			t.Errorf("State = %q with a route still pending, want not-approved", v.State)
		}
	})

	t.Run("ApprovalMakesItServe", func(t *testing.T) {
		node := c.nodeNamed(nodeHostname)
		c.approveRoutes(node.ID, advertisedSubnet, advertisedLAN)
		v := c.waitForState("serving", 2*time.Minute)
		if len(v.PendingRoutes) != 0 {
			t.Errorf("PendingRoutes = %v, want none once approved", v.PendingRoutes)
		}
	})

	t.Run("ClientCannotReachAnythingDirectly", func(t *testing.T) {
		// Establish the baseline the end-to-end assertions depend on.
		for _, url := range []string{destURL, routerTransitURL} {
			if out, err := c.exec("client", "curl", "-s", "-m", "5", url); err == nil {
				t.Fatalf("the client reached %s without the subnet router, so the test proves nothing: %q", url, out)
			}
		}
	})

	t.Run("ClientReachesRoutedSubnet", func(t *testing.T) {
		c.loginClient()

		// The core assertion: a TCP connection from the client to a network
		// only the subnet router can route to.
		c.eventually("the client to reach the routed subnet", 2*time.Minute, func() (bool, string) {
			out, err := c.exec("client", "curl", "-s", "-m", "10", destURL)
			if err != nil {
				return false, "curl failed: " + out
			}
			return strings.Contains(out, "reached-the-destination"), out
		})

		// Traffic is re-dialled from an ordinary host socket, so the
		// destination sees the subnet router's own address rather than the
		// client's. This is what "netstack is a proxy, not a NAT" means, and
		// why the router's own routing table decides reachability.
		seen := c.mustExec("client", "curl", "-s", "-m", "10", peerURL)
		got, err := parsePeerAddr(seen)
		if err != nil {
			t.Fatalf("destination reported an unparseable source address %q: %v", seen, err)
		}
		if got.String() != nodeTransitIP {
			t.Errorf("destination saw source %s (reported as %q), want the router's own address %s",
				got, seen, nodeTransitIP)
		}
	})

	t.Run("ClientReachesDirectlyAttachedLAN", func(t *testing.T) {
		// The contrast that motivates using a subnet route rather than an
		// exit node: 198.51.100.0/24 is configured directly on one of this
		// node's interfaces. An exit node refuses to forward there
		// (ipnlocal.shrinkDefaultRoute treats it as "guest wifi"); a subnet
		// route must serve it. This is the case a container bridge falls into.
		c.eventually("the client to reach the directly attached LAN", 2*time.Minute, func() (bool, string) {
			out, err := c.exec("client", "curl", "-s", "-m", "10", routerTransitURL)
			if err != nil {
				return false, "curl failed: " + out
			}
			return strings.Contains(out, "reached-the-destination"), out
		})
	})

	t.Run("ClientResolvesThroughSubnetRoute", func(t *testing.T) {
		// Split DNS works by pointing a nameserver at a DNS server inside an
		// advertised subnet, so the query has to survive UDP/53 forwarding
		// through netstack.
		// busybox nslookup asks for A and AAAA and exits non-zero when either
		// goes unanswered, which it does here because the name has no AAAA
		// record. The answer is what matters, so judge the output.
		c.eventually("the client to resolve through the subnet route", 2*time.Minute, func() (bool, string) {
			out, _ := c.exec("client", "nslookup", "-type=A", dnsName, dnsServer)
			return strings.Contains(out, destAddr), out
		})
		t.Logf("%s resolved to %s via the DNS server at %s", dnsName, destAddr, dnsServer)
	})

	t.Run("SupervisorRestartsTailscaled", func(t *testing.T) {
		before := c.mustStatus()
		c.mustExec("minitail", "sh", "-c", "kill -9 $(pidof tailscaled)")
		c.eventually("the supervisor to restart tailscaled", 2*time.Minute, func() (bool, string) {
			v, err := c.status()
			if err != nil {
				return false, "status error: " + err.Error()
			}
			return v.Restarts > before.Restarts, v.State + " restarts=" + strconv.Itoa(v.Restarts)
		})
		// And it comes all the way back to serving traffic.
		c.waitForState("serving", 3*time.Minute)
	})
}

// approveRoutes approves exactly the given routes on a node.
func (c *compose) approveRoutes(id uint64, routes ...string) {
	c.t.Helper()
	c.mustHeadscale("nodes", "approve-routes",
		"--identifier", strconv.FormatUint(id, 10),
		"--routes", strings.Join(routes, ","))
}

// loginClient registers the client node with a pre-auth key and brings it up.
//
// --accept-routes is what makes a client install the subnet routes a router
// advertises; without it the advertisement is invisible to it.
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
		"--accept-routes=true")
}

// assertUnchanged compares a baseline file captured before minitail started
// against the live output of the same command.
func assertUnchanged(t *testing.T, c *compose, what, baselineFile, command string) {
	t.Helper()
	want := c.mustExec("minitail", "cat", baselineFile)
	got := c.mustExec("minitail", "sh", "-c", command)
	if got != want {
		t.Errorf("%s changed after starting the subnet router:\n--- before ---\n%s\n--- after ---\n%s", what, want, got)
	}
}

// parsePeerAddr normalises the address the destination reported. A dual-stack
// listener reports an IPv4 peer in IPv4-mapped form ("[::ffff:198.51.100.20]"),
// which is the same address written differently.
func parsePeerAddr(s string) (netip.Addr, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, err
	}
	return addr.Unmap(), nil
}
