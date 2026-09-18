# Homebrew formula for minitail.
#
# This repository doubles as its own tap:
#
#   brew tap mkmik/minitail https://github.com/mkmik/minitail
#   brew install minitail
#   brew services start minitail
class Minitail < Formula
  desc "Isolated Tailscale subnet router for macOS that touches no routes or DNS"
  homepage "https://github.com/mkmik/minitail"
  license "MIT"
  # No tagged release yet, so this builds the tip of the default branch.
  # Pushing a v* tag runs .github/workflows/release.yml, which adds a stable
  # url and sha256 here and makes `brew install` use the release instead.
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

      This is a HEAD install, which `brew upgrade` skips by default. Update with:
        brew update && brew upgrade --fetch-HEAD minitail && brew services restart minitail

      Tailscale's own flags live in a config file, not in minitail's:
        minitail config path

      The seeded file advertises a placeholder route (10.0.0.0/8). Edit it,
      then restart. The first run opens a browser so you can log this node in
      to your tailnet; it then appears in the admin console as a separate
      machine, where you must approve its routes before peers can use them:
        https://login.tailscale.com/admin/machines
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/minitail version")
    assert_match "subnet router", shell_output("#{bin}/minitail help")
    # The config file is the interface, so it must be creatable unattended.
    system bin/"minitail", "config", "init", "-dir", testpath/"cfg"
    assert_match "--advertise-routes=10.0.0.0/8", (testpath/"cfg/minitail.conf").read
  end
end
