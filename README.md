# Rojo

A Go-based AI software-development orchestration platform.

Rojo accepts a software task, creates an isolated Git worktree, plans the work, applies code changes, runs deterministic verification, reviews the resulting diff, and returns an approved patch, revision request, or failure report.

## Requirements

- Go 1.25+ (to build, and for verifying Go repositories)
- Git
- Whatever toolchain the repositories you point it at are verified with — `npm`
  for Node, `pytest` for Python, `cargo` for Rust. A missing toolchain does not
  fail a job; it is recorded as a skipped check.

No database, no services. Rojo stores everything in a directory you point it at.

## Getting started

```bash
make build          # builds bin/rojo-api (server) and bin/rojo (CLI)
bin/rojo-api        # or: make run

# in another terminal, from any git repository:
bin/rojo run "add a retry with backoff to the HTTP client"
```

The server listens on `127.0.0.1:8080` by default — loopback, so only this
machine can reach it — and writes to `./rojo-data`. Jobs, their event history,
and their artifacts persist there, so everything survives a restart out of the
box. Set `ANTHROPIC_API_KEY` **or** `OPENAI_API_KEY` before starting the server
to enable the planner, implementor and reviewer; without either, a job still
isolates, verifies and reports.

`rojo run` streams progress to stderr and writes the finished patch to stdout,
so `rojo run "fix X" > fix.patch` captures the change while you watch it happen;
`-apply` applies it to the repository instead. See [The CLI](#the-cli) below.

## What a job does today

1. The job is validated, persisted and queued.
2. If a model key is set, the planner turns the task into a structured plan.
   Without a key this step is skipped and the rest still runs.
3. A worker creates an isolated git worktree on a `rojo/job/<id>` branch. The
   source repository's working tree and tracked files are never modified.
4. If a key is set, the implementor proposes structured file operations and the
   backend applies them inside the worktree — path-validated and sandboxed, so
   a proposal cannot write outside it however convincing the model was.
5. Deterministic verification runs **inside that worktree**, with the checks
   chosen by what the repository is: `go.mod` runs `gofmt`/`go vet`/`go test`,
   `package.json` with a real test script runs `npm test`, a Python manifest
   runs `pytest`, `Cargo.toml` runs `cargo test`. The repository selects a
   preset by what it contains; it cannot make Rojo invoke an arbitrary binary.
   A toolchain that is not installed, or a repo with no tests, is recorded as a
   note rather than a false pass — "verified" and "compiled" stay distinct.
   (Verification does run the repository's own test code — see
   [Security model](#security-model).)
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
| `ANTHROPIC_API_KEY`       | *(unset)*                   | Claude key for the planner, implementor and reviewer |
| `OPENAI_API_KEY`          | *(unset)*                   | OpenAI key, as an alternative backend. With neither key set, the agents are disabled |
| `ROJO_PROVIDER`           | *(inferred from the key set)* | `anthropic` or `openai`. Only needed when both keys are present |
| `ROJO_MODEL`              | provider default            | Model the agents use (`claude-opus-4-8` / `gpt-5.2`) |
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

One process owns the directory at a time. The server takes an exclusive lock on
`ROJO_DATA_DIR/.lock` at startup, so a second server pointed at the same
directory refuses to start rather than corrupting shared state — a second
`make run` fails with a clear message instead of quietly clobbering the first.

## Model providers

Rojo talks to either Claude or OpenAI. The planner, implementor and reviewer all
depend on one small interface (`model.Client`), so they never learn which
provider answered — switching is configuration, not code.

```bash
export ANTHROPIC_API_KEY=sk-ant-...   # Claude (default model: claude-opus-4-8)
export OPENAI_API_KEY=sk-...          # OpenAI (default model: gpt-5.2)
```

With one key set, that provider is used — no other configuration needed. With
both set, Claude wins unless you say otherwise, so adding an `OPENAI_API_KEY` to
an environment never silently changes what an existing deployment runs:

```bash
export ROJO_PROVIDER=openai           # explicit choice
export ROJO_MODEL=gpt-4.1             # optional: override the provider's default
```

A `ROJO_PROVIDER` that names an unknown provider, or one whose key is missing,
refuses to start rather than quietly falling back to the other.

Both clients use their vendor's official Go SDK, and both are tested against a
stubbed endpoint through the real SDK — the request that goes on the wire is
asserted, not assumed. The pipeline tests run end-to-end on both and compare the
resulting patches, so a provider-specific quirk would surface as a test failure
rather than in production.

## Security model

Rojo runs your repository's own code. Verification exists to run the tests, and
tests are code: `go test` runs the repo's `_test.go`, `pytest` runs its test
files and `conftest.py`, `cargo test` runs its `build.rs`, and `npm test` runs
whatever shell command the repository put in `package.json`'s `scripts.test`.
The command allowlist stops a repository from making Rojo invoke an arbitrary
binary like `rm`; it does **not** stop the repository's own test or build code
from doing whatever it wants. That is inherent to running tests, not a defect.

So the trust boundary is the repository, not the allowlist:

- **Point Rojo at repositories you trust** — code you would run `go test` /
  `npm test` on yourself. That is the intended model: one trusted local
  repository.
- **To verify untrusted code, use the sandbox.** A containerized runner
  (`internal/execution.DockerRunner`) exists to run verification with no network
  and bounded CPU/memory. It is deliberately **not** wired in by default —
  running it needs a mounted Docker socket, which is its own decision — so out of
  the box, verifying a repository executes that repository's code on the host.
- **Keep the server off the network** unless you have set `ROJO_AUTH_TOKEN`. The
  default bind is loopback for exactly this reason: the job endpoint runs code.

None of this is unique to Rojo — every CI system executes the code it tests —
but it is the thing to understand before pointing it at something you did not
write.

## The CLI

`rojo` talks to a running server over the same HTTP API as any other client —
it has no privileged path.

```
rojo run [-repo DIR] [-apply] "task"   submit, stream progress, deliver the patch
rojo list [-limit N]                   recent jobs, newest first
rojo get JOB_ID                        one job's status and task
rojo events JOB_ID                     a job's event history
rojo diff JOB_ID                       print a job's patch
rojo cancel JOB_ID                     ask the server to stop a job

  -server URL    default $ROJO_SERVER or http://127.0.0.1:8080
  -token TOKEN   default $ROJO_AUTH_TOKEN
```

`run` uses the current directory unless `-repo` is given. Progress goes to
stderr and the patch to stdout, so redirection captures the patch cleanly.
`-apply` writes the change into the repository — but never for a failed job,
whose patch is printed for inspection instead of silently applied. First Ctrl-C
asks the server to cancel; a second stops watching while the server finishes.
Exit codes: `0` completed, `1` failed, `2` cancelled.

## Architecture

```
cmd/api                 HTTP server entrypoint
cmd/rojo                Command-line client
internal/api            HTTP handlers, middleware, rate limiting, metrics endpoint
internal/jobs           Job domain: struct, status, transitions, validation
internal/queue          Buffered in-process job queue
internal/worker         Worker pool
internal/orchestration  Processor: the plan→implement→verify→review pipeline
internal/execution      CommandRunner with allowlist and timeouts
internal/workspace      Git worktree manager
internal/verification   Stack detection + deterministic check runner
internal/agents         Model client, planner, implementor, reviewer
internal/repocontext    Picks the files worth showing a model (git ls-files / git grep)
internal/metrics        Counters: job outcomes, durations, queue wait, model calls
internal/events         Event bus + persistence
internal/storage/filestore  Durable job/event/artifact storage, single-writer locked
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

