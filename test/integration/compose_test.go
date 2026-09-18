//go:build integration

// Package integration runs minitail against a real Headscale control server
// and real Tailscale clients in Docker. It needs no Tailscale credentials.
//
// Run it with: go test -tags integration -timeout 20m ./test/integration/
package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// defaultTSVersion is the tailscaled release used when TS_VERSION is unset.
// CI runs the suite against this and at least one prior release, because
// netstack behaviour and the status JSON shape are the two things most likely
// to shift underneath minitail.
const defaultTSVersion = "1.102.4"

// The test topology, mirroring docker-compose.yml.
const (
	// destAddr/destURL are served by the router on an address it owns
	// privately. The minitail node can only reach it by following
	// destNetwork's static route.
	destAddr = "192.0.2.40"
	destURL  = "http://192.0.2.40:8080/"
	// destNetwork is the network the minitail node needs a route to.
	destNetwork = "192.0.2.0/24"
	// routerTransitIP is the router's address on a LAN the minitail node is
	// directly attached to, serving the very same content as destURL. An exit
	// node would refuse to forward there; a subnet route must not.
	routerTransitIP  = "198.51.100.2"
	routerTransitURL = "http://198.51.100.2:8080/"
	// dnsServer is inside an advertised subnet, so queries reach it through
	// the subnet router. dnsName is what it resolves.
	dnsServer = "192.0.2.53"
	dnsName   = "corp.internal"
	// advertised are the routes the node under test offers.
	advertisedSubnet = "192.0.2.0/24"
	advertisedLAN    = "198.51.100.0/24"
	// peerURL reports back the source address the destination actually sees.
	peerURL = "http://192.0.2.40:8080/cgi-bin/peer"
	// nodeTransitIP is the minitail node's own address on the transit LAN,
	// which is the source address its outbound sockets use.
	nodeTransitIP = "198.51.100.20"
	// nodeHostname is the name the node under test registers under; it comes
	// from the config file the entrypoint writes.
	nodeHostname = "minitail-test"
	// headscaleUser is the Headscale user both nodes belong to.
	headscaleUser = "test"
)

// compose drives the test topology.
type compose struct {
	t   *testing.T
	env []string
}

func newCompose(t *testing.T) *compose {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not available")
	}
	tsVersion := os.Getenv("TS_VERSION")
	if tsVersion == "" {
		tsVersion = defaultTSVersion
	}
	t.Logf("testing against tailscale %s", tsVersion)

	c := &compose{t: t, env: append(os.Environ(), "TS_VERSION="+tsVersion)}
	t.Cleanup(func() {
		if t.Failed() {
			c.dumpLogs()
		}
		c.run(context.Background(), 5*time.Minute, "down", "--volumes", "--remove-orphans")
	})
	// A previous run that was killed mid-way would otherwise leave the
	// networks behind and make the address assignments fail.
	c.run(context.Background(), 5*time.Minute, "down", "--volumes", "--remove-orphans")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if out, err := c.run(ctx, 15*time.Minute, "up", "-d", "--build"); err != nil {
		t.Fatalf("docker compose up: %v\n%s", err, out)
	}
	c.wireNodeRouting()
	return c
}

// wireNodeRouting gives the node under test a route to the destination network
// via the router, then releases its entrypoint.
//
// The route is added from a throwaway container sharing that node's network
// namespace, rather than by the node itself, so the container keeps no
// capabilities of its own. That is the point: this design needs no privileges.
func (c *compose) wireNodeRouting() {
	c.t.Helper()
	name, err := c.containerName("minitail")
	if err != nil {
		c.t.Fatalf("finding the minitail container: %v", err)
	}
	out, err := exec.Command("docker", "run", "--rm",
		"--network=container:"+name, "--cap-add=NET_ADMIN", "alpine:3.20",
		"ip", "route", "add", destNetwork, "via", routerTransitIP).CombinedOutput()
	if err != nil {
		c.t.Fatalf("adding the route to %s: %v\n%s", destNetwork, err, out)
	}
	c.mustExec("minitail", "touch", "/baseline/ready")
}

// containerName resolves a compose service to its container name.
func (c *compose) containerName(service string) (string, error) {
	out, err := c.run(context.Background(), time.Minute, "ps", "-q", service)
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, out)
	}
	id := strings.TrimSpace(out)
	if id == "" {
		return "", fmt.Errorf("service %q has no running container", service)
	}
	// Several ids come back one per line if the service is scaled; take the first.
	if i := strings.IndexByte(id, '\n'); i >= 0 {
		id = id[:i]
	}
	return id, nil
}

// run invokes docker compose in the test's directory.
func (c *compose) run(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose"}, args...)...)
	cmd.Dir = "."
	cmd.Env = c.env
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

// exec runs a command inside a service container.
func (c *compose) exec(service string, args ...string) (string, error) {
	c.t.Helper()
	full := append([]string{"exec", "-T", service}, args...)
	out, err := c.run(context.Background(), 2*time.Minute, full...)
	return strings.TrimSpace(out), err
}

// mustExec fails the test if the command does not succeed.
func (c *compose) mustExec(service string, args ...string) string {
	c.t.Helper()
	out, err := c.exec(service, args...)
	if err != nil {
		c.t.Fatalf("%s: %v %v\n%s", service, args, err, out)
	}
	return out
}

func (c *compose) dumpLogs() {
	c.t.Helper()
	for _, svc := range []string{"headscale", "minitail", "client", "router"} {
		if out, err := c.run(context.Background(), time.Minute, "logs", "--tail=80", svc); err == nil {
			c.t.Logf("=== %s logs ===\n%s", svc, out)
		}
	}
	// minitail writes tailscaled's own output beside its state.
	if out, err := c.exec("minitail", "sh", "-c", "tail -n 120 /var/lib/minitail/tailscaled.log"); err == nil {
		c.t.Logf("=== tailscaled log ===\n%s", out)
	}
}

// headscale runs a headscale CLI command in the control server container.
func (c *compose) headscale(args ...string) (string, error) {
	return c.exec("headscale", append([]string{"headscale"}, args...)...)
}

func (c *compose) mustHeadscale(args ...string) string {
	c.t.Helper()
	out, err := c.headscale(args...)
	if err != nil {
		c.t.Fatalf("headscale %v: %v\n%s", args, err, out)
	}
	return out
}

// waitForHeadscale blocks until the control server answers CLI requests.
func (c *compose) waitForHeadscale() {
	c.t.Helper()
	c.eventually("headscale to become ready", 2*time.Minute, func() (bool, string) {
		out, err := c.headscale("users", "list", "-o", "json")
		return err == nil, out
	})
}

// eventually polls cond until it returns true or the timeout elapses,
// reporting the last observation on failure.
func (c *compose) eventually(what string, timeout time.Duration, cond func() (bool, string)) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		ok, detail := cond()
		if ok {
			return
		}
		last = detail
		time.Sleep(time.Second)
	}
	c.t.Fatalf("timed out after %v waiting for %s; last observation:\n%s", timeout, what, last)
}
