#!/bin/sh
# An IP router that also owns the test destination address.
#
# 192.0.2.40 lives on the router's loopback rather than on a Docker network:
# the exit node can only get there by following the static route the test rig
# installs, which is the point. httpd binds every address, so the same content
# is served on 192.0.2.40 (routed, must be reachable through the exit node) and
# on 198.51.100.2 (directly attached to the exit node, must not be).
set -eu

ip addr add 192.0.2.40/24 dev lo

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

exec httpd -f -p 8080 -h /srv
