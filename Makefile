.PHONY: build run test lint install image
build:      ; go build -ldflags "-s -w -X main.version=$$(git describe --tags --always --dirty 2>/dev/null || echo dev)" -o prb ./cmd/prb
run:        ; go run ./cmd/prb
test:       ; go test ./...
lint:       ; gofmt -l . && go vet ./...
install:    ; go install -ldflags "-s -w" ./cmd/prb
image:      ; go run ./cmd/prb build-image
