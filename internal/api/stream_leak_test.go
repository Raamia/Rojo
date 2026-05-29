package api

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Raamia/Rojo/internal/events"
)

// Clients disappear without a close handshake all the time: a closed browser
// tab, a dropped mobile connection, a killed curl. The stream handler only
// writes, so unless something reads from the connection it never learns the
// peer is gone — r.Context() is not cancelled for a hijacked connection, and a
// finished job produces no further events to fail a write on. Each such client
// would then park a goroutine on the event select forever, holding a bus
// subscription that Publish must walk on every event.
func TestStream_DeadClientsDoNotLeakGoroutines(t *testing.T) {
	bus := events.NewInProcessBus()
	stream := NewStreamHandler(bus)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/jobs/{jobID}/stream", stream.Stream)
	srv := httptest.NewServer(LoggerMiddleware(slog.New(slog.NewTextHandler(io.Discard, nil)))(mux))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	settleStreamGoroutines()
	before := runtime.NumGoroutine()

	const clients = 30
	for i := 0; i < clients; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("tcp dial: %v", err)
		}
		req := "GET /api/v1/jobs/leak-job/stream HTTP/1.1\r\n" +
			"Host: " + addr + "\r\n" +
			"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
			"Sec-WebSocket-Version: 13\r\n\r\n"
		if _, err := c.Write([]byte(req)); err != nil {
			t.Fatalf("write upgrade: %v", err)
		}
		buf := make([]byte, 256)
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = c.Read(buf) // consume the 101 response
		if tcp, ok := c.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0) // RST instead of FIN: abrupt death
		}
		_ = c.Close()
	}

	settleStreamGoroutines()
	after := runtime.NumGoroutine()

	t.Logf("goroutines before=%d after=%d for %d dead clients (delta=%d)",
		before, after, clients, after-before)
	// Before the CloseRead fix this leaked ~2 goroutines per dead client.
	if after-before >= clients/2 {
		t.Errorf("goroutine leak: %d dead clients left %d goroutines alive; "+
			"the handler is not detecting disconnected peers", clients, after-before)
	}
}

func settleStreamGoroutines() {
	for i := 0; i < 6; i++ {
		runtime.GC()
		time.Sleep(100 * time.Millisecond)
	}
}
