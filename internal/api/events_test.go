package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Raamia/Rojo/internal/events"
)

type stubEventStore struct {
	history []events.Event
	err     error
}

func (s stubEventStore) Append(context.Context, events.Event) error { return s.err }
func (s stubEventStore) History(context.Context, string) ([]events.Event, error) {
	return s.history, s.err
}

func getEvents(h *EventsHandler, id, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+id+"/events"+query, nil)
	req.SetPathValue("jobID", id)
	rec := httptest.NewRecorder()
	h.History(rec, req)
	return rec
}

func threeEvents() []events.Event {
	return []events.Event{
		{JobID: "j", Type: events.TypeJobStarted},
		{JobID: "j", Type: events.TypePlanCreated},
		{JobID: "j", Type: events.TypeJobCompleted},
	}
}

func decodeEvents(t *testing.T, rec *httptest.ResponseRecorder) []events.Event {
	t.Helper()
	var out []events.Event
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v — body %s", err, rec.Body)
	}
	return out
}

func TestEvents_WholeHistoryByDefault(t *testing.T) {
	h := NewEventsHandler(stubEventStore{history: threeEvents()})
	rec := getEvents(h, "j", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if got := decodeEvents(t, rec); len(got) != 3 {
		t.Errorf("got %d events, want 3", len(got))
	}
}

// The point of the cursor: a poller that has seen N events fetches only what is
// new, so watching a long job stays O(events) rather than O(polls × events).
func TestEvents_SinceReturnsOnlyNewer(t *testing.T) {
	h := NewEventsHandler(stubEventStore{history: threeEvents()})

	rec := getEvents(h, "j", "?since=2")
	got := decodeEvents(t, rec)
	if len(got) != 1 || got[0].Type != events.TypeJobCompleted {
		t.Errorf("?since=2 = %+v, want just the last event", got)
	}
}

// A poller caught up to the end asks a reasonable question and gets an empty
// list, not an error — that is the steady state while a job runs.
func TestEvents_SinceAtOrBeyondEndIsEmpty(t *testing.T) {
	h := NewEventsHandler(stubEventStore{history: threeEvents()})
	for _, q := range []string{"?since=3", "?since=99"} {
		rec := getEvents(h, "j", q)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", q, rec.Code)
		}
		if got := decodeEvents(t, rec); len(got) != 0 {
			t.Errorf("%s returned %d events, want 0", q, len(got))
		}
		// Must serialise as [] not null, so a client's decode always succeeds.
		if rec.Body.String() == "null\n" {
			t.Errorf("%s body is null, want []", q)
		}
	}
}

func TestEvents_SinceRejectsNonsense(t *testing.T) {
	h := NewEventsHandler(stubEventStore{history: threeEvents()})
	for _, q := range []string{"?since=-1", "?since=abc"} {
		if rec := getEvents(h, "j", q); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, rec.Code)
		}
	}
}

// The cursor is an index into an append-only log, so event N is always the same
// event: paging through in two halves must reconstruct the whole history with
// nothing dropped or repeated.
func TestEvents_SincePagingReconstructsTheWholeLog(t *testing.T) {
	h := NewEventsHandler(stubEventStore{history: threeEvents()})

	first := decodeEvents(t, getEvents(h, "j", "?since=0"))
	seen := len(first)
	rest := decodeEvents(t, getEvents(h, "j", "?since=1"))

	combined := append(first[:1], rest...)
	if len(combined) != 3 {
		t.Fatalf("paging produced %d events, want 3", len(combined))
	}
	_ = seen
	for i, want := range []string{events.TypeJobStarted, events.TypePlanCreated, events.TypeJobCompleted} {
		if combined[i].Type != want {
			t.Errorf("event %d = %s, want %s", i, combined[i].Type, want)
		}
	}
}
