#!/bin/bash
# Remove minitail: the LaunchAgent, the node's tailnet registration, the
# binary, and its state directory.
set -euo pipefail

PREFIX="${PREFIX:-$HOME/.local}"
BIN="${MINITAIL_BIN:-$PREFIX/bin/minitail}"
STATE_DIR="${MINITAIL_STATE_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/minitail}"
LOG_DIR="$HOME/Library/Logs/minitail"

if [[ ! -x "$BIN" ]] && command -v minitail >/dev/null 2>&1; then
	BIN="$(command -v minitail)"
fi

if [[ -x "$BIN" ]]; then
	"$BIN" service uninstall || true
else
	echo "minitail binary not found; unloading the LaunchAgent directly."
	launchctl bootout "gui/$(id -u)/com.github.mkmik.minitail" 2>/dev/null || true
	rm -f "$HOME/Library/LaunchAgents/com.github.mkmik.minitail.plist"
fi

# Remove the node from the tailnet while its state still exists. Without this
# it lingers in the admin console as an offline machine.
socket="$STATE_DIR/tailscaled.sock"
if [[ -S "$socket" ]] && command -v tailscale >/dev/null 2>&1; then
	echo "Logging this node out of the tailnet"
	tailscale --socket="$socket" logout || true
else
	echo "Note: tailscaled was not running, so the node could not be logged out."
	echo "Delete it by hand at https://login.tailscale.com/admin/machines"
fi

if [[ -d "$STATE_DIR" ]]; then
	echo "Removing $STATE_DIR"
	rm -rf "$STATE_DIR"
fi
rm -rf "$LOG_DIR"

if [[ -x "$BIN" && "$BIN" == "$PREFIX/bin/minitail" ]]; then
	echo "Removing $BIN"
	rm -f "$BIN"
elif [[ -x "$BIN" ]]; then
	echo "Leaving $BIN in place (not installed by scripts/install.sh)."
	echo "If you installed with Homebrew: brew services stop minitail && brew uninstall minitail"
fi

echo "Done."
