# Rojo

A Go-based AI software-development orchestration platform.

Rojo accepts a software task, creates an isolated Git worktree, plans the work, applies code changes, runs deterministic verification, reviews the resulting diff, and returns an approved patch, revision request, or failure report.

## Requirements

- Go 1.25+
- Git

That's it — no database, no Docker, no services. Rojo stores everything in a
directory you point it at.

## Getting started

```bash
make run
```

The server listens on `:8080` by default and writes to `./rojo-data`. Jobs,
their event history, and their artifacts persist there, so everything survives a
restart out of the box.

## What a job does today

1. The job is validated, persisted and queued.
2. If `ANTHROPIC_API_KEY` is set, the planner turns the task into a structured
   plan. Without a key this step is skipped and the rest still runs.
3. A worker creates an isolated git worktree on a `rojo/job/<id>` branch. The
   source repository's working tree and tracked files are never modified.
4. If a key is set, the implementor proposes structured file operations and the
   backend applies them inside the worktree — path-validated and sandboxed, so
   a proposal cannot write outside it however convincing the model was.
5. Deterministic verification runs **inside that worktree**: `gofmt -l .`,
   `go vet ./...`, `go test ./...`.
6. The reviewer judges whether the change did what was asked. It only ever sees
   a change that already passed the checks — deterministic results outrank model
   judgement, so a change that does not build is not something it gets an
   opinion about.
7. Approved jobs complete. A failing gate or a reviewer asking for changes sends
   the job back through implement → verify → review **once**, with the failing
   check output as the feedback, which is how a job fixes its own broken test.
8. The winning patch is saved as an artifact, readable at
   `GET /api/v1/jobs/{id}/diff`. A **failed** job keeps its patch too — the
   change that failed the tests is what you read to find out why.
9. Either way the worktree and its branch are removed.

On startup Rojo reconciles the stored jobs with its empty queue: jobs still marked
`queued` are re-enqueued, and jobs interrupted mid-flight by the previous
process are marked `failed` and have their worktrees reclaimed.

Set `ROJO_FANOUT_VARIANTS` above 1 to attempt each job several ways at once:
every variant gets its own worktree, all are verified concurrently, and the
first that passes every check wins. Today the variants do identical work — the
value arrives once an agent produces genuinely different candidates.

Every job is bounded by `ROJO_JOB_TIMEOUT`. A job that runs out of time ends
`failed` (not `cancelled` — that is reserved for a caller asking it to stop) and
still has its worktree reclaimed.

## Configuration

| Env var                    | Default                     | Description                       |
| -------------------------- | --------------------------- | --------------------------------- |
| `ROJO_HTTP_ADDR`          | `127.0.0.1:8080`            | API listen address. Loopback by default |
| `ROJO_DATA_DIR`           | `./rojo-data`               | Where jobs, events and artifacts are stored |
| `ROJO_QUEUE_BUFFER`       | `64`                        | Job queue capacity                |
| `ROJO_WORKER_COUNT`       | `4`                         | Worker pool size                  |
| `ROJO_WORKTREE_DIR`       | `/tmp/rojo-worktrees`       | Root dir for job worktrees        |
| `ROJO_SHUTDOWN_TIMEOUT`   | `15s`                       | Graceful shutdown deadline        |
| `ROJO_JOB_TIMEOUT`        | `30m`                       | Maximum wall-clock time for one job |
| `ROJO_FANOUT_VARIANTS`    | `1`                         | Attempts per job, each in its own worktree (max 8) |
| `ANTHROPIC_API_KEY`       | *(unset → agents disabled)* | API key for the planner, implementor and reviewer |
| `ROJO_MODEL`              | `claude-opus-4-8`           | Model the agents use |
| `ROJO_AUTH_TOKEN`         | *(unset → **auth disabled**)* | Bearer token required on every route except `/healthz` |
| `ROJO_RATE_LIMIT_BURST`   | `30`                        | Token-bucket capacity per client IP |
| `ROJO_RATE_LIMIT_RPS`     | `5`                         | Token refill per second           |
| `ROJO_TRUST_PROXY_HEADER` | `false`                     | Key rate limits on `X-Forwarded-For`. Enable only behind a reverse proxy you control |

> **Rojo binds loopback by default.** Job submission executes code, and
> authentication is off until `ROJO_AUTH_TOKEN` is set. If you expose the
> service with `ROJO_HTTP_ADDR`, set a token at the same time.

Values that cannot be parsed fall back to the default **without an error**, so a
typo like `ROJO_WORKER_COUNT=0x10` silently yields 4 workers. Check the startup
log if a setting does not appear to take effect.

## API

| Method | Path                                | Description             |
| ------ | ----------------------------------- | ----------------------- |
| POST   | `/api/v1/jobs`                      | Create a job            |
| GET    | `/api/v1/jobs`                      | List jobs, newest first. `?limit=` (default 50, max 200) and `?offset=`; total in the `X-Total-Count` header |
| GET    | `/api/v1/jobs/{jobID}`              | Get one job             |
| POST   | `/api/v1/jobs/{jobID}/cancel`       | Cancel a running job    |
| GET    | `/api/v1/jobs/{jobID}/events`       | Job event history |
| GET    | `/api/v1/jobs/{jobID}/diff`         | The job's patch, as `text/x-diff` — pipe it into `git apply` |
| GET    | `/api/v1/jobs/{jobID}/stream`       | Live event WebSocket    |
| GET    | `/api/v1/metrics`                   | Counters: job outcomes and durations, queue wait, model calls/latency, active jobs |
| GET    | `/healthz`                          | Liveness. Probes that the data directory is actually writable; `503` when it is not |

## Storage

Everything lives under `ROJO_DATA_DIR`, one directory per job:

```
rojo-data/jobs/<job-id>/
  job.json       current state
  events.jsonl   append-only event log
  patch.diff     the job's patch, when it produced one
```

Plain files on purpose — a patch you can `git apply`, a log you can `cat`, and
no schema to migrate.

## Architecture

```
cmd/api        HTTP server entrypoint
internal/api            HTTP handlers, middleware
internal/jobs           Job domain: struct, status, transitions, validation
internal/queue          Buffered in-process job queue
internal/worker         Worker pool
internal/orchestration  Processor + cancellation tracker
internal/execution      CommandRunner with allowlist and timeouts
internal/workspace      Git worktree manager
internal/verification   Deterministic check runner (gofmt, go vet, go test)
internal/agents         Model client, planner, implementor, reviewer
internal/repocontext    Picks the files worth showing a model (git ls-files / git grep)
internal/events         Event bus + persistence
internal/storage/filestore  Durable job, event and artifact storage on disk
tests                   End-to-end tests
```

## Docker

The image ships the Go toolchain and git, because that is what jobs run — a
scratch or distroless image would build fine and then fail every verification.

```bash
make docker
REPO=/absolute/path/to/your/repo make docker-run
```

The repository mount is **read-write**, and has to be: `git worktree add` writes
a branch ref and an admin entry inside the source repository's `.git`. What Rojo
never touches is the source working tree or its tracked files — every change
lives in the worktree, and cleanup removes both the worktree and the branch.

The repo is mounted at the same path inside the container as outside, so the
`repo_path` in a job request means the same thing on both sides.

## Development

```bash
make check   # the same gates CI runs: gofmt, go vet, go test -race
```

CI runs those on every push, plus `govulncheck` and a Docker image build.

