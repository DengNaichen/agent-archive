.PHONY: build test fmt fmt-check vet check install
build:
	CGO_ENABLED=0 go build -trimpath -o bin/agent-archive ./cmd/agent-archive
test:
	go test ./...
fmt:
	gofmt -w cmd internal
fmt-check:
	@test -z "$$(gofmt -l cmd internal)" || (gofmt -l cmd internal; exit 1)
vet:
	go vet ./...
check: fmt-check vet test build
install:
	CGO_ENABLED=0 go install ./cmd/agent-archive
