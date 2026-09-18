//go:build !darwin

// Package desktop wraps the few macOS user-session integrations minitail
// needs. On other platforms (where minitail runs headless, as it does in the
// integration tests) they are no-ops.
package desktop

import "log"

// Notify logs the notification instead of posting one.
func Notify(title, body string) error {
	log.Printf("notification: %s: %s", title, body)
	return nil
}

// Open logs the URL instead of opening a browser.
func Open(url string) error {
	log.Printf("open url: %s", url)
	return nil
}

// CopyToClipboard is not supported off macOS.
func CopyToClipboard(string) error { return nil }
