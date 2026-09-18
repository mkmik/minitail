# minitail

A Tailscale **exit node** for macOS that creates no network interface, installs
no routes, and changes no DNS configuration — plus a menu bar app to run it.

It exists for machines where something else already owns the network. A
corporate VPN client and a container runtime both install routes and both
reinstall them at unpredictable times; adding the stock Tailscale app as a
third participant can leave the routing table broken in a way that has to be
repaired by hand. minitail wants only one thing from Tailscale — letting other
devices on your tailnet egress through this machine — and gives up everything
that would put it in that fight.

## Install

```sh
brew tap mkmik/minitail https://github.com/mkmik/minitail
brew install minitail
brew services start minitail
```

That installs the open source `tailscale` package as a dependency (the CLI
package, not the Mac App Store app — the two coexist happily), and registers a
LaunchAgent so minitail starts at login.

There is no tagged release yet, so the formula builds the tip of the default
branch. Pushing a `v*` tag publishes a release and rewrites the formula to
build that instead:

```sh
git tag -a v0.1.0 -m "minitail v0.1.0" && git push origin v0.1.0
```

Then, once:

1. A browser opens so you can log this node in to your tailnet. The menu bar
   icon shows a login prompt too, if you miss it.
2. Approve its exit node advertisement in the
   [admin console](https://login.tailscale.com/admin/machines). Until you do,
   minitail reports **"Exit node not approved"** rather than pretending to
   work — no other device can select it yet.
3. On your other devices, pick this machine as the exit node.

Not a Homebrew user? `scripts/install.sh` builds from a checkout and installs
the same LaunchAgent. `scripts/uninstall.sh` reverses everything, including
removing the node from your tailnet.

## How it works

minitail supervises its own `tailscaled` with:

```
tailscaled --tun=userspace-networking \
           --statedir=~/.config/minitail \
           --socket=~/.config/minitail/tailscaled.sock \
           --port=41642
```

**`--tun=userspace-networking`** is the whole design. In that mode tailscaled
implements its network stack in-process with gVisor's netstack and never asks
the OS for a TUN device, a route, or a DNS change. Tailscale's own docs note it
is the only way to run an exit node on some operating systems.

Egress in this mode is a **proxy, not a NAT**: netstack terminates the incoming
flow in userspace and re-dials the destination from an ordinary host socket.
That is exactly what is wanted here — the outbound connection is subject to
whatever the routing table currently says, so whatever the VPN and the
container runtime have set up applies automatically, with no configuration.

The separate `--statedir`, `--socket` and `--port` keep this instance isolated
from any system Tailscale install. The consequence, accepted deliberately: this
is a **distinct node**. It has its own node key, needs its own login, and shows
up as a separate machine in the admin console, counting against your device
limit. It is the same user account, so no extra seat.

`minitail` needs **no root**. Userspace networking requires no interface, no
route, and no privileged port, so it runs as a plain LaunchAgent in your GUI
session — which is also what lets it show a menu bar icon, post notifications,
and open a browser for the first login. A LaunchDaemon could do none of that.

## What you give up

These are the real trade-offs of netstack mode. The first one is the one that
tends to surprise people.

### An exit node does not reach its own directly-attached LANs

A Tailscale exit node deliberately refuses to forward traffic to subnets that
are configured directly on one of its own interfaces. It is "guest wifi": peers
get internet access, not LAN access. (In Tailscale's source this is
`ipnlocal.shrinkDefaultRoute`; the node's own addresses are added back, the
surrounding subnets are not.)

So:

* Networks the Mac reaches **through a gateway** — corporate subnets behind a
  VPN tunnel, anything past your router — work through the exit node. This is
  the common case and it needs nothing extra.
* Networks the Mac is **directly attached to** — a container runtime's bridge,
  your home LAN — do not, no matter what the ACLs say.

If you need those container IPs reachable from the tailnet, advertise them
explicitly as subnet routes, which makes this node a subnet router as well as
an exit node:

```sh
minitail run --advertise-routes=198.19.0.0/16
```

and approve those routes in the admin console too. It is off by default.

### Your ACLs must permit exit node egress

Routing a device at an approved exit node is not enough on its own: the tailnet
policy has to grant `autogroup:internet`, or the exit node's own packet filter
drops the flow and the client reports `rejected due to acl`. Tailscale's default
policy includes this; a hand-written one may not.

### Protocol coverage, throughput, and connection state

* **TCP** is well covered, and is what this is for.
* **UDP** is forwarded too. netstack even tries to reuse the original source
  port on the outbound socket, falling back to a random one.
* **ICMP echo** is relayed rather than forwarded: netstack answers pings to
  destinations behind the exit node by pinging them itself and synthesising the
  reply. On macOS it does this with an unprivileged ICMP socket, so it still
  needs no root. It is rate limited, and because the ping is re-originated, TTL
  and traceroute semantics are not preserved.
* **Throughput** is lower than kernel mode; gVisor's TCP is not the kernel's.
* **Path MTU discovery** behaves differently from a kernel forwarding path.
* **Source ports and addresses** seen by the remote end come from this Mac's
  socket layer, not from the originating device.
* **Connection state** lives in this Mac's socket table, so long-lived
  connections drop if the daemon restarts.

### Out of scope

DNS and MagicDNS: minitail passes `--accept-dns=false` and never touches the
resolver. Everything works at the IP level.

## Using it

The menu bar icon shows the state; the menu has Start, Stop, Re-authenticate,
Open admin console, Copy tailnet IP, and Quit. Quitting from the menu stays
quit (the LaunchAgent only restarts minitail if it exits non-zero).

From a terminal:

```sh
minitail status            # what it is doing right now
minitail status --json     # the same, for scripts
minitail service status    # is the LaunchAgent loaded?
minitail run -h            # every flag
```

`minitail status` talks to the running supervisor over a unix socket, and falls
back to querying tailscaled directly if minitail is not running.

`tailscaled`'s own output is always in `~/.config/minitail/tailscaled.log`.
minitail's own log depends on how you started it: `$(brew --prefix)/var/log/minitail.log`
under `brew services`, or `~/Library/Logs/minitail/minitail.log` under the
LaunchAgent that `scripts/install.sh` and `minitail service install` write.

## Testing

```sh
go test ./...                                              # unit tests
cd test/integration && go test -tags integration ./...     # needs Docker
```

The unit tests cover the state machine against recorded
`tailscale status --json` fixtures: stopped, needs-login, connecting,
running-but-not-approved, and running-as-exit-node. All the logic lives there,
behind an interface; the systray layer is a thin adapter that renders state and
forwards clicks, and is not tested.

The integration suite runs against **Headscale**, the open source
implementation of the Tailscale control server, so it needs no Tailscale
credentials, no OAuth client and no seats. It stands up a control server, a
minitail exit node, an ordinary kernel-mode Tailscale client, and a destination
reachable only by routing, and asserts that:

* the daemon registers using an isolated `--statedir` and `--socket`, leaving a
  decoy system-wide state file untouched;
* it comes up in userspace mode and adds **no interface and no route** —
  compared against a snapshot taken immediately before it started;
* the control plane sees the exit node advertisement, minitail reports the
  un-approved state, and approval flips it to serving;
* a second device completes a **TCP** connection to a destination it cannot
  reach directly, and the destination sees the exit node's own source address,
  confirming egress goes out through a host socket;
* a directly attached LAN on the exit node stays unreachable, pinning the
  "guest wifi" behaviour described above;
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
2. Start it and let it come up as an exit node.
3. `ifconfig` shows no new interface, and `netstat -rn` still matches
   `/tmp/before.txt`.
4. Run the restart sequence that used to break things, and confirm the routing
   table survives.
5. Reboot, and confirm the exit node comes back without interaction.

CI's job is to prove the exit node works and the supervisor behaves. The
non-interference property is verified by hand.

## License

MIT
