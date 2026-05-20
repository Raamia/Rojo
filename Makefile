.PHONY: test run lint fmt db-up db-down migrate-up migrate-down

DB_URL ?= postgres://rojo:rojo@localhost:5432/rojo?sslmode=disable

test:
	go test ./...

run:
	go run ./cmd/api

lint:
	go vet ./...

fmt:
	gofmt -w .

db-up:
	docker compose up -d postgres

db-down:
	docker compose down

migrate-up:
	goose -dir migrations postgres "$(DB_URL)" up

migrate-down:
	goose -dir migrations postgres "$(DB_URL)" down
