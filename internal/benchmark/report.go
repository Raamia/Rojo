package benchmark

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Prices convert tokens into money, in dollars per million tokens.
//
// They are supplied by whoever runs the benchmark rather than baked in on
// purpose. Provider pricing changes, and a hardcoded table silently turns a
// measured number into a wrong one months later — the exact failure this
// package exists to avoid. Zero means cost is simply not reported.
type Prices struct {
	InputPerMillion  float64
	OutputPerMillion float64
}

func (p Prices) set() bool { return p.InputPerMillion > 0 || p.OutputPerMillion > 0 }

func (p Prices) cost(in, out int64) float64 {
	return float64(in)/1e6*p.InputPerMillion + float64(out)/1e6*p.OutputPerMillion
}

// Summary aggregates a run.
type Summary struct {
	Cases int `json:"cases"`
	// Completed counts cases the harness actually measured; a case that errored
	// out is excluded from every rate below, because counting a harness failure
	// as a task failure would understate the system being measured.
	Measured      int `json:"measured"`
	HarnessErrors int `json:"harness_errors"`

	PipelineSuccesses int `json:"pipeline_successes"`
	TaskSuccesses     int `json:"task_successes"`
	FalseSuccesses    int `json:"false_successes"`
	CasesWithRevision int `json:"cases_with_revision"`

	PipelineSuccessRate float64 `json:"pipeline_success_rate"`
	TaskSuccessRate     float64 `json:"task_success_rate"`
	FalseSuccessRate    float64 `json:"false_success_rate"`
	RevisionRate        float64 `json:"revision_rate"`

	DurationMs Percentiles `json:"duration_ms"`

	ModelCalls   int64 `json:"model_calls"`
	ModelErrors  int64 `json:"model_errors"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`

	MeanTokensPerCase int64 `json:"mean_tokens_per_case"`

	// CostUSD and MeanCostUSD are only populated when prices were supplied.
	CostUSD     *float64 `json:"total_cost_usd,omitempty"`
	MeanCostUSD *float64 `json:"mean_cost_usd_per_case,omitempty"`

	Results []Result `json:"results"`
}

// StaleServerTokens reports the one combination that cannot happen on a server
// with token accounting: calls were made and no tokens were counted.
//
// It exists because the failure is silent and the symptom looks like good news
// — a report claiming a run cost nothing — and the cause is usually a binary
// built before the accounting existed.
func (s Summary) StaleServerTokens() bool { return s.ModelCalls > 0 && s.TotalTokens == 0 }

type Percentiles struct {
	Min  int64 `json:"min"`
	P50  int64 `json:"p50"`
	P95  int64 `json:"p95"`
	Max  int64 `json:"max"`
	Mean int64 `json:"mean"`
}

// Summarize aggregates results into the numbers a claim can be made from.
func Summarize(results []Result, prices Prices) Summary {
	s := Summary{Cases: len(results), Results: results}

	durations := make([]int64, 0, len(results))
	for _, r := range results {
		if r.Error != "" {
			s.HarnessErrors++
			continue
		}
		s.Measured++
		if r.PipelineSuccess {
			s.PipelineSuccesses++
		}
		if r.TaskSuccess {
			s.TaskSuccesses++
		}
		if r.FalseSuccess() {
			s.FalseSuccesses++
		}
		if r.Revisions > 0 {
			s.CasesWithRevision++
		}
		durations = append(durations, r.DurationMs)

		// Token and call totals include errored-out cases' contributions only
		// when the case was measured; an aborted case's deltas are unreliable.
		s.ModelCalls += r.ModelCalls
		s.ModelErrors += r.ModelErrors
		s.InputTokens += r.InputTokens
		s.OutputTokens += r.OutputTokens
	}

	s.TotalTokens = s.InputTokens + s.OutputTokens
	s.DurationMs = percentiles(durations)

	if s.Measured > 0 {
		s.PipelineSuccessRate = ratio(s.PipelineSuccesses, s.Measured)
		s.TaskSuccessRate = ratio(s.TaskSuccesses, s.Measured)
		s.FalseSuccessRate = ratio(s.FalseSuccesses, s.Measured)
		s.RevisionRate = ratio(s.CasesWithRevision, s.Measured)
		s.MeanTokensPerCase = s.TotalTokens / int64(s.Measured)
	}

	if prices.set() {
		total := prices.cost(s.InputTokens, s.OutputTokens)
		s.CostUSD = &total
		if s.Measured > 0 {
			mean := total / float64(s.Measured)
			s.MeanCostUSD = &mean
		}
	}
	return s
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	// Rounded to four places so a JSON report does not carry float noise like
	// 0.8333333333333334 into a number someone will quote.
	return math.Round(float64(n)/float64(d)*10000) / 10000
}

// percentiles uses the nearest-rank method, which for the small case counts a
// benchmark like this runs is the only honest choice: interpolating between two
// samples invents a value that was never observed.
func percentiles(values []int64) Percentiles {
	if len(values) == 0 {
		return Percentiles{}
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var total int64
	for _, v := range sorted {
		total += v
	}
	return Percentiles{
		Min:  sorted[0],
		P50:  nearestRank(sorted, 0.50),
		P95:  nearestRank(sorted, 0.95),
		Max:  sorted[len(sorted)-1],
		Mean: total / int64(len(sorted)),
	}
}

func nearestRank(sorted []int64, p float64) int64 {
	rank := int(math.Ceil(p * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// Table renders the summary for a terminal.
func (s Summary) Table() string {
	var b strings.Builder

	b.WriteString("\nPER-CASE\n")
	b.WriteString("case                 difficulty  status     pipeline  truth   revs   dur     tokens\n")
	b.WriteString("-------------------- ----------- ---------- --------- ------- ------ ------- --------\n")
	for _, r := range s.Results {
		status := r.Status
		if r.Error != "" {
			status = "harness-err"
		}
		fmt.Fprintf(&b, "%-20s %-11s %-10s %-9s %-7s %-6d %-7s %-8d\n",
			trim(r.Case, 20), trim(r.Difficulty, 11), trim(status, 10),
			yesNo(r.PipelineSuccess), truthLabel(r),
			r.Revisions, dur(r.DurationMs), r.InputTokens+r.OutputTokens)
	}

	b.WriteString("\nSUMMARY\n")
	fmt.Fprintf(&b, "  cases                  %d measured", s.Measured)
	if s.HarnessErrors > 0 {
		fmt.Fprintf(&b, "  (%d harness errors, excluded from rates)", s.HarnessErrors)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "  task success           %.1f%%  (%d/%d)   <- ground truth: hidden assertion passed\n",
		s.TaskSuccessRate*100, s.TaskSuccesses, s.Measured)
	fmt.Fprintf(&b, "  pipeline success       %.1f%%  (%d/%d)   <- the job reported completed\n",
		s.PipelineSuccessRate*100, s.PipelineSuccesses, s.Measured)
	fmt.Fprintf(&b, "  false success          %.1f%%  (%d/%d)   <- claimed success, task NOT done\n",
		s.FalseSuccessRate*100, s.FalseSuccesses, s.Measured)
	fmt.Fprintf(&b, "  needed a revision      %.1f%%  (%d/%d)\n",
		s.RevisionRate*100, s.CasesWithRevision, s.Measured)
	fmt.Fprintf(&b, "  duration               p50 %s   p95 %s   max %s   mean %s\n",
		dur(s.DurationMs.P50), dur(s.DurationMs.P95), dur(s.DurationMs.Max), dur(s.DurationMs.Mean))
	fmt.Fprintf(&b, "  model calls            %d  (%d errors)\n", s.ModelCalls, s.ModelErrors)
	fmt.Fprintf(&b, "  tokens                 %d in / %d out / %d total  (mean %d per case)\n",
		s.InputTokens, s.OutputTokens, s.TotalTokens, s.MeanTokensPerCase)
	if s.CostUSD != nil && s.MeanCostUSD != nil {
		fmt.Fprintf(&b, "  cost                   $%.4f total   $%.4f per case\n", *s.CostUSD, *s.MeanCostUSD)
	} else {
		b.WriteString("  cost                   not computed (pass -price-in and -price-out)\n")
	}

	// Model calls with no tokens behind them means the server is older than
	// token accounting and is not reporting any. Left unflagged this reads as
	// "the run was free", which is the most misleading number this report could
	// print — and it is caused by something as mundane as a stale binary.
	if s.StaleServerTokens() {
		b.WriteString("\n  WARNING: the server reported " +
			fmt.Sprintf("%d model calls but zero tokens.\n", s.ModelCalls) +
			"           It predates token accounting — rebuild it (`make build`) and re-run.\n" +
			"           Token and cost figures above are missing, not zero.\n")
	}

	if s.Measured > 0 && s.Measured < 20 {
		fmt.Fprintf(&b, "\n  NOTE: %d cases is a small sample. p95 is the nearest observed value,\n"+
			"        not an estimate, and a single case moves every rate by %.1f points.\n",
			s.Measured, 100/float64(s.Measured))
	}
	return b.String()
}

func truthLabel(r Result) string {
	switch {
	case r.Error != "":
		return "-"
	case r.TaskSuccess:
		return "yes"
	case r.PipelineSuccess:
		return "NO(!)" // a false success: the interesting failure
	default:
		return "no"
	}
}

func dur(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return (time.Duration(ms) * time.Millisecond).Round(100 * time.Millisecond).String()
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
