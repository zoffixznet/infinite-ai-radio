# Infinite AI Radio - endless locally-generated music

BINARY  := iar
GO      ?= go
PREFIX  ?= $(HOME)/.local
# Version resolution, in order: git (a checkout), the .version file (a
# source tarball - git archive expands it via export-subst), then dev.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null)
ifeq ($(VERSION),)
VERSION := $(shell grep -v '^\$$Format' .version 2>/dev/null)
endif
ifeq ($(VERSION),)
VERSION := dev
endif

# -trimpath keeps the building machine's paths out of the binary; the
# version is stamped into the variable cmd/iar/main.go declares for it.
BUILDFLAGS  := -trimpath -ldflags "-X main.version=$(VERSION)"
RELFLAGS    := -trimpath -ldflags "-s -w -X main.version=$(VERSION)"
# Platforms release tarballs are built for. The binary is pure Go and
# every asset is embedded, so these cross-compile without a toolchain.
PLATFORMS   := linux/amd64 linux/arm64

# System packages needed at runtime/build time (Debian/Ubuntu names)
APT_PKGS := ffmpeg git build-essential

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@echo "Infinite AI Radio - endless locally-generated music"
	@echo
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  %-12s %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the iar binary into the repo root
	$(GO) build $(BUILDFLAGS) -o $(BINARY) ./cmd/iar

.PHONY: run
run: build ## Build and run Infinite AI Radio (starts playback)
	./$(BINARY)

.PHONY: setup
setup: build ## First-run setup: install the music engine and download models (resumable, no sudo)
	./$(BINARY) setup

.PHONY: deps
deps: ## Check required system tools; if any are missing, print and run the apt-get install line
	@missing=""; \
	command -v ffmpeg  >/dev/null 2>&1 || missing="$$missing ffmpeg"; \
	command -v ffprobe >/dev/null 2>&1 || missing="$$missing ffmpeg"; \
	command -v git     >/dev/null 2>&1 || missing="$$missing git"; \
	command -v gcc     >/dev/null 2>&1 || missing="$$missing build-essential"; \
	if [ -n "$$missing" ]; then \
		pkgs=$$(echo $$missing | tr ' ' '\n' | sort -u | tr '\n' ' ' | sed 's/ $$//'); \
		echo "Missing system packages: $$pkgs"; \
		echo "Running: sudo apt-get install -y $$pkgs"; \
		sudo apt-get install -y $$pkgs; \
	else \
		echo "All required system tools are present (ffmpeg, git, gcc)."; \
	fi
	@if command -v tailscale >/dev/null 2>&1; then \
		echo "Optional: tailscale is installed (phone remote can bind your tailnet address)."; \
	else \
		echo "Optional: tailscale not found. The phone remote works on localhost without it;"; \
		echo "to reach it from your phone, install Tailscale (needs sudo) per the official"; \
		echo "guide at https://tailscale.com/download/linux and run: sudo tailscale up"; \
	fi

.PHONY: test
test: ## Run the full test suite
	$(GO) test ./...

.PHONY: smoke
smoke: build ## End-to-end smoke test of the built binary (silent, sandboxed)
	./scripts/smoke_test.sh

.PHONY: browser-test
browser-test: ## Drive the phone remote in headless Firefox (needs geckodriver, firefox, pactl; silent)
	$(GO) test -tags browser -count=1 -v -run TestRealBrowser -timeout 12m ./internal/remote/

.PHONY: screenshots
screenshots: ## Re-shoot the README's phone remote screenshots into assets/ (needs geckodriver, firefox, pactl, Pillow)
	./scripts/screenshots.sh assets

.PHONY: lint
lint: ## Run go vet and check gofmt
	$(GO) vet ./...
	@fmt_out=$$(gofmt -l .); \
	if [ -n "$$fmt_out" ]; then \
		echo "gofmt needed on:"; echo "$$fmt_out"; exit 1; \
	fi

.PHONY: install
install: build ## Install the binary into ~/.local/bin
	install -d $(PREFIX)/bin
	install -m 0755 $(BINARY) $(PREFIX)/bin/$(BINARY)

.PHONY: release
release: ## Build release tarballs and checksums for every platform into dist/
	@rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		name=$(BINARY)_$(VERSION)_$${os}_$${arch}; \
		echo "building $$name"; \
		mkdir -p dist/$$name; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build $(RELFLAGS) -o dist/$$name/$(BINARY) ./cmd/iar || exit 1; \
		cp README.md LICENSE dist/$$name/; \
		cp internal/prosody/data/LICENSE dist/$$name/LICENSE.third-party; \
		cp -r assets dist/$$name/assets; \
		tar -C dist -czf dist/$$name.tar.gz $$name; \
		rm -rf dist/$$name; \
	done
	@cd dist && sha256sum *.tar.gz > SHA256SUMS
	@echo; ls -l dist

.PHONY: clean
clean: ## Remove build outputs
	rm -f $(BINARY) coverage.out
	rm -rf dist .shots
