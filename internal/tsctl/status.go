// Package tsctl talks to an isolated tailscaled instance through the
// `tailscale` CLI, pointed at a private --socket.
package tsctl

import (
	"encoding/json"
	"fmt"
	"net/netip"
)

// Backend states reported by tailscaled in Status.BackendState. These mirror
// ipn.State.String() rather than importing tailscale.com, so that minitail
// stays a small module and keeps working across tailscaled versions.
const (
	BackendNoState          = "NoState"
	BackendNeedsLogin       = "NeedsLogin"
	BackendNeedsMachineAuth = "NeedsMachineAuth"
	BackendStopped          = "Stopped"
	BackendStarting         = "Starting"
	BackendRunning          = "Running"
)

// Status is the subset of `tailscale status --json` that minitail needs.
//
// It is deliberately a minimal, tolerant decode of ipnstate.Status: unknown
// fields are ignored, so a newer or older tailscaled does not break parsing.
type Status struct {
	Version string

	// TUN reports whether tailscaled created a kernel TUN device. In
	// userspace-networking mode tailscaled sets this to false, which is the
	// authoritative signal that no network interface was created.
	TUN bool

	BackendState string

	// AuthURL is non-empty while an interactive login is pending.
	AuthURL string

	TailscaleIPs []netip.Addr
	Self         *PeerStatus
	Health       []string

	CurrentTailnet *TailnetStatus
}

// PeerStatus is the subset of ipnstate.PeerStatus that minitail needs. It is
// used for the self node only.
type PeerStatus struct {
	ID       string
	HostName string
	DNSName  string
	Online   bool

	// ExitNodeOption is tailscaled's own "offered && approved" signal: it is
	// true only once the control plane has approved the exit node
	// advertisement, so it doubles as the not-yet-approved detector.
	ExitNodeOption bool

	TailscaleIPs []netip.Addr
	AllowedIPs   []netip.Prefix
}

// TailnetStatus identifies the tailnet the node belongs to.
type TailnetStatus struct {
	Name           string
	MagicDNSSuffix string
}

// ParseStatus decodes the output of `tailscale status --json`.
func ParseStatus(b []byte) (*Status, error) {
	var s Status
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parsing tailscale status: %w", err)
	}
	if s.BackendState == "" {
		return nil, fmt.Errorf("parsing tailscale status: no BackendState in %d bytes of JSON", len(b))
	}
	return &s, nil
}

// exitRoutes are the two prefixes a node must have approved to serve as an
// exit node.
var exitRoutes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/0"),
	netip.MustParsePrefix("::/0"),
}

// ExitNodeApproved reports whether the control plane has approved this node's
// exit node advertisement.
//
// ExitNodeOption is the primary signal. AllowedIPs is checked as a fallback
// for the (older) case where ExitNodeOption is not populated for the self
// node; tailscaled derives ExitNodeOption from exactly these prefixes.
func (s *Status) ExitNodeApproved() bool {
	if s == nil || s.Self == nil {
		return false
	}
	if s.Self.ExitNodeOption {
		return true
	}
	return containsExitRoutes(s.Self.AllowedIPs)
}

func containsExitRoutes(prefixes []netip.Prefix) bool {
	var v4, v6 bool
	for _, p := range prefixes {
		switch p {
		case exitRoutes[0]:
			v4 = true
		case exitRoutes[1]:
			v6 = true
		}
	}
	return v4 && v6
}

// FirstIPv4 returns the node's 100.x tailnet address, if it has one.
func (s *Status) FirstIPv4() string {
	if s == nil {
		return ""
	}
	ips := s.TailscaleIPs
	if len(ips) == 0 && s.Self != nil {
		ips = s.Self.TailscaleIPs
	}
	for _, ip := range ips {
		if ip.Is4() {
			return ip.String()
		}
	}
	if len(ips) > 0 {
		return ips[0].String()
	}
	return ""
}
