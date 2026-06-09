package verification

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReportAllPassed_EmptyResultsIsTrue(t *testing.T) {
	// Documented: AllPassed loops over Results and returns true when it finds
	// no failing Result. An empty Report therefore passes vacuously.
	var r Report
	if !r.AllPassed() {
		t.Fatalf("empty Report: got AllPassed()=false, want true")
	}

	r2 := Report{Results: []Result{}}
	if !r2.AllPassed() {
		t.Fatalf("Report with empty (non-nil) slice: got AllPassed()=false, want true")
	}
}

func TestReportAllPassed_AllPassedIsTrue(t *testing.T) {
	r := Report{Results: []Result{
		{Check: "gofmt", Passed: true},
		{Check: "go test", Passed: true},
		{Check: "go vet", Passed: true},
	}}
	if !r.AllPassed() {
		t.Fatalf("all Results passed: got AllPassed()=false, want true")
	}
}

func TestReportAllPassed_AnyFailedIsFalse(t *testing.T) {
	cases := []struct {
		name    string
		results []Result
	}{
		{"single failing", []Result{{Check: "go test", Passed: false}}},
		{"first fails", []Result{
			{Check: "gofmt", Passed: false},
			{Check: "go test", Passed: true},
		}},
		{"last fails", []Result{
			{Check: "gofmt", Passed: true},
			{Check: "go test", Passed: false},
		}},
		{"middle fails", []Result{
			{Check: "gofmt", Passed: true},
			{Check: "go test", Passed: false},
			{Check: "go vet", Passed: true},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Report{Results: tc.results}
			if r.AllPassed() {
				t.Fatalf("%s: got AllPassed()=true, want false", tc.name)
			}
		})
	}
}

// Characterizes the JSON tags declared on Result: check, passed, output
// (omitempty), duration_ms.
func TestResultJSONTags(t *testing.T) {
	res := Result{Check: "go test", Passed: true, Output: "ok", Duration: 42}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal Result: %v", err)
	}
	got := string(b)
	for _, want := range []string{`"check":"go test"`, `"passed":true`, `"output":"ok"`, `"duration_ms":42`} {
		if !strings.Contains(got, want) {
			t.Fatalf("marshalled Result %q missing %q", got, want)
		}
	}

	// Output has omitempty: an empty Output must be dropped, but duration_ms
	// (no omitempty) must remain even when zero.
	b2, err := json.Marshal(Result{Check: "gofmt", Passed: false, Duration: 0})
	if err != nil {
		t.Fatalf("marshal empty-output Result: %v", err)
	}
	got2 := string(b2)
	if strings.Contains(got2, `"output"`) {
		t.Fatalf("empty Output should be omitted, got %q", got2)
	}
	if !strings.Contains(got2, `"duration_ms":0`) {
		t.Fatalf("duration_ms should be present even when zero, got %q", got2)
	}
}

// Round-trip: fields survive marshal/unmarshal via their JSON tags.
func TestResultRoundTrip(t *testing.T) {
	in := Result{Check: "go vet", Passed: false, Output: "vet failed", Duration: 7}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Result
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", out, in)
	}
}
