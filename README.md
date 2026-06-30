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
3. A worker creates an isolated git worktree on a `rojo/job/<id>` branch, so the
   original repository is never modified.
4. Deterministic verification runs **inside that worktree**: `gofmt -l .`,
   `go vet ./...`, `go test ./...`.
5. If every check passes the job completes; if any fails the job ends `failed`
   with a summary. Either way the worktree and its branch are removed.

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

The implementor and reviewer are still scaffolded but not part of this pipeline
The known limitations are documented below.

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
| `ANTHROPIC_API_KEY`       | *(unset → planning disabled)* | API key for the planner |
| `ROJO_MODEL`              | `claude-opus-4-8`           | Model the planner uses |
| `ROJO_AUTH_TOKEN`         | *(unset → **auth disabled**)* | Bearer token required on every route except `/healthz` |
| `ROJO_RATE_LIMIT_BURST`   | `30`                        | Token-bucket capacity per client IP |
| `ROJO_RATE_LIMIT_RPS`     | `5`                         | Token refill per second           |

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
| GET    | `/api/v1/jobs`                      | List jobs               |
| GET    | `/api/v1/jobs/{jobID}`              | Get one job             |
| POST   | `/api/v1/jobs/{jobID}/cancel`       | Cancel a running job    |
| GET    | `/api/v1/jobs/{jobID}/events`       | Job event history |
| GET    | `/api/v1/jobs/{jobID}/stream`       | Live event WebSocket    |

## Storage

Everything lives under `ROJO_DATA_DIR`, one directory per job:

```
rojo-data/jobs/<job-id>/
  job.json       current state
  events.jsonl   append-only event log
  diff.patch     artifacts, when the job produces them
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
internal/agents         Model client, planner, implementor, reviewer (not yet wired)
internal/events         Event bus + postgres store
internal/storage/filestore  Durable job, event and artifact storage on disk
tests                   End-to-end tests
```

