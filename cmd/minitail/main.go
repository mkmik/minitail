// Command minitail supervises an isolated, userspace-networking tailscaled
// that advertises subnet routes into a tailnet, and surfaces its state in the
// macOS menu bar.
//
// It creates no network interface, installs no routes, and changes no DNS
// configuration on this machine, so it can coexist with software that manages
// network state aggressively (corporate VPNs, container runtimes).
//
// minitail has no user interface for Tailscale's own options: what it passes
// to tailscaled and to `tailscale up` comes from a config file the user edits.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/mkmik/minitail/internal/app"
	"github.com/mkmik/minitail/internal/config"
	"github.com/mkmik/minitail/internal/desktop"
	"github.com/mkmik/minitail/internal/launchagent"
	"github.com/mkmik/minitail/internal/supervisor"
	"github.com/mkmik/minitail/internal/tsctl"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("minitail: ")

	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"run"}
	}
	cmd, rest := args[0], args[1:]

	var err error
	switch cmd {
	case "run":
		err = runCmd(rest)
	case "status":
		err = statusCmd(rest)
	case "config":
		err = configCmd(rest)
	case "service":
		err = serviceCmd(rest)
	case "version", "--version", "-version":
		fmt.Printf("minitail %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	case "help", "--help", "-h":
		usage(os.Stdout)
	default:
		usage(os.Stderr)
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `minitail - an isolated Tailscale subnet router for macOS

Usage:
  minitail run [flags]        supervise tailscaled and show the menu bar icon
  minitail status [flags]     print the current state
  minitail config path        print the config file's location
  minitail config init        create the config file if it does not exist
  minitail config show        print the commands the config file produces
  minitail service install    install and load the login LaunchAgent
  minitail service uninstall  unload and remove the LaunchAgent
  minitail service status     report whether the LaunchAgent is loaded
  minitail version            print the version

Tailscale's own options live in the config file, not in these flags.
Run 'minitail run -h' for minitail's flags.
`)
}

// runCmd starts the supervisor, the polling controller, the control socket,
// and (on macOS) the menu bar.
func runCmd(args []string) error {
	cfg := config.Default()
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfg.RegisterFlags(fs)
	headless := fs.Bool("headless", false, "do not show a menu bar icon (used by the integration tests)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := cfg.Resolve(); err != nil {
		return err
	}
	seeded, err := cfg.Load()
	if err != nil {
		return err
	}
	if seeded {
		log.Printf("wrote a default config to %s", cfg.Path)
		log.Printf("it advertises a placeholder route; edit it and restart minitail")
	}

	log.Printf("config %s", cfg.Path)
	log.Printf("using %s and %s", cfg.TailscaledPath, cfg.TailscalePath)
	log.Printf("tailscaled %s", strings.Join(cfg.TailscaledArgs(), " "))
	log.Printf("tailscale up %s", strings.Join(cfg.UpArgs(), " "))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ctrl, cleanup := newController(cfg)
	defer cleanup()

	go func() {
		if err := app.ServeStatus(ctx, ctrl, cfg.ControlSocket); err != nil {
			log.Printf("status socket: %v", err)
		}
	}()

	if *headless || runtime.GOOS != "darwin" {
		ctrl.Run(ctx)
		return nil
	}
	return runTray(ctx, cancel, ctrl, cfg.Path)
}

// newController wires the supervisor, the tailscale client, and the desktop
// integrations together.
func newController(cfg config.Config) (*app.Controller, func()) {
	out, err := os.OpenFile(cfg.TailscaledLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("cannot open %s, sending tailscaled output to stderr: %v", cfg.TailscaledLog, err)
		out = os.Stderr
	}

	var ctrl *app.Controller
	sup := supervisor.New(supervisor.Options{
		Path:   cfg.TailscaledPath,
		Args:   cfg.TailscaledArgs(),
		Stdout: out,
		Stderr: out,
		Logf:   func(f string, a ...any) { log.Printf("tailscaled: "+f, a...) },
		// A tailscaled restart invalidates the cached status, so nudge the
		// polling loop rather than waiting for the next tick.
		OnChange: func() {
			if ctrl != nil {
				ctrl.Poke()
			}
		},
	})

	ctrl = app.NewController(app.Options{
		Config: cfg,
		Daemon: sup,
		Tailscale: tsctl.New(tsctl.Options{
			Binary:     cfg.TailscalePath,
			GlobalArgs: cfg.TailscaleArgs(),
			UpArgs:     cfg.UpArgs(),
			Logf:       log.Printf,
		}),
		Notifier: app.NotifyFunc(desktop.Notify),
		Opener:   app.OpenFunc(desktop.Open),
		Logf:     log.Printf,
	})

	return ctrl, func() {
		if out != os.Stderr {
			_ = out.Close()
		}
	}
}

// statusCmd prints the state of a running minitail, falling back to querying
// tailscaled directly when no supervisor is running.
func statusCmd(args []string) error {
	cfg := config.Default()
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cfg.RegisterFlags(fs)
	asJSON := fs.Bool("json", false, "print the status as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Resolving binaries is only needed for the fallback path, so a missing
	// tailscale install must not stop `minitail status` from reporting.
	resolveErr := cfg.Resolve()
	if _, err := cfg.Load(); err != nil && resolveErr == nil {
		resolveErr = err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	view, err := app.FetchStatus(ctx, cfg.ControlSocket)
	if err != nil {
		if resolveErr != nil {
			return resolveErr
		}
		view, err = statusWithoutSupervisor(ctx, cfg)
		if err != nil {
			return err
		}
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(view)
	}
	fmt.Printf("%-12s %s\n", view.State, view.Summary)
	if view.Detail != "" {
		fmt.Printf("%-12s %s\n", "", view.Detail)
	}
	if view.TailnetIP != "" {
		fmt.Printf("%-12s %s\n", "address", view.TailnetIP)
	}
	for _, r := range view.Routes {
		note := ""
		if slices.Contains(view.PendingRoutes, r) {
			note = "  (awaiting approval)"
		}
		fmt.Printf("%-12s %s%s\n", "route", r, note)
	}
	if view.AuthURL != "" {
		fmt.Printf("%-12s %s\n", "login", view.AuthURL)
	}
	for _, h := range view.Health {
		fmt.Printf("%-12s %s\n", "health", h)
	}
	return nil
}

// statusWithoutSupervisor derives a view straight from tailscaled, for when
// minitail itself is not running.
func statusWithoutSupervisor(ctx context.Context, cfg config.Config) (app.View, error) {
	client := tsctl.New(tsctl.Options{Binary: cfg.TailscalePath, GlobalArgs: cfg.TailscaleArgs()})
	st, err := client.Status(ctx)
	if err != nil {
		return app.View{}, fmt.Errorf("minitail is not running and tailscaled is not reachable on %s: %w", cfg.Socket, err)
	}
	advertised, _ := cfg.File.AdvertisedRoutes()
	return app.Derive(app.Input{
		WantRunning:   true,
		DaemonRunning: true,
		Status:        st,
		Advertised:    advertised,
	}), nil
}

// configCmd exposes the config file, so that "where do I change the flags?"
// has an answer that does not involve reading the source.
func configCmd(args []string) error {
	cfg := config.Default()
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	cfg.RegisterFlags(fs)
	sub := "show"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	// The binaries are irrelevant here; only the paths matter.
	_ = cfg.Resolve()

	if sub == "path" {
		fmt.Println(cfg.Path)
		return nil
	}

	seeded, err := cfg.Load()
	if err != nil {
		return err
	}
	switch sub {
	case "init":
		if seeded {
			fmt.Printf("created %s\n", cfg.Path)
		} else {
			fmt.Printf("%s already exists\n", cfg.Path)
		}
		return nil
	case "show":
		if seeded {
			fmt.Printf("# created %s\n\n", cfg.Path)
		}
		fmt.Printf("tailscaled %s\n", strings.Join(cfg.TailscaledArgs(), " "))
		fmt.Printf("tailscale %s\n", strings.Join(cfg.TailscaleArgs(append([]string{"up"}, cfg.UpArgs()...)...), " "))
		routes, err := cfg.File.AdvertisedRoutes()
		if err != nil {
			return err
		}
		for _, r := range routes {
			fmt.Printf("advertising %s\n", r)
		}
		return nil
	default:
		return fmt.Errorf("unknown config command %q; expected path, init or show", sub)
	}
}

func serviceCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: minitail service install|uninstall|status")
	}
	if runtime.GOOS != "darwin" {
		return errors.New("minitail service is only supported on macOS")
	}
	switch args[0] {
	case "install":
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if exe, err = filepath.EvalSymlinks(exe); err != nil {
			return err
		}
		spec, err := launchagent.DefaultSpec(exe)
		if err != nil {
			return err
		}
		path, err := launchagent.Install(spec)
		if err != nil {
			return err
		}
		fmt.Printf("installed %s\n", path)
		fmt.Println("minitail will now start at login. Look for its icon in the menu bar;")
		fmt.Println("the first run opens a browser to log this node in to your tailnet.")
		return nil
	case "uninstall":
		existed, err := launchagent.Uninstall()
		if err != nil {
			return err
		}
		if existed {
			fmt.Println("removed the minitail LaunchAgent")
		} else {
			fmt.Println("no minitail LaunchAgent was installed")
		}
		return nil
	case "status":
		if launchagent.Loaded() {
			fmt.Println("loaded")
		} else {
			fmt.Println("not loaded")
		}
		return nil
	default:
		return fmt.Errorf("unknown service command %q", args[0])
	}
}
