// Package httpd is the HTTP server under a tool's routes: it listens, answers a health check, and on stop lets requests under way finish. An MCP
// server and a metrics page each serve through it.
package httpd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/katbyte/go-kt/clog"
)

const (
	// HealthPath answers "ok" while the tool is serving, whatever state the thing it works on is in: a container is not unhealthy because what it
	// watches is down.
	HealthPath = "/healthz"

	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 10 * time.Second
)

// Health adds the health check to mux at HealthPath.
func Health(mux *http.ServeMux) {
	mux.HandleFunc("GET "+HealthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
}

// Listen takes the address to serve on, ":8080" say, before anything is served, so a tool can say where it is serving: a port of 0 is one picked
// for it. An address that cannot be listened on is an error naming it.
func Listen(ctx context.Context, addr string) (net.Listener, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", addr, err)
	}

	return ln, nil
}

// Serve serves h on ln until the context ends, then gives requests under way ten seconds to finish. Each onStop runs as the stop begins: an MCP
// server closes its sessions there, which would otherwise hold the stop open. Whoever calls it ends the context on a signal.
func Serve(ctx context.Context, ln net.Listener, h http.Handler, onStop ...func()) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: readHeaderTimeout}
	for _, f := range onStop {
		srv.RegisterOnShutdown(f)
	}

	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(ln) }()

	select {
	case err := <-failed:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return fmt.Errorf("serving on %s: %w", ln.Addr(), err)
	case <-ctx.Done():
	}

	clog.Log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}

	return nil
}
