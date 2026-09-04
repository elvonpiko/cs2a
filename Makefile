BINARY_AGENT := cs2a-agent
BINARY_PANEL := cs2a-panel
VERSION ?= dev

LDFLAGS := -s -w -X cs2a/internal/version.Version=$(VERSION)

.PHONY: all generate test build clean

all: test build

## generate: render templ templates into Go code
generate:
	go tool templ generate

## test: run all Go tests
test:
	go test ./...

## build: build agent + panel binaries into dist/
build: generate
	mkdir -p dist
	go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY_AGENT) ./cmd/cs2a-agent
	go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY_PANEL) ./cmd/cs2a-panel

## clean: remove build output
clean:
	rm -rf dist
