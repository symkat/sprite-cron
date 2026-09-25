.PHONY: build test check fmt
build:
	CGO_ENABLED=0 go build -trimpath -o bin/sprite-cron ./cmd/sprite-cron
test:
	go test -race ./...
check:
	test -z "$$(gofmt -l cmd internal)"
	go vet ./...
	go test -race ./...
fmt:
	gofmt -w cmd internal
