package api

import (
	"net/http"

	"github.com/Raamia/Rojo/internal/events"
)

type EventsHandler struct {
	Store events.Store
}

func NewEventsHandler(store events.Store) *EventsHandler {
	return &EventsHandler{Store: store}
}

func (h *EventsHandler) History(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		WriteJSONError(w, http.StatusServiceUnavailable, "event store not available")
		return
	}
	jobID := r.PathValue("jobID")
	history, err := h.Store.History(r.Context(), jobID)
	if err != nil {
		LoggerFrom(r.Context()).Error("load event history", "job_id", jobID, "err", err)
		WriteJSONError(w, http.StatusInternalServerError, "failed to load events")
		return
	}
	if history == nil {
		history = []events.Event{}
	}
	WriteJSON(w, http.StatusOK, history)
}
