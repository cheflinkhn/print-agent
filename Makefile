VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
# No C code anywhere, so every target is one self-contained file.
export CGO_ENABLED := 0

.PHONY: all test vet build windows linux-arm64 fakeserver release clean

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

# What a GitHub release carries; .github/workflows/release.yml runs this for
# a version tag. The names hold no version, so a link to
# releases/latest/download/NAME always gives the newest.
RELEASE := dist/release
release: windows linux-arm64
	rm -rf $(RELEASE)
	mkdir -p $(RELEASE)
	cp dist/cheflink-print-agent.exe $(RELEASE)/cheflink-print-agent.exe
	cp dist/agent-linux-arm64 $(RELEASE)/cheflink-print-agent-linux-arm64
	zip -j -q $(RELEASE)/cheflink-print-agent-windows.zip \
		dist/cheflink-print-agent.exe installer/install.cmd installer/uninstall.cmd installer/install.ps1
	cd $(RELEASE) && sha256sum cheflink-print-agent-windows.zip cheflink-print-agent.exe cheflink-print-agent-linux-arm64 > SHA256SUMS

clean:
	rm -rf dist
