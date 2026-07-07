package api

import (
	"errors"
	"net/http"
	"runtime/debug"
)

// RecoverMiddleware turns a panicking handler into a 500.
//
// net/http already recovers handler panics so the process survives, but it does
// it by closing the connection without writing a status line: the client sees a
// truncated response rather than an error, and the logging middleware records
// whatever status it had assumed. This recovers first, so the panic is logged
// with a stack and with the request id the client was given, and the client gets
// an ordinary JSON error it can act on.
//
// It must wrap the handler *inside* LoggerMiddleware, so the request id and
// per-request logger are already in context when a panic is caught.
func RecoverMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// http.ErrAbortHandler is how a handler deliberately drops a
				// connection — the streaming endpoint uses it. Re-panicking
				// keeps net/http's meaning intact instead of converting an
				// intentional abort into a spurious 500.
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}
				LoggerFrom(r.Context()).Error("panic serving request",
					"panic", rec, "stack", string(debug.Stack()))

				// A hijacked connection (a WebSocket) has no response left to
				// write; attempting one would panic again inside this handler.
				if hijacked(w) {
					return
				}
				WriteJSONError(w, http.StatusInternalServerError, "internal server error")
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// hijacked reports whether the response has been taken over, which the stream
// handler does when it upgrades to a WebSocket.
func hijacked(w http.ResponseWriter) bool {
	rec, ok := w.(*statusRecorder)
	return ok && rec.hijacked
}
