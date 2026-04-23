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
make test
make run
```
