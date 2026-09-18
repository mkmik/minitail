#!/bin/sh
# An IP router that also owns the test destination and a DNS server.
#
# Both addresses live on the router's loopback rather than on a Docker network:
# the subnet router can only get there by following the static route the test
# rig installs, which is the point. httpd binds every address, so the same
# content is served on 192.0.2.40 (routed) and on 198.51.100.2 (a LAN the
# subnet router is directly attached to).
set -eu

ip addr add 192.0.2.40/24 dev lo
ip addr add 192.0.2.53/32 dev lo

mkdir -p /srv/cgi-bin
echo reached-the-destination > /srv/index.html

# Reports the source address the destination actually sees, which is how the
# test distinguishes netstack's userspace proxying from NAT forwarding.
cat > /srv/cgi-bin/peer <<'CGI'
#!/bin/sh
echo "Content-Type: text/plain"
echo
echo "$REMOTE_ADDR"
CGI
chmod +x /srv/cgi-bin/peer

# The DNS server a split-DNS setup would point at. It is inside an advertised
# subnet, so queries to it travel through the subnet router over UDP/53.
dnsmasq \
	--listen-address=192.0.2.53 \
	--bind-interfaces \
	--no-resolv \
	--no-hosts \
	--address=/corp.internal/192.0.2.40 \
	--log-facility=-

exec httpd -f -p 8080 -h /srv
