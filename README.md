# Rojo

A Go-based AI software-development orchestration platform.

Rojo accepts a software task, creates an isolated Git worktree, plans the work, applies code changes, runs deterministic verification, reviews the resulting diff, and returns an approved patch, revision request, or failure report.

## Requirements

- Go 1.23+
- Git
- PostgreSQL
- Docker

## Getting started

```bash
# Bring up postgres
make db-up
make migrate-up

# Run the API
ROJO_DB_URL=postgres://rojo:rojo@localhost:5432/rojo?sslmode=disable make run
```

The server listens on `:8080` by default.

## Configuration

| Env var                    | Default                     | Description                       |
| -------------------------- | --------------------------- | --------------------------------- |
| `ROJO_HTTP_ADDR`          | `:8080`                     | API listen address                |
| `ROJO_DB_URL`             | *(unset → in-memory repo)*  | Postgres connection URL           |
| `ROJO_QUEUE_BUFFER`       | `64`                        | Job queue capacity                |
| `ROJO_WORKER_COUNT`       | `4`                         | Worker pool size                  |
| `ROJO_WORKTREE_DIR`       | `/tmp/rojo-worktrees`      | Root dir for job worktrees        |
| `ROJO_SHUTDOWN_TIMEOUT`   | `15s`                       | Graceful shutdown deadline        |

## API

| Method | Path                                | Description             |
| ------ | ----------------------------------- | ----------------------- |
| POST   | `/api/v1/jobs`                      | Create a job            |
| GET    | `/api/v1/jobs`                      | List jobs               |
| GET    | `/api/v1/jobs/{jobID}`              | Get one job             |
| POST   | `/api/v1/jobs/{jobID}/cancel`       | Cancel a running job    |
| GET    | `/api/v1/jobs/{jobID}/events`       | Job event history       |
| GET    | `/api/v1/jobs/{jobID}/stream`       | Live event WebSocket    |

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
internal/events         Event bus + postgres store
internal/storage/postgres  pgx-backed JobRepository
migrations              SQL migrations (goose)
tests                   End-to-end tests
```

