package api

import (
	"net/http"
	"strconv"

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

	// `?since=N` returns only events at index N onward, so a client polling a
	// running job fetches just what is new instead of the whole log every time.
	// The event log is append-only, so an index is a stable cursor: event N is
	// always the same event. An out-of-range or unparseable value yields an
	// empty slice rather than an error — a poller that has already seen
	// everything is asking a perfectly reasonable question.
	if raw := r.URL.Query().Get("since"); raw != "" {
		since, err := strconv.Atoi(raw)
		if err != nil || since < 0 {
			WriteJSONError(w, http.StatusBadRequest, "since must be a non-negative integer")
			return
		}
		if since > len(history) {
			since = len(history)
		}
		history = history[since:]
	}
	WriteJSON(w, http.StatusOK, history)
}
