package launchagent_test

import (
	"encoding/xml"
	"strings"
	"testing"

	"github.com/mkmik/minitail/internal/launchagent"
)

func testSpec() launchagent.Spec {
	return launchagent.Spec{
		Program:    "/opt/homebrew/bin/minitail",
		Args:       []string{"run"},
		StdoutPath: "/Users/someone/Library/Logs/minitail/minitail.log",
		StderrPath: "/Users/someone/Library/Logs/minitail/minitail.log",
		PathEnv:    "/opt/homebrew/bin:/usr/bin:/bin",
	}
}

func TestPlistIsWellFormedXML(t *testing.T) {
	data, err := testSpec().Plist()
	if err != nil {
		t.Fatalf("Plist: %v", err)
	}
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	dec.Strict = true
	for {
		if _, err := dec.Token(); err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("plist is not well-formed XML: %v\n%s", err, data)
		}
	}
}

func TestPlistContents(t *testing.T) {
	data, err := testSpec().Plist()
	if err != nil {
		t.Fatalf("Plist: %v", err)
	}
	s := string(data)
	for _, want := range []string{
		"<key>Label</key>",
		"<string>" + launchagent.Label + "</string>",
		"<string>/opt/homebrew/bin/minitail</string>",
		"<string>run</string>",
		"<key>RunAtLoad</key>",
		// KeepAlive must be conditional: quitting from the menu bar exits
		// zero and has to stay quit, while a crash must be restarted.
		"<key>KeepAlive</key>",
		"<key>SuccessfulExit</key>",
		"<false/>",
		"<key>PATH</key>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plist is missing %q:\n%s", want, s)
		}
	}
	// A LaunchAgent, never a LaunchDaemon: notifications and opening a browser
	// need the user's GUI session.
	if strings.Contains(s, "LaunchDaemon") {
		t.Error("plist must describe a LaunchAgent")
	}
}

func TestPlistEscapesSpecialCharacters(t *testing.T) {
	spec := testSpec()
	spec.Program = `/Users/a&b/<minitail>`
	data, err := spec.Plist()
	if err != nil {
		t.Fatalf("Plist: %v", err)
	}
	s := string(data)
	if strings.Contains(s, "<minitail>") {
		t.Error("the program path was not XML-escaped")
	}
	if !strings.Contains(s, "&amp;") {
		t.Errorf("expected an escaped ampersand:\n%s", s)
	}
}

func TestPlistRequiresProgram(t *testing.T) {
	var spec launchagent.Spec
	if _, err := spec.Plist(); err == nil {
		t.Error("Plist() with no program succeeded, want an error")
	}
}
