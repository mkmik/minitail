package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// statusPath is where the control socket serves the current View.
const statusPath = "/status"

// ServeControl exposes the controller on a unix socket: its current View as
// JSON, so that `minitail status` (and the integration tests) can inspect a
// running supervisor without a GUI, and the menu's commands, so that `minitail
// stop` and friends can drive it. quit is what the menu's Quit item does. It
// returns when ctx is cancelled.
func ServeControl(ctx context.Context, c *Controller, socketPath string, quit func()) error {
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
	mux.HandleFunc("GET "+statusPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(c.View())
	})
	// Commands get ctx rather than the request's context, which ends with the
	// response: Start and Reload hand it on to tailscaled.
	mux.HandleFunc("POST /{command}", func(w http.ResponseWriter, r *http.Request) {
		var err error
		switch r.PathValue("command") {
		case "start":
			c.Start(ctx)
		case "stop":
			c.Stop()
		case "reload":
			err = c.Reload(ctx)
		case "reauthenticate":
			err = c.Reauthenticate(ctx)
		case "quit":
			// Answer first: minitail can exit before this handler returns.
			w.WriteHeader(http.StatusNoContent)
			_ = http.NewResponseController(w).Flush()
			quit()
			return
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
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
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := call(ctx, socketPath, http.MethodGet, statusPath)
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

// Command asks a running minitail to carry out one of the commands
// ServeControl accepts.
func Command(ctx context.Context, socketPath, name string) error {
	resp, err := call(ctx, socketPath, http.MethodPost, "/"+name)
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return fmt.Errorf("minitail is not running: %w", opErr)
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		// Every command this binary sends exists in its own ServeControl.
		return fmt.Errorf("the running minitail predates %q; restart it to run the installed version", name)
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return errors.New(strings.TrimSpace(string(msg)))
}

// call sends one request to a running minitail over its control socket.
func call(ctx context.Context, socketPath, method, path string) (*http.Response, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}
	// The host is ignored by the unix dialer but must be syntactically valid.
	req, err := http.NewRequestWithContext(ctx, method, "http://minitail"+path, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}
