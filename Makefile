.PHONY: lint test build check dev dev-down

lint:
	golangci-lint run ./...

test:
	go test ./... -race -count=1

build:
	go build ./...

check: lint test

dev:
	docker compose up --build

dev-down:
	docker compose down
