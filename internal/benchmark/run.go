package benchmark

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Raamia/Rojo/internal/events"
	"github.com/Raamia/Rojo/internal/execution"
)

// Result is everything one case produced.
type Result struct {
	Case       string `json:"case"`
	Difficulty string `json:"difficulty,omitempty"`
	JobID      string `json:"job_id,omitempty"`

	// Status is the job's terminal state as the pipeline reported it.
	Status string `json:"status"`
	// PipelineSuccess is the pipeline's own verdict: it reached `completed`.
	PipelineSuccess bool `json:"pipeline_success"`
	// TaskSuccess is the hidden assertion's verdict, and the only one that
	// says the task was actually done.
	TaskSuccess bool `json:"task_success"`
	// GroundTruthStage names where verification stopped when it failed.
	GroundTruthStage  string `json:"ground_truth_stage,omitempty"`
	GroundTruthOutput string `json:"ground_truth_output,omitempty"`

	Revisions    int   `json:"revisions"`
	DurationMs   int64 `json:"duration_ms"`
	ModelCalls   int64 `json:"model_calls"`
	ModelErrors  int64 `json:"model_errors"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`

	PatchBytes int `json:"patch_bytes"`

	// Error is set when the harness itself could not complete the case, which
	// is distinct from the case failing.
	Error string `json:"error,omitempty"`
}

// FalseSuccess reports the case where the pipeline claimed success and the
// task was not actually done.
//
// This is the number the whole benchmark exists to produce. Every other metric
// can be gamed by a model that writes something harmless; this one cannot,
// because the assertion it is measured against was never in the repository the
// model saw.
func (r Result) FalseSuccess() bool { return r.PipelineSuccess && !r.TaskSuccess }

// Runner executes cases against a running server.
type Runner struct {
	Client *Client
	// Git and Go run the harness's own commands — applying the patch and
	// running the hidden assertion. Separate from anything the server uses.
	Commands execution.CommandRunner
	// CaseTimeout bounds one case end to end.
	CaseTimeout time.Duration
	// Poll is how often the job's status is checked.
	Poll time.Duration
	// Log receives progress lines. Nil discards them.
	Log io.Writer
}

const DefaultCaseTimeout = 10 * time.Minute

func (r *Runner) logf(format string, args ...any) {
	if r.Log == nil {
		return
	}
	fmt.Fprintf(r.Log, format+"\n", args...)
}

func (r *Runner) caseTimeout() time.Duration {
	if r.CaseTimeout <= 0 {
		return DefaultCaseTimeout
	}
	return r.CaseTimeout
}

// RunCase runs one case end to end and returns what it produced.
//
// A harness failure is returned inside the Result rather than as an error, so
// one broken case does not abandon the run: a partial report with an explicit
// hole in it is worth more than no report.
func (r *Runner) RunCase(ctx context.Context, c Case) Result {
	res := Result{Case: c.Name, Difficulty: c.Difficulty}

	ctx, cancel := context.WithTimeout(ctx, r.caseTimeout())
	defer cancel()

	// Token counters are process-wide, so a per-case figure is the difference
	// across the case. Valid only because cases run sequentially; the report
	// says so rather than leaving a reader to assume otherwise.
	before, err := r.Client.Metrics(ctx)
	if err != nil {
		res.Error = fmt.Sprintf("read metrics before the case: %v", err)
		return res
	}

	repoDir, err := os.MkdirTemp("", "rojo-bench-repo-")
	if err != nil {
		res.Error = fmt.Sprintf("create repo dir: %v", err)
		return res
	}
	defer os.RemoveAll(repoDir)

	if err := c.InitRepo(ctx, r.Commands, repoDir); err != nil {
		res.Error = fmt.Sprintf("prepare fixture repo: %v", err)
		return res
	}

	job, err := r.Client.CreateJob(ctx, c.Task, repoDir)
	if err != nil {
		res.Error = fmt.Sprintf("submit job: %v", err)
		return res
	}
	res.JobID = job.ID
	r.logf("  %-18s job %s submitted", c.Name, job.ID)

	final, err := r.Client.WaitForTerminal(ctx, job.ID, r.Poll)
	if err != nil {
		res.Status = final.Status
		res.Error = fmt.Sprintf("wait for job: %v", err)
		return res
	}
	res.Status = final.Status
	res.PipelineSuccess = final.Status == "completed"
	// Server-side timestamps rather than harness wall clock, so the figure does
	// not carry the poll interval as error.
	res.DurationMs = final.UpdatedAt.Sub(final.CreatedAt).Milliseconds()

	if evs, err := r.Client.Events(ctx, job.ID); err != nil {
		r.logf("  %-18s could not read events: %v", c.Name, err)
	} else {
		res.Revisions = countRevisions(evs)
	}

	after, err := r.Client.Metrics(ctx)
	if err != nil {
		r.logf("  %-18s could not read metrics after the case: %v", c.Name, err)
	} else {
		res.ModelCalls = after.Model.Calls - before.Model.Calls
		res.ModelErrors = after.Model.Errors - before.Model.Errors
		res.InputTokens = after.Model.Tokens.Input - before.Model.Tokens.Input
		res.OutputTokens = after.Model.Tokens.Output - before.Model.Tokens.Output
	}

	// The patch is fetched for failed jobs too. A job that failed its own gate
	// may still have produced a patch that does the task, and a job that passed
	// may have produced one that does not — neither is visible without looking.
	patch, err := r.Client.Diff(ctx, job.ID)
	if err != nil {
		res.Error = fmt.Sprintf("fetch diff: %v", err)
		return res
	}
	res.PatchBytes = len(patch)

	truth, err := CheckPatch(ctx, r.Commands, c, patch)
	if err != nil {
		res.Error = fmt.Sprintf("ground-truth check: %v", err)
		return res
	}
	res.TaskSuccess = truth.Passed
	res.GroundTruthStage = truth.Stage
	res.GroundTruthOutput = truth.Output

	r.logf("  %-18s %s  pipeline=%s truth=%s  %dms  %d rev  %d tok",
		c.Name, verdict(res), yesNo(res.PipelineSuccess), yesNo(res.TaskSuccess),
		res.DurationMs, res.Revisions, res.InputTokens+res.OutputTokens)
	return res
}

// RunAll runs every case in order.
//
// Sequential on purpose: the token and model-call figures are derived from
// deltas on process-wide counters, and concurrent cases would each attribute
// the others' usage to themselves. Concurrency is worth measuring separately,
// with a metric that does not depend on isolation.
func (r *Runner) RunAll(ctx context.Context, cases []Case) []Result {
	results := make([]Result, 0, len(cases))
	for i, c := range cases {
		r.logf("[%d/%d] %s", i+1, len(cases), c.Name)
		results = append(results, r.RunCase(ctx, c))
		if ctx.Err() != nil {
			r.logf("run cancelled after %d of %d cases", i+1, len(cases))
			break
		}
	}
	return results
}

func countRevisions(evs []events.Event) int {
	n := 0
	for _, e := range evs {
		if e.Type == events.TypeRevisionRequested {
			n++
		}
	}
	return n
}

func verdict(r Result) string {
	switch {
	case r.Error != "":
		return "ERROR "
	case r.TaskSuccess:
		return "PASS  "
	case r.FalseSuccess():
		return "FALSE+"
	default:
		return "FAIL  "
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no "
}

// GitAndGoRunner builds the command runner the harness uses for its own work.
//
// It is allowlisted to exactly the two binaries it needs, for the same reason
// the server's runners are: the harness applies patches produced by a model,
// and the allowlist is what keeps "apply this patch" from becoming "run this".
func GitAndGoRunner(timeout time.Duration) execution.CommandRunner {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return execution.NewSafeRunner(
		execution.NewExecRunner(),
		execution.NewAllowlist("git", "go"),
		timeout,
	)
}

// FilterCases narrows a case list to the named ones, preserving order. An empty
// filter keeps everything.
func FilterCases(cases []Case, only []string) ([]Case, error) {
	if len(only) == 0 {
		return cases, nil
	}
	want := make(map[string]bool, len(only))
	for _, n := range only {
		if n = strings.TrimSpace(n); n != "" {
			want[n] = true
		}
	}
	out := make([]Case, 0, len(want))
	for _, c := range cases {
		if want[c.Name] {
			out = append(out, c)
			delete(want, c.Name)
		}
	}
	if len(want) > 0 {
		missing := make([]string, 0, len(want))
		for n := range want {
			missing = append(missing, n)
		}
		return nil, fmt.Errorf("no such case: %s", strings.Join(missing, ", "))
	}
	return out, nil
}
