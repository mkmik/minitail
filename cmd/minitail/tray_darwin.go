//go:build darwin && !nogui

package main

import (
	"context"
	"log"

	"fyne.io/systray"

	"github.com/mkmik/minitail/internal/app"
	"github.com/mkmik/minitail/internal/desktop"
	"github.com/mkmik/minitail/internal/icons"
)

// runTray renders the controller's views into the menu bar and forwards
// clicks back to it. It is deliberately dumb: it holds no state of its own
// and makes no decisions, so reading it is sufficient review.
func runTray(ctx context.Context, cancel context.CancelFunc, ctrl *app.Controller, configPath string) error {
	ctx, stopCtrl := context.WithCancel(ctx)

	onReady := func() {
		systray.SetTooltip("minitail")

		mStatus := systray.AddMenuItem("Starting…", "")
		mStatus.Disable()
		mDetail := systray.AddMenuItem("", "")
		mDetail.Disable()
		mDetail.Hide()
		systray.AddSeparator()

		mLogin := systray.AddMenuItem("Open login page", "Authenticate this node with Tailscale")
		mLogin.Hide()
		mApprove := systray.AddMenuItem("Approve routes…", "Open the admin console to approve the advertised routes")
		mApprove.Hide()
		mCopyIP := systray.AddMenuItem("Copy tailnet IP", "Copy this node's tailnet address")
		mCopyIP.Hide()
		systray.AddSeparator()

		mStart := systray.AddMenuItem("Start", "Start the subnet router")
		mStop := systray.AddMenuItem("Stop", "Stop the subnet router")
		mReauth := systray.AddMenuItem("Re-authenticate…", "Log this node out and log in again")
		mAdmin := systray.AddMenuItem("Open admin console", "Open the Tailscale admin console")
		mConfig := systray.AddMenuItem("Edit configuration\u2026", "Open minitail.conf, where Tailscale's flags live")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Quit minitail", "Stop the subnet router and quit")

		views, unsubscribe := ctrl.Subscribe()
		var cur app.View

		render := func(v app.View) {
			cur = v
			systray.SetTemplateIcon(icons.For(v.State), icons.For(v.State))
			mStatus.SetTitle(v.Summary)
			if v.Detail != "" {
				mDetail.SetTitle(v.Detail)
				mDetail.Show()
			} else {
				mDetail.Hide()
			}
			toggle(mLogin, v.AuthURL != "")
			toggle(mApprove, v.State == app.StateNotApproved)
			toggle(mCopyIP, v.TailnetIP != "")
			if v.TailnetIP != "" {
				mCopyIP.SetTitle("Copy tailnet IP (" + v.TailnetIP + ")")
			}
			setEnabled(mStart, v.CanStart)
			setEnabled(mStop, v.CanStop)
			tooltip := "minitail: " + v.Summary
			systray.SetTooltip(tooltip)
		}

		go func() {
			defer unsubscribe()
			for {
				select {
				case <-ctx.Done():
					return
				case v, ok := <-views:
					if !ok {
						return
					}
					render(v)
				case <-mLogin.ClickedCh:
					open(cur.AuthURL)
				case <-mApprove.ClickedCh:
					open(cur.AdminURL())
				case <-mAdmin.ClickedCh:
					open(cur.AdminURL())
				case <-mConfig.ClickedCh:
					// Hand the file to whatever the user's editor is; `open`
					// picks it from the system's default for .conf files.
					if err := desktop.Open(configPath); err != nil {
						log.Printf("opening %s: %v", configPath, err)
					}
				case <-mCopyIP.ClickedCh:
					if err := desktop.CopyToClipboard(cur.TailnetIP); err != nil {
						log.Printf("copying to clipboard: %v", err)
					}
				case <-mStart.ClickedCh:
					ctrl.Start(ctx)
				case <-mStop.ClickedCh:
					ctrl.Stop()
				case <-mReauth.ClickedCh:
					if err := ctrl.Reauthenticate(ctx); err != nil {
						log.Printf("re-authenticating: %v", err)
					}
				case <-mQuit.ClickedCh:
					systray.Quit()
					return
				}
			}
		}()

		go func() {
			ctrl.Run(ctx)
			systray.Quit()
		}()
	}

	onExit := func() {
		stopCtrl()
		cancel()
	}

	systray.Run(onReady, onExit)
	return nil
}

func toggle(m *systray.MenuItem, show bool) {
	if show {
		m.Show()
	} else {
		m.Hide()
	}
}

func setEnabled(m *systray.MenuItem, enabled bool) {
	if enabled {
		m.Enable()
	} else {
		m.Disable()
	}
}

func open(url string) {
	if url == "" {
		return
	}
	if err := desktop.Open(url); err != nil {
		log.Printf("opening %s: %v", url, err)
	}
}
