# Rojo runs a Go toolchain and git as subprocesses — that is the product, not an
# implementation detail. The verification gate shells out to `gofmt`, `go vet`
# and `go test`, and the workspace manager to `git worktree`. So the runtime
# image cannot be scratch or distroless: it has to carry the tools the jobs use.
#
# The build stage is still separate, so the compiler's caches and the module
# download do not end up in the shipped image.

FROM golang:1.25-alpine AS build
WORKDIR /src

# Dependencies first: this layer is rebuilt only when go.mod or go.sum change,
# not on every source edit.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO off gives a static binary, so the runtime stage needs no libc match.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/rojo ./cmd/api

FROM golang:1.25-alpine
# git for worktrees; the Go toolchain comes with the base image. Both are on the
# command allowlists in cmd/api/main.go — nothing else is executable by a job,
# so nothing else needs installing.
RUN apk add --no-cache git ca-certificates

# Jobs run as a non-root user. This is not a sandbox — the threat model is a
# trusted local repository, and the project security model is explicit that containers are not a
# perfect boundary — but a process that never needs root should not have it.
RUN adduser -D -u 10001 rojo

# git refuses to operate on a repository owned by another user unless it is
# marked safe. A mounted host repo is exactly that case, and without this every
# job fails at worktree creation with "detected dubious ownership".
RUN git config --system --add safe.directory '*'

COPY --from=build /out/rojo /usr/local/bin/rojo

# Both are volumes: jobs are the durable record and must outlive the container,
# and worktrees are large, short-lived, and pointless to commit to an image layer.
ENV ROJO_DATA_DIR=/data \
    ROJO_WORKTREE_DIR=/worktrees \
    ROJO_HTTP_ADDR=:8080
RUN mkdir -p /data /worktrees && chown rojo:rojo /data /worktrees
VOLUME ["/data", "/worktrees"]

USER rojo
EXPOSE 8080

# /healthz probes whether the data directory is actually writable, so this
# reports unhealthy for a bad mount rather than merely "the port is open".
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1

ENTRYPOINT ["rojo"]
