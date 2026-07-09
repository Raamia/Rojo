package main

import (
	"fmt"
	"strings"

	"github.com/Raamia/Rojo/internal/events"
)

// renderEvent turns one event into a human line, or "" for events not worth a
// line of a user's attention. The event stream is the machine record; this is
// the story a person watches — and step.completed after every step.started is
// repetition, not information.
func renderEvent(e events.Event) string {
	ts := e.CreatedAt.Local().Format("15:04:05")
	p := e.Payload

	switch e.Type {
	case events.TypeStepStarted:
		return fmt.Sprintf("%s  → %v", ts, p["status"])
	case events.TypePlanCreated:
		return fmt.Sprintf("%s  plan: %v", ts, p["summary"])
	case events.TypeImplementationCompleted:
		return fmt.Sprintf("%s  applied %v operation(s): %s", ts, p["operations"], pathList(p["paths"]))
	case events.TypeVerificationCompleted:
		line := fmt.Sprintf("%s  checks: %v", ts, p["summary"])
		if notes := resultNotes(p["results"]); notes != "" {
			line += " — " + notes
		}
		return line
	case events.TypeReviewCompleted:
		return fmt.Sprintf("%s  review: %v — %v", ts, p["decision"], p["notes"])
	case events.TypeRevisionRequested:
		return fmt.Sprintf("%s  revision requested: %s", ts, oneLine(fmt.Sprint(p["notes"]), 120))
	case events.TypeDiffCaptured:
		if p["empty"] == true {
			return fmt.Sprintf("%s  no changes were produced", ts)
		}
		return fmt.Sprintf("%s  patch captured: %v bytes across %s", ts, p["bytes"], pathList(p["files"]))
	case events.TypeFanoutCompleted:
		return fmt.Sprintf("%s  fan-out: %v of %v variants passed", ts, p["passed"], p["total"])
	case events.TypeJobCompleted:
		return fmt.Sprintf("%s  ✓ completed", ts)
	case events.TypeJobFailed:
		return fmt.Sprintf("%s  ✗ failed: %s", ts, oneLine(fmt.Sprint(p["error"]), 200))
	case events.TypeJobCancelled:
		return fmt.Sprintf("%s  cancelled", ts)
	default:
		// job.created, job.started, step.completed, workspace.created,
		// review.started: real events, but noise in a terminal.
		return ""
	}
}

// pathList renders a payload's path slice. Events round-trip through JSON, so
// by the time they reach a client the slice is []any.
func pathList(v any) string {
	items, ok := v.([]any)
	if !ok || len(items) == 0 {
		return "no files"
	}
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, fmt.Sprint(it))
	}
	const max = 6
	if len(parts) > max {
		return strings.Join(parts[:max], ", ") + fmt.Sprintf(", … (%d more)", len(parts)-max)
	}
	return strings.Join(parts, ", ")
}

// resultNotes surfaces the caveats the gate recorded — "no test files found",
// "pytest not installed" — because a green line that means less than it looks
// like it means is exactly what a user needs called out.
func resultNotes(v any) string {
	items, ok := v.([]any)
	if !ok {
		return ""
	}
	var notes []string
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if note, _ := m["note"].(string); note != "" {
			notes = append(notes, note)
		}
	}
	return strings.Join(notes, "; ")
}
