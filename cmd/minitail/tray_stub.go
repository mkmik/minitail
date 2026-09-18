//go:build !darwin || nogui

package main

import (
	"context"

	"github.com/mkmik/minitail/internal/app"
)

// runTray has no menu bar to render to off macOS, so it just runs the
// controller. The integration tests exercise this path.
func runTray(ctx context.Context, _ context.CancelFunc, ctrl *app.Controller) error {
	ctrl.Run(ctx)
	return nil
}
