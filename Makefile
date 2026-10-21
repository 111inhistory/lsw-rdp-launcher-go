GO ?= go
GUI_GOTOOLCHAIN ?= go1.27.1
WINDOWS_TARGET ?= x86_64-pc-windows-gnu

.PHONY: check check-go check-gui check-agent

# Root ./... intentionally does not include the independent GUI module.
check: check-go check-gui check-agent

check-go:
	$(GO) build ./...
	$(GO) vet ./...
	$(GO) test -race ./...

check-gui:
	cd cmd/rdp-launcher-dialog && GOTOOLCHAIN=$(GUI_GOTOOLCHAIN) $(GO) build -o /dev/null .
	cd cmd/rdp-launcher-dialog && GOTOOLCHAIN=$(GUI_GOTOOLCHAIN) $(GO) vet ./...
	cd cmd/rdp-launcher-dialog && GOTOOLCHAIN=$(GUI_GOTOOLCHAIN) $(GO) test -race ./...

# Cross-compilation checks the Windows test binary; it does not execute it.
check-agent:
	cargo clippy --manifest-path windows-agent/Cargo.toml --target $(WINDOWS_TARGET) -- -D warnings
	cargo test --manifest-path windows-agent/Cargo.toml --target $(WINDOWS_TARGET) --no-run
