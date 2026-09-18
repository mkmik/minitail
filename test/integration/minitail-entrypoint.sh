#!/bin/sh
# Write minitail's config file, snapshot the network, then start minitail.
#
# The config file is how minitail is configured — it has no flags for
# Tailscale's own options — so the test rig writes one exactly as a user would.
#
# The snapshot is the evidence for this project's central claim: starting the
# subnet router must add no interface and change no route. Taking it after the
# rig has finished wiring the container means any difference is attributable to
# minitail alone.
set -eu

mkdir -p /baseline /var/lib/minitail
cat > /var/lib/minitail/minitail.conf <<'CONF'
[tailscaled]
--tun=userspace-networking
--port=41642

[up]
# 192.0.2.0/24 is reachable from this node only through a static route, and
# 198.51.100.0/24 is a LAN this node is directly attached to. A subnet router
# serves both; an exit node would refuse the second one.
--advertise-routes=192.0.2.0/24,198.51.100.0/24
--accept-routes=false
--accept-dns=false
--hostname=minitail-test
--login-server=http://headscale:8080
CONF

echo "waiting for the test rig to finish network setup"
while [ ! -f /baseline/ready ]; do sleep 1; done

ip -o link show | awk '{print $2}' | sort > /baseline/links.txt
ip route show | sort > /baseline/routes4.txt
ip -6 route show | sort > /baseline/routes6.txt

# A decoy system-wide Tailscale state file. minitail points tailscaled at its
# own --statedir, so this must still be byte-for-byte identical afterwards.
mkdir -p /var/lib/tailscale
printf 'decoy-system-tailscale-state\n' > /var/lib/tailscale/tailscaled.state
cp /var/lib/tailscale/tailscaled.state /baseline/decoy.state

exec minitail run --headless "$@"
