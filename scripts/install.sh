#!/bin/bash
# Install minitail for the current user, without Homebrew.
#
# Homebrew is the easier path:
#   brew tap mkmik/minitail https://github.com/mkmik/minitail
#   brew install minitail && brew services start minitail
#
# This script exists for people who would rather build from a checkout. It
# builds the binary, installs it, and registers the login LaunchAgent.
set -euo pipefail

PREFIX="${PREFIX:-$HOME/.local}"
BIN_DIR="$PREFIX/bin"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ "$(uname -s)" != "Darwin" ]]; then
	echo "minitail's menu bar and LaunchAgent are macOS-only." >&2
	exit 1
fi

if ! command -v go >/dev/null 2>&1; then
	echo "Go is required to build minitail. Install it with: brew install go" >&2
	exit 1
fi

if ! command -v tailscaled >/dev/null 2>&1 &&
	[[ ! -x /opt/homebrew/bin/tailscaled && ! -x /usr/local/bin/tailscaled ]]; then
	echo "The open source tailscaled was not found." >&2
	echo "Install it with: brew install tailscale" >&2
	echo "(That is the CLI package, not the Mac App Store app; the two coexist.)" >&2
	exit 1
fi

version="$(git -C "$REPO_ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)"

echo "Building minitail $version"
mkdir -p "$BIN_DIR"
(cd "$REPO_ROOT" && go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$BIN_DIR/minitail" ./cmd/minitail)

echo "Installed $BIN_DIR/minitail"
"$BIN_DIR/minitail" service install

cat <<MSG

minitail is running and will start again at login.

  Tailscale's own flags live in a config file, not in minitail's:

      $BIN_DIR/minitail config path

  The seeded file advertises a placeholder route (10.0.0.0/8). Edit it and
  restart minitail.

  Look for its icon in the menu bar. The first run opens a browser so you can
  log this node in to your tailnet; it then appears in the admin console as a
  separate machine, where you must approve its routes before any other device
  can use them:

      https://login.tailscale.com/admin/machines

  Check on it any time with:

      $BIN_DIR/minitail status

MSG

case ":$PATH:" in
*":$BIN_DIR:"*) ;;
*) echo "Note: $BIN_DIR is not on your PATH." ;;
esac
