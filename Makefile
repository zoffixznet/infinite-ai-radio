# bgm - local AI background music player

BINARY  := bgm
GO      ?= go
PREFIX  ?= $(HOME)/.local

# System packages needed at runtime/build time (Debian/Ubuntu names)
APT_PKGS := ffmpeg git build-essential

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@echo "bgm - local AI background music player"
	@echo
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  %-12s %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the bgm binary into the repo root
	$(GO) build -o $(BINARY) ./cmd/bgm

.PHONY: run
run: build ## Build and run bgm (starts playback)
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

.PHONY: test
test: ## Run the full test suite
	$(GO) test ./...

.PHONY: smoke
smoke: build ## End-to-end smoke test of the built binary (silent, sandboxed)
	./scripts/smoke_test.sh

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

.PHONY: clean
clean: ## Remove build outputs
	rm -f $(BINARY) coverage.out
	rm -rf dist
