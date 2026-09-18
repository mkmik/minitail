// Package desktop wraps the few macOS user-session integrations minitail
// needs: a notification, a browser, and the clipboard. Each is a tiny exec
// wrapper rather than a cgo binding, and each is injected into the controller
// so tests never need a GUI session.
package desktop

import (
	"fmt"
	"os/exec"
	"strings"
)

// Notify posts a macOS notification.
func Notify(title, body string) error {
	script := fmt.Sprintf("display notification %s with title %s", quote(body), quote(title))
	return exec.Command("osascript", "-e", script).Run()
}

// Open opens a URL in the user's default browser.
func Open(url string) error {
	return exec.Command("open", url).Run()
}

// CopyToClipboard puts s on the pasteboard.
func CopyToClipboard(s string) error {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(s)
	return cmd.Run()
}

// quote renders s as an AppleScript string literal.
func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}
