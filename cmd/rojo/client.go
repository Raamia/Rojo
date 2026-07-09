package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Raamia/Rojo/internal/events"
)

// job mirrors the API's job JSON. Declared here rather than importing
// internal/jobs because the CLI is a client: it should depend on the wire
// format the server publishes, not reach into server internals — that is the
// same discipline any third-party client would be held to.
type job struct {
	ID        string    `json:"ID"`
	Task      string    `json:"Task"`
	RepoPath  string    `json:"RepoPath"`
	Status    string    `json:"Status"`
	CreatedAt time.Time `json:"CreatedAt"`
	UpdatedAt time.Time `json:"UpdatedAt"`
}

// apiError is the {"error": "..."} body every non-2xx response carries.
type apiError struct {
	Message string `json:"error"`
}

// client is a thin, explicit wrapper over the HTTP API. Every method returns
// errors that name what the caller was doing — a CLI's error text is its
// entire debugging experience.
type client struct {
	base  string
	token string
	http  *http.Client
}

func newClient(base, token string) *client {
	return &client{
		base:  base,
		token: token,
		// A bounded client: the CLI polls, so no single request should hang
		// longer than a poll interval is worth waiting.
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *client) do(method, path string, body any) (*http.Response, error) {
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, buf)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the rojo server at %s: %w\n(start it with `make run`, or point --server at it)", c.base, err)
	}
	return resp, nil
}

// decode reads a JSON body into out, translating error responses into the
// server's own message rather than a bare status code.
func decode(resp *http.Response, out any) error {
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return asError(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func asError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e apiError
	if json.Unmarshal(b, &e) == nil && e.Message != "" {
		return fmt.Errorf("%s (HTTP %d)", e.Message, resp.StatusCode)
	}
	return fmt.Errorf("server returned HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(b))
}

func (c *client) createJob(task, repoPath string) (job, error) {
	resp, err := c.do(http.MethodPost, "/api/v1/jobs",
		map[string]string{"task": task, "repo_path": repoPath})
	if err != nil {
		return job{}, err
	}
	var j job
	if err := decode(resp, &j); err != nil {
		return job{}, fmt.Errorf("create job: %w", err)
	}
	return j, nil
}

func (c *client) getJob(id string) (job, error) {
	resp, err := c.do(http.MethodGet, "/api/v1/jobs/"+id, nil)
	if err != nil {
		return job{}, err
	}
	var j job
	if err := decode(resp, &j); err != nil {
		return job{}, fmt.Errorf("get job %s: %w", id, err)
	}
	return j, nil
}

func (c *client) listJobs(limit int) ([]job, string, error) {
	resp, err := c.do(http.MethodGet, fmt.Sprintf("/api/v1/jobs?limit=%d", limit), nil)
	if err != nil {
		return nil, "", err
	}
	total := resp.Header.Get("X-Total-Count")
	var list []job
	if err := decode(resp, &list); err != nil {
		return nil, "", fmt.Errorf("list jobs: %w", err)
	}
	return list, total, nil
}

func (c *client) jobEvents(id string) ([]events.Event, error) {
	resp, err := c.do(http.MethodGet, "/api/v1/jobs/"+id+"/events", nil)
	if err != nil {
		return nil, err
	}
	var evs []events.Event
	if err := decode(resp, &evs); err != nil {
		return nil, fmt.Errorf("events for %s: %w", id, err)
	}
	return evs, nil
}

// jobDiff returns the patch, or ("", nil) when the job has not produced one —
// the caller decides whether that is worth mentioning.
func (c *client) jobDiff(id string) (string, error) {
	resp, err := c.do(http.MethodGet, "/api/v1/jobs/"+id+"/diff", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, resp.Body)
		return "", nil
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("diff for %s: %w", id, asError(resp))
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read diff: %w", err)
	}
	return string(b), nil
}

func (c *client) cancelJob(id string) error {
	resp, err := c.do(http.MethodPost, "/api/v1/jobs/"+id+"/cancel", nil)
	if err != nil {
		return err
	}
	if err := decode(resp, nil); err != nil {
		return fmt.Errorf("cancel %s: %w", id, err)
	}
	return nil
}
