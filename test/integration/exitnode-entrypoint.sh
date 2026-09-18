#!/bin/sh
# Wait until the test rig has finished wiring this container's network, then
# snapshot the network state and start minitail.
#
# The snapshot is the evidence for this project's central claim: starting the
# exit node must add no interface and change no route. Taking it after the rig
# is done means any difference is attributable to minitail alone.
set -eu

mkdir -p /baseline
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
