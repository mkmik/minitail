package app

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// statusPath is the single endpoint served on the control socket.
const statusPath = "/status"

// ServeStatus exposes the controller's current View as JSON on a unix socket,
// so that `minitail status` (and the integration tests) can inspect a running
// supervisor without a GUI. It returns when ctx is cancelled.
func ServeStatus(ctx context.Context, c *Controller, socketPath string) error {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return err
	}
	// A socket left behind by a process that did not shut down cleanly would
	// otherwise make every subsequent start fail with "address already in use".
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer os.Remove(socketPath)

	mux := http.NewServeMux()
	mux.HandleFunc(statusPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(c.View())
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// FetchStatus reads the View from a running minitail over its control socket.
func FetchStatus(ctx context.Context, socketPath string) (View, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 5 * time.Second,
	}
	// The host is ignored by the unix dialer but must be syntactically valid.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://minitail"+statusPath, nil)
	if err != nil {
		return View{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return View{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return View{}, errors.New("minitail status: unexpected response " + resp.Status)
	}
	var v View
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return View{}, err
	}
	return v, nil
}
