GO ?= go
GO_GOTOOLCHAIN ?= go1.27.3
VERSION ?= $(shell git describe --tags --exact-match HEAD 2>/dev/null || printf dev)
WINDOWS_TARGET ?= x86_64-pc-windows-gnu

.PHONY: build check check-go check-agent

# Exact release tags supply the CLI version; VCS metadata retains dirty detection.
build:
	GOTOOLCHAIN=$(GO_GOTOOLCHAIN) $(GO) build -ldflags "-X main.version=$(VERSION)" -o bin/rdp-launcher ./cmd/rdp-launcher

check: check-go check-agent

check-go:
	GOTOOLCHAIN=$(GO_GOTOOLCHAIN) $(GO) build ./...
	GOTOOLCHAIN=$(GO_GOTOOLCHAIN) $(GO) vet ./...
	GOTOOLCHAIN=$(GO_GOTOOLCHAIN) $(GO) test -race ./...

# Cross-compilation checks the Windows test binary; it does not execute it.
check-agent:
	cargo clippy --manifest-path windows-agent/Cargo.toml --target $(WINDOWS_TARGET) -- -D warnings
	cargo test --manifest-path windows-agent/Cargo.toml --target $(WINDOWS_TARGET) --no-run
