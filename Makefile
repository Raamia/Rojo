.PHONY: test run lint fmt

test:
	go test ./...

run:
	go run ./cmd/api

lint:
	go vet ./...

fmt:
	gofmt -w .
