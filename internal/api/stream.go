package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/Raamia/Rojo/internal/events"
)

const (
	subscriberBuffer = 64
	writeTimeout     = 5 * time.Second
)

type StreamHandler struct {
	Bus events.Bus
}

func NewStreamHandler(bus events.Bus) *StreamHandler {
	return &StreamHandler{Bus: bus}
}

func (h *StreamHandler) Stream(w http.ResponseWriter, r *http.Request) {
	if h.Bus == nil {
		WriteJSONError(w, http.StatusServiceUnavailable, "event bus not available")
		return
	}
	jobID := r.PathValue("jobID")

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Error("ws accept", "err", err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "bye")

	sub := h.Bus.Subscribe(jobID, subscriberBuffer)
	defer h.Bus.Unsubscribe(sub)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-sub.C:
			if !ok {
				return
			}
			if err := writeEvent(ctx, conn, evt); err != nil {
				slog.Info("ws write failed, closing subscriber", "job_id", jobID, "err", err)
				return
			}
		}
	}
}

func writeEvent(ctx context.Context, conn *websocket.Conn, evt events.Event) error {
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	body, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	return conn.Write(writeCtx, websocket.MessageText, body)
}
