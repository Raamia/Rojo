package benchmark

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSummarize_Rates(t *testing.T) {
	results := []Result{
		{Case: "a", PipelineSuccess: true, TaskSuccess: true, DurationMs: 1000},
		{Case: "b", PipelineSuccess: true, TaskSuccess: true, DurationMs: 2000, Revisions: 1},
		// The interesting one: the pipeline was satisfied and the task was not done.
		{Case: "c", PipelineSuccess: true, TaskSuccess: false, DurationMs: 3000},
		{Case: "d", PipelineSuccess: false, TaskSuccess: false, DurationMs: 4000},
	}
	s := Summarize(results, Prices{})

	if s.Measured != 4 {
		t.Fatalf("Measured = %d, want 4", s.Measured)
	}
	if s.PipelineSuccessRate != 0.75 {
		t.Errorf("PipelineSuccessRate = %v, want 0.75", s.PipelineSuccessRate)
	}
	if s.TaskSuccessRate != 0.5 {
		t.Errorf("TaskSuccessRate = %v, want 0.5", s.TaskSuccessRate)
	}
	if s.FalseSuccesses != 1 || s.FalseSuccessRate != 0.25 {
		t.Errorf("false successes = %d (%v), want 1 (0.25)", s.FalseSuccesses, s.FalseSuccessRate)
	}
	if s.RevisionRate != 0.25 {
		t.Errorf("RevisionRate = %v, want 0.25", s.RevisionRate)
	}
}

// A case the harness could not run is not a case the system failed. Counting it
// as a failure would understate the thing being measured, which is the exact
// dishonesty this package exists to avoid.
func TestSummarize_HarnessErrorsExcludedFromRates(t *testing.T) {
	results := []Result{
		{Case: "a", PipelineSuccess: true, TaskSuccess: true, DurationMs: 100},
		{Case: "b", Error: "could not reach the server"},
	}
	s := Summarize(results, Prices{})

	if s.Cases != 2 {
		t.Errorf("Cases = %d, want 2", s.Cases)
	}
	if s.Measured != 1 || s.HarnessErrors != 1 {
		t.Fatalf("Measured/HarnessErrors = %d/%d, want 1/1", s.Measured, s.HarnessErrors)
	}
	if s.TaskSuccessRate != 1.0 {
		t.Errorf("TaskSuccessRate = %v, want 1.0 — the errored case must not count as a failure", s.TaskSuccessRate)
	}
}

func TestResult_FalseSuccess(t *testing.T) {
	tests := []struct {
		name     string
		pipeline bool
		task     bool
		want     bool
	}{
		{"claimed success, task done", true, true, false},
		{"claimed success, task NOT done", true, false, true},
		{"admitted failure, task not done", false, false, false},
		{"admitted failure, task done anyway", false, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Result{PipelineSuccess: tt.pipeline, TaskSuccess: tt.task}
			if got := r.FalseSuccess(); got != tt.want {
				t.Errorf("FalseSuccess() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPercentiles_NearestRank(t *testing.T) {
	// Nearest rank never invents a value: every result must be one of the
	// inputs, which is the property that makes it honest at small n.
	got := percentiles([]int64{50, 10, 30, 20, 40})
	if got.Min != 10 || got.Max != 50 {
		t.Errorf("min/max = %d/%d, want 10/50", got.Min, got.Max)
	}
	if got.Mean != 30 {
		t.Errorf("Mean = %d, want 30", got.Mean)
	}
	// ceil(0.50*5) = 3 -> the 3rd smallest = 30
	if got.P50 != 30 {
		t.Errorf("P50 = %d, want 30", got.P50)
	}
	// ceil(0.95*5) = 5 -> the 5th smallest = 50
	if got.P95 != 50 {
		t.Errorf("P95 = %d, want 50", got.P95)
	}
}

func TestPercentiles_EdgeCases(t *testing.T) {
	if got := percentiles(nil); got != (Percentiles{}) {
		t.Errorf("percentiles(nil) = %+v, want zero", got)
	}
	one := percentiles([]int64{7})
	if one.Min != 7 || one.P50 != 7 || one.P95 != 7 || one.Max != 7 || one.Mean != 7 {
		t.Errorf("single sample = %+v, want every field 7", one)
	}
}

// percentiles must not reorder the caller's slice; Summarize builds it from
// results that are reported afterwards.
func TestPercentiles_DoesNotMutateInput(t *testing.T) {
	in := []int64{3, 1, 2}
	percentiles(in)
	if in[0] != 3 || in[1] != 1 || in[2] != 2 {
		t.Errorf("input was reordered: %v", in)
	}
}

func TestSummarize_CostOnlyWhenPricesGiven(t *testing.T) {
	results := []Result{
		{Case: "a", TaskSuccess: true, InputTokens: 1_000_000, OutputTokens: 500_000, DurationMs: 10},
	}

	none := Summarize(results, Prices{})
	if none.CostUSD != nil || none.MeanCostUSD != nil {
		t.Error("cost was computed with no prices supplied; an invented price is a wrong number")
	}

	priced := Summarize(results, Prices{InputPerMillion: 2, OutputPerMillion: 10})
	if priced.CostUSD == nil {
		t.Fatal("cost was not computed despite prices being supplied")
	}
	// 1M in at $2 + 0.5M out at $10 = 2 + 5 = 7
	if *priced.CostUSD < 6.999 || *priced.CostUSD > 7.001 {
		t.Errorf("CostUSD = %v, want 7", *priced.CostUSD)
	}
	if *priced.MeanCostUSD < 6.999 || *priced.MeanCostUSD > 7.001 {
		t.Errorf("MeanCostUSD = %v, want 7 for a single case", *priced.MeanCostUSD)
	}
}

func TestSummarize_TokenTotals(t *testing.T) {
	s := Summarize([]Result{
		{Case: "a", InputTokens: 100, OutputTokens: 50, ModelCalls: 3, DurationMs: 1},
		{Case: "b", InputTokens: 300, OutputTokens: 150, ModelCalls: 3, ModelErrors: 1, DurationMs: 1},
	}, Prices{})

	if s.InputTokens != 400 || s.OutputTokens != 200 || s.TotalTokens != 600 {
		t.Errorf("tokens = %d/%d/%d, want 400/200/600", s.InputTokens, s.OutputTokens, s.TotalTokens)
	}
	if s.MeanTokensPerCase != 300 {
		t.Errorf("MeanTokensPerCase = %d, want 300", s.MeanTokensPerCase)
	}
	if s.ModelCalls != 6 || s.ModelErrors != 1 {
		t.Errorf("model calls/errors = %d/%d, want 6/1", s.ModelCalls, s.ModelErrors)
	}
}

func TestSummarize_EmptyRun(t *testing.T) {
	s := Summarize(nil, Prices{InputPerMillion: 1})
	if s.Cases != 0 || s.Measured != 0 {
		t.Errorf("empty run = %d cases / %d measured", s.Cases, s.Measured)
	}
	// Rates on zero cases must be zero rather than NaN, which would serialise
	// as invalid JSON and take the whole report down with it.
	if s.TaskSuccessRate != 0 || s.RevisionRate != 0 {
		t.Errorf("rates on an empty run = %v/%v, want 0", s.TaskSuccessRate, s.RevisionRate)
	}
	if _, err := json.Marshal(s); err != nil {
		t.Fatalf("empty summary does not marshal: %v", err)
	}
}

func TestTable_SurfacesFalseSuccess(t *testing.T) {
	s := Summarize([]Result{
		{Case: "sneaky", Status: "completed", PipelineSuccess: true, TaskSuccess: false, DurationMs: 500},
	}, Prices{})
	table := s.Table()

	if !strings.Contains(table, "false success") {
		t.Error("table does not report the false-success rate")
	}
	// The per-case row has to make it obvious, not just the summary line.
	if !strings.Contains(table, "NO(!)") {
		t.Errorf("per-case row does not flag the false success:\n%s", table)
	}
	if !strings.Contains(table, "small sample") {
		t.Error("table does not caveat a single-case run")
	}
}

func TestTable_NoSmallSampleNoteWhenNothingMeasured(t *testing.T) {
	if got := Summarize(nil, Prices{}).Table(); strings.Contains(got, "small sample") {
		t.Error("empty run should not carry a small-sample caveat")
	}
}

func TestFilterCases(t *testing.T) {
	all := []Case{{Name: "a"}, {Name: "b"}, {Name: "c"}}

	t.Run("empty filter keeps everything", func(t *testing.T) {
		got, err := FilterCases(all, nil)
		if err != nil || len(got) != 3 {
			t.Fatalf("got %d cases, %v", len(got), err)
		}
	})

	t.Run("selects and preserves order", func(t *testing.T) {
		got, err := FilterCases(all, []string{"c", "a"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
			t.Errorf("got %v, want [a c] in fixture order", names(got))
		}
	})

	t.Run("unknown name is an error, not a silent skip", func(t *testing.T) {
		if _, err := FilterCases(all, []string{"a", "nope"}); err == nil {
			t.Fatal("unknown case name was accepted")
		} else if !strings.Contains(err.Error(), "nope") {
			t.Errorf("err = %v, want it to name the missing case", err)
		}
	})

	t.Run("blank entries are ignored", func(t *testing.T) {
		got, err := FilterCases(all, []string{"a", "  ", ""})
		if err != nil || len(got) != 1 {
			t.Fatalf("got %d cases, %v", len(got), err)
		}
	})
}

func names(cases []Case) []string {
	out := make([]string, len(cases))
	for i, c := range cases {
		out[i] = c.Name
	}
	return out
}

// A server that predates token accounting reports calls with no tokens. That
// combination must be flagged, because the report otherwise reads as a run
// that cost nothing.
func TestSummary_FlagsStaleServerTokens(t *testing.T) {
	stale := Summarize([]Result{
		{Case: "a", TaskSuccess: true, ModelCalls: 3, DurationMs: 10},
	}, Prices{})
	if !stale.StaleServerTokens() {
		t.Fatal("calls with zero tokens was not flagged as a stale server")
	}
	if !strings.Contains(stale.Table(), "predates token accounting") {
		t.Errorf("table does not warn about the stale server:\n%s", stale.Table())
	}

	healthy := Summarize([]Result{
		{Case: "a", TaskSuccess: true, ModelCalls: 3, InputTokens: 10, OutputTokens: 5, DurationMs: 10},
	}, Prices{})
	if healthy.StaleServerTokens() {
		t.Error("a run with tokens was flagged as stale")
	}
	if strings.Contains(healthy.Table(), "predates token accounting") {
		t.Error("healthy run carries a stale-server warning")
	}

	// A run with no model calls at all (agents disabled) is not stale, just idle.
	idle := Summarize([]Result{{Case: "a", DurationMs: 10}}, Prices{})
	if idle.StaleServerTokens() {
		t.Error("a run with no model calls was flagged as stale")
	}
}
