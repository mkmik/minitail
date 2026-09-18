# Homebrew formula for minitail.
#
# This repository doubles as its own tap:
#
#   brew tap mkmik/minitail https://github.com/mkmik/minitail
#   brew install minitail
#   brew services start minitail
class Minitail < Formula
  desc "Isolated Tailscale exit node for macOS that touches no routes or DNS"
  homepage "https://github.com/mkmik/minitail"
  url "https://github.com/mkmik/minitail/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"
  license "MIT"
  head "https://github.com/mkmik/minitail.git"

  # "tailscale" is the open source tailscaled and CLI. minitail supervises its
  # own tailscaled with a private state directory, socket and port, so it
  # coexists with the stock Tailscale app rather than replacing it.
  depends_on "go" => :build
  depends_on :macos
  depends_on "tailscale"

  def install
    system "go", "build", *std_go_args(ldflags: "-s -w -X main.version=#{version}"), "./cmd/minitail"
  end

  service do
    run [opt_bin/"minitail", "run"]
    keep_alive successful_exit: false
    # A LaunchAgent, not a LaunchDaemon: the menu bar icon, notifications and
    # opening a browser for the first login all need the user's GUI session.
    require_root false
    environment_variables PATH: std_service_path_env
    log_path var/"log/minitail.log"
    error_log_path var/"log/minitail.log"
  end

  def caveats
    <<~EOS
      Start minitail and have it come back after a reboot:
        brew services start minitail

      The first run opens a browser so you can log this node in to your tailnet.
      It then appears in the admin console as a separate machine; you must
      approve its exit node advertisement there before other devices can use it:
        https://login.tailscale.com/admin/machines
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/minitail version")
    assert_match "isolated Tailscale exit node", shell_output("#{bin}/minitail help")
  end
end
