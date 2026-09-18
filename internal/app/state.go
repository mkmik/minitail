// Package app holds minitail's logic: a pure state machine that turns
// tailscaled's status into something a menu bar can render, plus the
// controller that drives the daemon. Neither depends on the systray.
package app

import (
	"strings"

	"github.com/mkmik/minitail/internal/tsctl"
)

// State is the coarse condition of the exit node, as shown by the menu bar icon.
type State string

const (
	// StateStopped means the user has not asked for the daemon to run.
	StateStopped State = "stopped"
	// StateStarting means tailscaled is launching, or has not answered yet.
	StateStarting State = "starting"
	// StateNeedsLogin means an interactive login is required.
	StateNeedsLogin State = "needs-login"
	// StateNeedsMachineAuth means the tailnet admin must approve the device.
	StateNeedsMachineAuth State = "needs-machine-auth"
	// StateConnecting means the node is logged in and coming up.
	StateConnecting State = "connecting"
	// StateDown means tailscaled is up but the node has been brought down.
	StateDown State = "down"
	// StateNotApproved means the node is connected and advertising itself as
	// an exit node, but the advertisement has not been approved in the admin
	// console, so no other device can select it.
	StateNotApproved State = "not-approved"
	// StateExitNode is the healthy state: connected and serving as an exit node.
	StateExitNode State = "exit-node"
	// StateError means tailscaled could not be started or kept running.
	StateError State = "error"
)

// Healthy reports whether s is the fully working state.
func (s State) Healthy() bool { return s == StateExitNode }

// NeedsAttention reports whether s is something the user has to act on.
func (s State) NeedsAttention() bool {
	switch s {
	case StateNeedsLogin, StateNeedsMachineAuth, StateNotApproved, StateError:
		return true
	}
	return false
}

// Input is everything Derive needs. It is a plain struct so tests can build
// any situation without a running daemon.
type Input struct {
	// WantRunning is the user's intent, toggled by the Start/Stop menu items.
	WantRunning bool
	// DaemonRunning reports whether the supervised tailscaled process is alive.
	DaemonRunning bool
	// Restarts counts unexpected tailscaled exits since minitail started.
	Restarts int
	// GivenUp reports that the supervisor has stopped trying to restart.
	GivenUp bool
	// Status is the last successful status poll, or nil if there is none.
	Status *tsctl.Status
	// StatusErr is the error from the last status poll, if it failed.
	StatusErr error
	// DaemonErr is the last error from the supervisor.
	DaemonErr error
}

// View is the rendered state: everything the menu bar needs, and nothing that
// depends on it.
type View struct {
	State State
	// Summary is the one-line status shown at the top of the menu.
	Summary string
	// Detail explains what the user should do, when there is something to do.
	Detail string
	// AuthURL is the login URL, when a login is pending.
	AuthURL string
	// TailnetIP is the node's 100.x address, when it has one.
	TailnetIP string
	// TailnetName is the tailnet this node belongs to.
	TailnetName string
	// Hostname is the name this node registered under.
	Hostname string
	// Health carries tailscaled's own health warnings.
	Health []string
	// Userspace reports that tailscaled created no TUN device. It is false
	// until the first successful status poll.
	Userspace bool
	// Restarts mirrors Input.Restarts.
	Restarts int

	CanStart bool
	CanStop  bool
}

// adminConsoleURL is where exit node advertisements are approved.
const adminConsoleURL = "https://login.tailscale.com/admin/machines"

// Derive maps an Input onto a View. It is pure: no I/O, no clock, no globals.
func Derive(in Input) View {
	v := View{
		State:    StateStarting,
		Restarts: in.Restarts,
		CanStart: !in.WantRunning,
		CanStop:  in.WantRunning,
	}
	if st := in.Status; st != nil {
		v.Userspace = !st.TUN
		v.TailnetIP = st.FirstIPv4()
		v.Health = st.Health
		if st.Self != nil {
			v.Hostname = st.Self.HostName
		}
		if st.CurrentTailnet != nil {
			v.TailnetName = st.CurrentTailnet.Name
		}
	}

	switch {
	case !in.WantRunning:
		v.State = StateStopped
		v.Summary = "Stopped"
		v.Detail = "The exit node is not running."
		return v

	case in.GivenUp:
		v.State = StateError
		v.Summary = "Failed to start"
		v.Detail = errDetail("tailscaled could not be kept running", in.DaemonErr)
		return v

	case !in.DaemonRunning:
		v.State = StateStarting
		v.Summary = "Starting tailscaled…"
		if in.Restarts > 0 {
			v.Detail = "Restarting after an unexpected exit."
		}
		return v

	case in.Status == nil:
		v.State = StateStarting
		v.Summary = "Waiting for tailscaled…"
		v.Detail = errDetail("", in.StatusErr)
		return v
	}

	st := in.Status
	switch st.BackendState {
	case tsctl.BackendNeedsLogin:
		v.State = StateNeedsLogin
		v.Summary = "Login required"
		v.AuthURL = st.AuthURL
		if st.AuthURL != "" {
			v.Detail = "Open the login page to authenticate this node."
		} else {
			v.Detail = "Waiting for a login URL…"
		}

	case tsctl.BackendNeedsMachineAuth:
		v.State = StateNeedsMachineAuth
		v.Summary = "Waiting for device approval"
		v.Detail = "Approve this machine in the Tailscale admin console."

	case tsctl.BackendNoState, tsctl.BackendStarting:
		v.State = StateConnecting
		v.Summary = "Connecting…"

	case tsctl.BackendStopped:
		v.State = StateDown
		v.Summary = "Logged out"
		v.Detail = "tailscaled is running but the node is down."

	case tsctl.BackendRunning:
		if st.ExitNodeApproved() {
			v.State = StateExitNode
			v.Summary = "Exit node active"
			if v.TailnetIP != "" {
				v.Detail = "Serving tailnet traffic as " + v.TailnetIP + "."
			}
		} else {
			// The node is connected and advertising, but the control plane has
			// not approved the advertisement. Without this branch the app would
			// look healthy while no peer could actually select it.
			v.State = StateNotApproved
			v.Summary = "Exit node not approved"
			v.Detail = "Connected, but the exit node advertisement still needs approval in the admin console."
		}

	default:
		v.State = StateConnecting
		v.Summary = "Connecting…"
		v.Detail = "tailscaled reports an unknown state: " + st.BackendState
	}
	return v
}

// AdminURL returns the admin console page for this view.
func (v View) AdminURL() string { return adminConsoleURL }

// Title is a compact description suitable for a notification title.
func (v View) Title() string {
	if v.Summary == "" {
		return "minitail"
	}
	return "minitail: " + v.Summary
}

func errDetail(prefix string, err error) string {
	if err == nil {
		return prefix
	}
	msg := strings.TrimSpace(err.Error())
	if prefix == "" {
		return msg
	}
	return prefix + ": " + msg
}
