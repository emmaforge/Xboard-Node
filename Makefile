# The displayed version comes from internal/version (a plain constant),
# not from git: build-time hash injection made `xbnode version` print a
# 40-char commit SHA. Bump internal/version/version.go on each release.
LDFLAGS := -s -w

.PHONY: build clean test install build-linux build-linux-arm64 build-all

# Build for current platform
build:
	go build -ldflags "$(LDFLAGS)" -tags "with_quic with_utls with_wireguard with_clash_api" -o xboard-node ./cmd/xboard-node
	go build -ldflags "$(LDFLAGS)" -o xbnode ./cmd/xbnode

# Build for Linux amd64
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -tags "with_quic with_utls with_wireguard with_acme with_clash_api" -o xboard-node-linux-amd64 ./cmd/xboard-node
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o xbnode-linux-amd64 ./cmd/xbnode

# Build for Linux arm64
build-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -tags "with_quic with_utls with_wireguard with_acme with_clash_api" -o xboard-node-linux-arm64 ./cmd/xboard-node
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o xbnode-linux-arm64 ./cmd/xbnode

# Build all platforms
build-all: build-linux build-linux-arm64

# Run tests
test:
	go test -v -race -count=1 ./internal/...

# Clean build artifacts
clean:
	rm -f xboard-node xbnode xboard-node-linux-* xbnode-linux-*

# Install to system (single node, legacy compat)
install: build
	sudo cp xboard-node /usr/local/bin/
	sudo cp xbnode /usr/local/bin/
	sudo mkdir -p /etc/xboard-node
	@if [ ! -f /etc/xboard-node/config.yml ]; then \
		sudo cp config.yml.example /etc/xboard-node/config.yml; \
		echo "Config copied to /etc/xboard-node/config.yml - please edit it"; \
	fi
