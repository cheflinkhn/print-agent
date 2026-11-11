VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
# No C code anywhere, so every target is one self-contained file.
export CGO_ENABLED := 0

.PHONY: all test vet build windows linux-arm64 fakeserver clean

all: vet test build windows

test:
	go test ./...

vet:
	go vet ./...
	GOOS=windows GOARCH=amd64 go vet ./...

# The agent for this machine.
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o dist/agent ./cmd/agent

# The agent for a cashier PC (64-bit Windows 10 or later).
windows:
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/cheflink-print-agent.exe ./cmd/agent

# The agent for a Raspberry Pi or similar, for a venue without a cashier PC.
linux-arm64:
	GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/agent-linux-arm64 ./cmd/agent

# The stand-in for Cheflink, for development.
fakeserver:
	go build -trimpath -o dist/fakeserver ./cmd/fakeserver

clean:
	rm -rf dist
