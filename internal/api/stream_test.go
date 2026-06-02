package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Raamia/Rojo/internal/events"
)

// The /stream endpoint upgrades to WebSocket, which requires the
// ResponseWriter to support hijacking. LoggerMiddleware wraps the writer in a
// statusRecorder; if that wrapper does not forward Hijack, the handshake fails
// with 501 through the real middleware chain — which is how the endpoint is
// actually served in main.go. This test dials through LoggerMiddleware to guard
// that path.
func TestStream_UpgradesThroughLoggerMiddleware(t *testing.T) {
	bus := events.NewInProcessBus()
	stream := NewStreamHandler(bus)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/jobs/{jobID}/stream", stream.Stream)
	handler := LoggerMiddleware(slog.New(slog.NewTextHandler(io.Discard, nil)))(mux)

	srv := httptest.NewServer(handler)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	jobID := "ws-job"
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/jobs/" + jobID + "/stream"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial failed (upgrade broken through middleware): %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	// Let the handler register its subscription, then publish an event for this
	// job. Retry publishing a few times to avoid racing the Subscribe call.
	want := events.Event{JobID: jobID, Type: events.TypeJobStarted}
	go func() {
		for i := 0; i < 20; i++ {
			_ = bus.Publish(context.Background(), want)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	var got events.Event
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if got.Type != events.TypeJobStarted || got.JobID != jobID {
		t.Fatalf("got event %+v, want type=%s job=%s", got, events.TypeJobStarted, jobID)
	}
}
