GO ?= go
GO_GOTOOLCHAIN ?= go1.27.3
GUI_GOTOOLCHAIN ?= $(GO_GOTOOLCHAIN)
VERSION ?= $(shell git describe --tags --exact-match HEAD 2>/dev/null || printf dev)
WINDOWS_TARGET ?= x86_64-pc-windows-gnu

.PHONY: build check check-go check-gui check-agent

# Exact release tags supply the CLI version; VCS metadata retains dirty detection.
build:
	GOTOOLCHAIN=$(GO_GOTOOLCHAIN) $(GO) build -ldflags "-X main.version=$(VERSION)" -o bin/rdp-launcher ./cmd/rdp-launcher
	cd cmd/rdp-launcher-dialog && GOTOOLCHAIN=$(GUI_GOTOOLCHAIN) $(GO) build -o ../../bin/rdp-launcher-dialog .

# Root ./... intentionally does not include the independent GUI module.
check: check-go check-gui check-agent

check-go:
	GOTOOLCHAIN=$(GO_GOTOOLCHAIN) $(GO) build ./...
	GOTOOLCHAIN=$(GO_GOTOOLCHAIN) $(GO) vet ./...
	GOTOOLCHAIN=$(GO_GOTOOLCHAIN) $(GO) test -race ./...

check-gui:
	cd cmd/rdp-launcher-dialog && GOTOOLCHAIN=$(GUI_GOTOOLCHAIN) $(GO) build -o /dev/null .
	cd cmd/rdp-launcher-dialog && GOTOOLCHAIN=$(GUI_GOTOOLCHAIN) $(GO) vet ./...
	cd cmd/rdp-launcher-dialog && GOTOOLCHAIN=$(GUI_GOTOOLCHAIN) $(GO) test -race ./...

# Cross-compilation checks the Windows test binary; it does not execute it.
check-agent:
	cargo clippy --manifest-path windows-agent/Cargo.toml --target $(WINDOWS_TARGET) -- -D warnings
	cargo test --manifest-path windows-agent/Cargo.toml --target $(WINDOWS_TARGET) --no-run
