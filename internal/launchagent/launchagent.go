// Package launchagent writes and controls the macOS LaunchAgent that starts
// minitail at login.
//
// This is deliberately a LaunchAgent and not a LaunchDaemon: an agent runs
// inside the user's GUI session, which is what makes a menu bar icon, desktop
// notifications, and opening a browser for the first login possible. A daemon
// has no GUI session and could not prompt for login at all.
package launchagent

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Label is the launchd job label, and the plist's file name stem.
const Label = "com.github.mkmik.minitail"

// Spec describes the agent to write.
type Spec struct {
	// Program is the absolute path to the minitail binary.
	Program string
	// Args are the arguments after the program name.
	Args []string
	// StdoutPath and StderrPath receive the agent's output.
	StdoutPath string
	StderrPath string
	// PathEnv is the PATH given to the agent. launchd starts jobs with a
	// minimal PATH that contains neither Homebrew prefix, so tailscaled would
	// not be found without this.
	PathEnv string
}

// DefaultSpec returns the spec for the given minitail binary.
func DefaultSpec(program string) (Spec, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Spec{}, err
	}
	logDir := filepath.Join(home, "Library", "Logs", "minitail")
	return Spec{
		Program:    program,
		Args:       []string{"run"},
		StdoutPath: filepath.Join(logDir, "minitail.log"),
		StderrPath: filepath.Join(logDir, "minitail.log"),
		PathEnv:    "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
	}, nil
}

// PlistPath returns the location of the LaunchAgent plist for the current user.
func PlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
}

// Plist renders the LaunchAgent property list.
//
// KeepAlive is conditional on SuccessfulExit=false rather than unconditional:
// quitting from the menu bar exits zero and must stay quit, while a crash
// exits non-zero and must be restarted.
func (s Spec) Plist() ([]byte, error) {
	if s.Program == "" {
		return nil, fmt.Errorf("launchagent: program path is required")
	}
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	writeKeyString(&b, "Label", Label)

	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range append([]string{s.Program}, s.Args...) {
		b.WriteString("\t\t<string>" + escape(a) + "</string>\n")
	}
	b.WriteString("\t</array>\n")

	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	// Interactive keeps launchd from throttling a job that belongs to the GUI.
	writeKeyString(&b, "ProcessType", "Interactive")
	if s.StdoutPath != "" {
		writeKeyString(&b, "StandardOutPath", s.StdoutPath)
	}
	if s.StderrPath != "" {
		writeKeyString(&b, "StandardErrorPath", s.StderrPath)
	}
	if s.PathEnv != "" {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		b.WriteString("\t\t<key>PATH</key>\n\t\t<string>" + escape(s.PathEnv) + "</string>\n")
		b.WriteString("\t</dict>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String()), nil
}

func writeKeyString(b *strings.Builder, key, value string) {
	b.WriteString("\t<key>" + escape(key) + "</key>\n\t<string>" + escape(value) + "</string>\n")
}

func escape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Install writes the plist and bootstraps the agent into the current GUI
// session, replacing any previous version.
func Install(s Spec) (string, error) {
	path, err := PlistPath()
	if err != nil {
		return "", err
	}
	data, err := s.Plist()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	for _, p := range []string{s.StdoutPath, s.StderrPath} {
		if p == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	// Ignore the bootout error: it fails when nothing was loaded, which is the
	// normal case for a first install.
	_ = run("launchctl", "bootout", domainTarget())
	if err := run("launchctl", "bootstrap", domain(), path); err != nil {
		// Older macOS releases only understand load/unload.
		if err2 := run("launchctl", "load", "-w", path); err2 != nil {
			return path, fmt.Errorf("bootstrapping agent: %w (and load: %v)", err, err2)
		}
	}
	return path, nil
}

// Uninstall unloads the agent and removes its plist. It reports whether a
// plist was present.
func Uninstall() (bool, error) {
	path, err := PlistPath()
	if err != nil {
		return false, err
	}
	_, statErr := os.Stat(path)
	existed := statErr == nil

	if err := run("launchctl", "bootout", domainTarget()); err != nil {
		_ = run("launchctl", "unload", "-w", path)
	}
	if existed {
		if err := os.Remove(path); err != nil {
			return true, err
		}
	}
	return existed, nil
}

// Loaded reports whether launchd currently knows about the agent.
func Loaded() bool {
	return run("launchctl", "print", domainTarget()) == nil
}

func domain() string       { return "gui/" + strconv.Itoa(os.Getuid()) }
func domainTarget() string { return domain() + "/" + Label }

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Run()
}
