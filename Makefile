.PHONY: lint test build check

lint:
	golangci-lint run ./...

test:
	go test ./... -race -count=1

build:
	go build ./...

check: lint test
