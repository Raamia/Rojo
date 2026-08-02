.PHONY: build test race run lint fmt check bench docker docker-run

# Three binaries: the server, the CLI that talks to it, and the MCP server.
#
# rojo-mcp shares no state with the other two — no data directory, no lock, no
# API key — so it runs beside a running rojo-api or entirely on its own.
build:
	go build -o bin/rojo-api ./cmd/api
	go build -o bin/rojo ./cmd/rojo
	go build -o bin/rojo-mcp ./cmd/rojo-mcp
	go build -o bin/rojo-bench ./cmd/rojo-bench

# Measure the pipeline against the fixed case set. Needs a running server, and
# a model key on that server for the numbers to mean anything: with no key the
# agents are disabled, every case reports a false success, and the run proves
# only that the harness works.
#
# Prices are passed in rather than baked in, because a hardcoded rate silently
# becomes a wrong number the next time a provider changes one.
bench:
	go build -o bin/rojo-bench ./cmd/rojo-bench
	bin/rojo-bench -json bench-report.json $(BENCH_FLAGS)

test:
	go test ./...

# What CI runs. The concurrent parts — queue, worker pool, event bus, fan-out —
# are where the bugs that only appear under load live.
race:
	go test -race ./...

run:
	go run ./cmd/api

lint:
	go vet ./...

fmt:
	gofmt -w .

# The same gates as CI, in the same order, so a green `make check` means a green
# pipeline rather than a hopeful one.
check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt'd:"; echo "$$unformatted"; exit 1; \
	fi
	go vet ./...
	go test -race ./...

docker:
	docker build -t rojo:latest .

# The repository mount is read-write, and it has to be: `git worktree add`
# creates a branch ref under .git/refs/heads/ and an admin entry under
# .git/worktrees/, both inside the source repository. A read-only mount fails at
# worktree creation with "cannot lock ref" — confirmed by trying it.
#
# The isolation guarantee is narrower than "never writes here" and worth stating
# precisely: Rojo never modifies the source repository's working tree or its
# tracked files. Every change a job makes happens in a worktree cut from it, and
# cleanup removes both that worktree and the branch.
#
# Run REPO=/abs/path/to/repo make docker-run. The path is mounted at the same
# path inside the container so the repo_path in a job request means the same
# thing on both sides.
docker-run:
	docker run --rm -p 8080:8080 \
		-v rojo-data:/data \
		-v $(REPO):$(REPO) \
		-e ANTHROPIC_API_KEY \
		-e ROJO_AUTH_TOKEN \
		rojo:latest
