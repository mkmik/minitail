// Package icons holds minitail's menu bar artwork.
package icons

import (
	_ "embed"

	"github.com/mkmik/minitail/internal/app"
)

//go:generate go run gen.go

var (
	//go:embed stopped.png
	stopped []byte
	//go:embed starting.png
	starting []byte
	//go:embed attention.png
	attention []byte
	//go:embed partial.png
	partial []byte
	//go:embed active.png
	active []byte
)

// For returns the template icon for a state. The icons are macOS template
// images, so AppKit tints them for the current menu bar appearance.
func For(s app.State) []byte {
	switch s {
	case app.StateStopped:
		return stopped
	case app.StateNeedsLogin, app.StateNeedsMachineAuth, app.StateError:
		return attention
	case app.StateNotApproved, app.StateDown:
		return partial
	case app.StateServing:
		return active
	default:
		return starting
	}
}
