#!/bin/sh
# A plain kernel-mode tailscaled, standing in for another device on the tailnet
# that wants to reach networks through minitail's node.
set -eu
mkdir -p /var/lib/tsclient /var/run/tailscale
exec tailscaled \
	--state=/var/lib/tsclient/tailscaled.state \
	--statedir=/var/lib/tsclient \
	--socket=/var/run/tailscale/client.sock \
	--tun=tailscale0 \
	--port=41641
