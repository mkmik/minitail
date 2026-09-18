# minitail

A Tailscale **subnet router** for macOS that creates no network interface,
installs no routes, and changes no DNS configuration on the Mac itself — plus a
menu bar app to run it.

It exists for machines where something else already owns the network. A
corporate VPN client and a container runtime both install routes and both
reinstall them at unpredictable times; adding the stock Tailscale app as a
third participant can leave the routing table broken in a way that has to be
repaired by hand. minitail wants one thing from Tailscale — letting other
devices on your tailnet reach networks that only this machine can reach — and
gives up everything that would put it in that fight.

minitail has **no user interface for Tailscale's own options**. What it passes
to `tailscaled` and to `tailscale up` comes from a config file you edit.

## Install

```sh
brew tap mkmik/minitail https://github.com/mkmik/minitail
brew install minitail
brew services start minitail
```

That installs the open source `tailscale` package as a dependency (the CLI
package, not the Mac App Store app — the two coexist happily), and registers a
LaunchAgent so minitail starts at login.

### Updating

There is no tagged release yet, so the formula has only a `head` spec and
`brew install` builds the tip of the default branch. Homebrew does not check a
HEAD install against upstream unless you ask it to, so plain `brew upgrade`
will report nothing to do however far behind you are. To update:

```sh
brew update && brew upgrade --fetch-HEAD minitail
brew services restart minitail
```

(`brew reinstall minitail` also works — it always re-fetches — and the restart
is what makes the running supervisor pick up the new binary.)

To get ordinary versioned upgrades instead, tag a release. That publishes it
and rewrites the formula to build from the tarball, after which `brew upgrade`
behaves normally and `--fetch-HEAD` is no longer needed:

```sh
git tag -a v0.1.0 -m "minitail v0.1.0" && git push origin v0.1.0
```

Not a Homebrew user? `scripts/install.sh` builds from a checkout and installs
the same LaunchAgent. `scripts/uninstall.sh` reverses everything, including
removing the node from your tailnet.

## Configure

The first run writes `~/.config/minitail/minitail.conf` and tells you it did.
`minitail config path` prints its location; the menu bar has an **Edit
configuration…** item.

```ini
[tailscaled]
--tun=userspace-networking
--port=41642

[up]
--advertise-routes=10.0.0.0/8
--accept-routes=false
--accept-dns=false
--hostname=mbp-minitail
```

One command-line argument per line, `#` comments, two sections: `[tailscaled]`
is appended to the daemon's command line and `[up]` is passed to
`tailscale up`. Nothing is shell-parsed, so a value with spaces needs no
quoting. `minitail config show` prints the exact commands your file produces.

**`--advertise-routes=10.0.0.0/8` is a placeholder.** Replace it with the
networks you actually want reachable, comma-separated.

The only two flags minitail supplies itself are `--statedir` and `--socket`,
which point this instance at its own state and keep it separate from any
system-wide Tailscale install. Setting either in the config file is an error
rather than a silent override.

After editing, restart: `brew services restart minitail`, or Stop then Start in
the menu bar.

### First run, once

1. A browser opens so you can log this node in to your tailnet. The menu bar
   icon shows a login prompt too, if you miss it.
2. Approve its routes in the
   [admin console](https://login.tailscale.com/admin/machines). Until you do,
   minitail reports **"10.0.0.0/8 awaiting approval"** rather than pretending
   to work — no peer can use an unapproved route.
3. On your other devices, accept routes (`tailscale set --accept-routes`). A
   client that does not accept routes cannot see the advertisement at all.

### Split DNS

Point a **Split DNS** nameserver in the admin console at a DNS server whose IP
is inside one of your advertised routes. Queries then travel to it through this
Mac over UDP/53, which netstack forwards like any other traffic. There is an
integration test that does exactly this.

minitail passes `--accept-dns=false`, so it never touches *this* Mac's own
resolver — that stays whatever your VPN client set it to.

## How it works

minitail supervises its own `tailscaled`, restarting it if it dies, and adds:

```
tailscaled --statedir=~/.config/minitail/tailscaled \
           --socket=~/.config/minitail/tailscaled.sock \
           <your [tailscaled] flags>
```

**`--tun=userspace-networking`** is the point of the seeded default. In that
mode tailscaled implements its network stack in-process with gVisor's netstack
and never asks the OS for a TUN device, a route, or a DNS change.

Forwarding in this mode is a **proxy, not a NAT**: netstack terminates the
incoming flow in userspace and re-dials the destination from an ordinary host
socket. That is exactly what is wanted here — the outbound connection is
subject to whatever the routing table currently says, so whatever the VPN and
the container runtime have set up applies automatically, with no configuration
and no second copy of it inside Tailscale.

The separate `--statedir` and `--socket` keep this instance isolated from any
system Tailscale install. The consequence, accepted deliberately: this is a
**distinct node**. It has its own node key, needs its own login, and shows up
as a separate machine in the admin console, counting against your device limit.
It is the same user account, so no extra seat.

minitail needs **no root**. Userspace networking requires no interface, no
route, and no privileged port, so it runs as a plain LaunchAgent in your GUI
session — which is also what lets it show a menu bar icon, post notifications,
and open a browser for the first login. A LaunchDaemon could do none of that.

## What you give up

* **TCP** is well covered, and is what this is for.
* **UDP** is forwarded too, which is what makes DNS work. netstack even tries
  to reuse the original source port on the outbound socket, falling back to a
  random one.
* **ICMP echo** is relayed rather than forwarded: netstack answers pings to
  destinations behind the router by pinging them itself and synthesising the
  reply. On macOS it does this with an unprivileged ICMP socket, so it still
  needs no root. It is rate limited, and because the ping is re-originated, TTL
  and traceroute semantics are not preserved.
* **Throughput** is lower than kernel mode; gVisor's TCP is not the kernel's.
* **Path MTU discovery** behaves differently from a kernel forwarding path.
* **Source ports and addresses** seen by the remote end come from this Mac's
  socket layer, not from the originating device. A host behind the route sees
  this Mac's address, so per-device firewall rules on the far side will not
  distinguish your tailnet peers.
* **Connection state** lives in this Mac's socket table, so long-lived
  connections drop if the daemon restarts.

### Your ACLs must permit the traffic

Advertising and approving a route is not enough on its own: the tailnet policy
has to allow peers to send traffic to it, or this node's own packet filter
drops the flow and the client reports `rejected due to acl`. Tailscale's
default policy allows everything; a hand-written one may not.

### If you want an exit node instead

Add `--advertise-exit-node` to `[up]`. It works like any other advertised
route, and minitail's approval reporting covers it.

One caveat, and the reason this project advertises subnets by default: a
Tailscale exit node deliberately refuses to forward to subnets configured
directly on one of its own interfaces — it is "guest wifi", so peers get
internet access, not LAN access. (In Tailscale's source this is
`ipnlocal.shrinkDefaultRoute`.) So a container runtime's bridge, or your home
LAN, is **not** reachable through an exit node no matter what the ACLs say.
Advertising it as a subnet route is the way to reach it, and the integration
suite asserts both halves of that contrast.

## Using it

The menu bar icon shows the state; the menu has Start, Stop, Re-authenticate,
Open admin console, Edit configuration…, Copy tailnet IP, and Quit. Quitting
from the menu stays quit (the LaunchAgent only restarts minitail if it exits
non-zero).

From a terminal:

```sh
minitail status            # what it is doing right now, and which routes are pending
minitail status --json     # the same, for scripts
minitail config show       # the exact commands your config file produces
minitail service status    # is the LaunchAgent loaded?
minitail run -h            # minitail's own flags (Tailscale's live in the config)
```

`minitail status` talks to the running supervisor over a unix socket, and falls
back to querying tailscaled directly if minitail is not running.

`tailscaled`'s own output is in `~/.config/minitail/tailscaled.log`. minitail's
own log depends on how you started it: `$(brew --prefix)/var/log/minitail.log`
under `brew services`, or `~/Library/Logs/minitail/minitail.log` under the
LaunchAgent that `scripts/install.sh` and `minitail service install` write.

## Testing

```sh
go test ./...                                              # unit tests
cd test/integration && go test -tags integration ./...     # needs Docker
```

The unit tests cover the config file parser and the state machine, the latter
against recorded `tailscale status --json` fixtures: stopped, needs-login,
connecting, running-with-routes-unapproved, running-approved, and an exit node.
All the logic lives there, behind an interface; the systray layer is a thin
adapter that renders state and forwards clicks, and is not tested.

The integration suite runs against **Headscale**, the open source
implementation of the Tailscale control server, so it needs no Tailscale
credentials, no OAuth client and no seats. It stands up a control server, a
minitail subnet router, an ordinary kernel-mode Tailscale client, and a router
hosting a web server and a DNS server on networks the client cannot reach, and
asserts that:

* a fresh install seeds a usable config file with the placeholder route;
* the daemon registers using an isolated `--statedir` and `--socket`, leaving a
  decoy system-wide state file untouched;
* it comes up in userspace mode and adds **no interface and no route** —
  compared against a snapshot taken immediately before it started;
* the control plane sees both advertisements, minitail reports them as pending,
  approving one of two still reads as pending, and approving both flips it to
  serving;
* a second device completes a **TCP** connection to a network it cannot reach
  directly, and the destination sees the router's own source address,
  confirming traffic goes out through a host socket;
* the same device reaches a LAN the router is **directly attached to** — the
  case an exit node refuses;
* the same device **resolves a name** through a DNS server inside an advertised
  subnet, which is the split-DNS path;
* killing `tailscaled` gets it restarted and back to serving.

CI runs this on Linux against the current stable `tailscaled` and one prior
release, and separately builds and packages on macOS. The macOS job makes no
networking assertions: GitHub's runners are a poor proxy for a laptop running a
corporate VPN and a container runtime.

### What CI cannot cover

The motivating requirement — that this daemon does **not corrupt the macOS
routing table** when the VPN and the container runtime restart — cannot be
tested in CI. It stays a manual acceptance check on the real machine:

1. `netstat -rn > /tmp/before.txt` before starting minitail.
2. Start it and let its routes be approved.
3. `ifconfig` shows no new interface, and `netstat -rn` still matches
   `/tmp/before.txt`.
4. Run the restart sequence that used to break things, and confirm the routing
   table survives.
5. Reboot, and confirm the node comes back without interaction.

CI's job is to prove the routing works and the supervisor behaves. The
non-interference property is verified by hand.

## License

MIT
