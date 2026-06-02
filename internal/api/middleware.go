package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

type ctxKey int

const loggerKey ctxKey = iota

func LoggerMiddleware(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqID := newRequestID()
			log := base.With(
				"request_id", reqID,
				"method", r.Method,
				"path", r.URL.Path,
			)
			w.Header().Set("X-Request-ID", reqID)

			ctx := context.WithValue(r.Context(), loggerKey, log)
			start := time.Now()
			rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rw, r.WithContext(ctx))
			log.Info("request",
				"status", rw.status,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

func LoggerFrom(ctx context.Context) *slog.Logger {
	if log, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return log
	}
	return slog.Default()
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Hijack forwards to the underlying ResponseWriter so that connection upgrades
// (notably the WebSocket handshake on /stream) still work when this recorder is
// in the chain. Because statusRecorder embeds the http.ResponseWriter interface
// — not the concrete writer — the underlying Hijack method is not promoted, so
// without this the /stream endpoint fails the upgrade with 501 Not Implemented.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
	}
	return hj.Hijack()
}

// Flush forwards to the underlying ResponseWriter when it supports flushing, so
// streaming responses are not buffered by this wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
