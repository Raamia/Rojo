package benchmark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Raamia/Rojo/internal/events"
)

// Client talks to a running rojo-api over its public HTTP API.
//
// The benchmark deliberately goes through HTTP rather than embedding the
// processor: queue wait is one of the numbers being measured, and it only
// exists when a job actually queues. Measuring an in-process call would report
// a system nobody runs.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL: baseURL,
		Token:   token,
		// Generous: a single request here is cheap, but the server may be busy
		// verifying another case when this one polls.
		HTTP: &http.Client{Timeout: 60 * time.Second},
	}
}

// Job is the subset of the API's job representation the benchmark reads.
type Job struct {
	ID        string    `json:"ID"`
	Task      string    `json:"Task"`
	Status    string    `json:"Status"`
	CreatedAt time.Time `json:"CreatedAt"`
	UpdatedAt time.Time `json:"UpdatedAt"`
}

// Terminal reports whether the job has stopped moving.
func (j Job) Terminal() bool {
	switch j.Status {
	case "completed", "failed", "cancelled":
		return true
	}
	return false
}

// Metrics is the subset of the metrics snapshot the benchmark reads. Token
// totals are process-wide counters, so a per-case figure is the difference
// between a snapshot taken before the case and one taken after — which is only
// valid while cases run one at a time.
type Metrics struct {
	Model struct {
		Calls  int64 `json:"calls"`
		Errors int64 `json:"errors"`
		Tokens struct {
			Input  int64 `json:"input"`
			Output int64 `json:"output"`
		} `json:"tokens"`
	} `json:"model"`
	RevisionsRequested int64 `json:"revisions_requested"`
}

func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, buf)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the rojo server at %s: %w "+
			"(start it with `bin/rojo-api`)", c.BaseURL, err)
	}
	return resp, nil
}

func decodeInto(resp *http.Response, out any) error {
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) CreateJob(ctx context.Context, task, repoPath string) (Job, error) {
	resp, err := c.do(ctx, http.MethodPost, "/api/v1/jobs",
		map[string]string{"task": task, "repo_path": repoPath})
	if err != nil {
		return Job{}, err
	}
	var j Job
	if err := decodeInto(resp, &j); err != nil {
		return Job{}, fmt.Errorf("create job: %w", err)
	}
	return j, nil
}

func (c *Client) GetJob(ctx context.Context, id string) (Job, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/v1/jobs/"+id, nil)
	if err != nil {
		return Job{}, err
	}
	var j Job
	if err := decodeInto(resp, &j); err != nil {
		return Job{}, fmt.Errorf("get job %s: %w", id, err)
	}
	return j, nil
}

func (c *Client) Events(ctx context.Context, id string) ([]events.Event, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/v1/jobs/"+id+"/events", nil)
	if err != nil {
		return nil, err
	}
	var evs []events.Event
	if err := decodeInto(resp, &evs); err != nil {
		return nil, fmt.Errorf("events for %s: %w", id, err)
	}
	return evs, nil
}

// Diff returns the job's patch, or "" when it produced none. A job with no
// patch is a real outcome, not an error — the model may have proposed nothing.
func (c *Client) Diff(ctx context.Context, id string) (string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/v1/jobs/"+id+"/diff", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", nil
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return "", fmt.Errorf("diff for %s: HTTP %d: %s", id, resp.StatusCode, bytes.TrimSpace(b))
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read diff: %w", err)
	}
	return string(b), nil
}

func (c *Client) Metrics(ctx context.Context) (Metrics, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/v1/metrics", nil)
	if err != nil {
		return Metrics{}, err
	}
	var m Metrics
	if err := decodeInto(resp, &m); err != nil {
		return Metrics{}, fmt.Errorf("metrics: %w", err)
	}
	return m, nil
}

// Health confirms a server is up before a run starts, so a misconfigured
// address fails immediately rather than after the first case's timeout.
func (c *Client) Health(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/healthz", nil)
	if err != nil {
		return err
	}
	return decodeInto(resp, nil)
}

// WaitForTerminal polls until the job stops moving or ctx expires.
//
// Polling rather than the WebSocket stream: the benchmark needs the final
// state and the full event log, both of which the REST endpoints give exactly,
// and a dropped socket mid-run would lose a case for a reason that has nothing
// to do with what is being measured.
func (c *Client) WaitForTerminal(ctx context.Context, id string, poll time.Duration) (Job, error) {
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	// last carries the most recent status across iterations so a timeout can
	// say what the job was doing rather than just that time ran out.
	last := Job{ID: id, Status: "unknown"}
	for {
		job, err := c.GetJob(ctx, id)
		if err != nil {
			// The deadline usually expires partway through this request, not
			// while waiting on the ticker below, and the transport reports that
			// as a connection failure. Reporting it verbatim would blame the
			// server — telling the reader to go start one that is already
			// running — for what is really a job that did not finish in time.
			if ctx.Err() != nil {
				return last, fmt.Errorf("job %s still %s when the benchmark gave up: %w",
					id, last.Status, ctx.Err())
			}
			return Job{}, err
		}
		last = job
		if job.Terminal() {
			return job, nil
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("job %s still %s when the benchmark gave up: %w",
				id, last.Status, ctx.Err())
		case <-ticker.C:
		}
	}
}
