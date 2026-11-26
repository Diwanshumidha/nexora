# Shortcuts for common Nexora tasks. Builds, packaging and dev mode go
# through the Wails Taskfile (`wails3 task ...`), which stays the source of
# truth; the rest are plain go/pnpm commands. Run `make` for the list.
#
# Variables:
#   EXTRA_TAGS=gtk3            extra Go build tags (e.g. WebKitGTK 4.1 systems)
#   NEXORA_EXTENSIONS_REPO=…   nexora-extensions checkout (default ../nexora-extensions)
#   PREVIEW_PORT=18765         port for `make preview`

SHELL := /bin/bash
.DEFAULT_GOAL := help

TASK        := wails3 task
GO_PKGS     := . ./internal/...
TAGS        := $(if $(EXTRA_TAGS),-tags $(EXTRA_TAGS),)
TASK_VARS   := $(if $(EXTRA_TAGS),EXTRA_TAGS=$(EXTRA_TAGS),)

PREVIEW_DIR  ?= $(CURDIR)/.preview
PREVIEW_PORT ?= 18765
PREVIEW_MCP  ?= 127.0.0.1:17377

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "; printf "Usage: make <target>\n"} \
		/^##@/ {printf "\n%s\n", substr($$0, 5)} \
		/^[a-zA-Z0-9_-]+:.*## / {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

##@ Setup

.PHONY: deps
deps: ## Install frontend dependencies and download Go modules
	pnpm install
	go mod download

.PHONY: extensions
extensions: ## Build the SDK and extensions in the nexora-extensions checkout
	$(TASK) build:extensions

##@ Develop

.PHONY: dev
dev: ## Run the app with hot reload, loading the dev extensions
	$(TASK) dev $(TASK_VARS)

.PHONY: run
run: ## Build and run the desktop app
	$(TASK) run $(TASK_VARS)

.PHONY: preview
preview: ## Run in a browser (server mode) with a throwaway profile in .preview/
	$(TASK) build:server DEV=true $(TASK_VARS)
	@mkdir -p "$(PREVIEW_DIR)"
	@echo "Nexora preview: http://127.0.0.1:$(PREVIEW_PORT)  (profile: $(PREVIEW_DIR))"
	XDG_CONFIG_HOME="$(PREVIEW_DIR)" NEXORA_MCP_ADDR="$(PREVIEW_MCP)" \
		WAILS_SERVER_HOST=127.0.0.1 WAILS_SERVER_PORT=$(PREVIEW_PORT) ./bin/nexora-server

.PHONY: bindings
bindings: ## Regenerate the frontend's TypeScript bindings for Go services
	wails3 generate bindings -clean=true -ts -i

##@ Build

.PHONY: build
build: ## Production binary in bin/
	$(TASK) build $(TASK_VARS)

.PHONY: package
package: ## Platform package (AppImage/deb/rpm, .app, installer)
	$(TASK) package $(TASK_VARS)

.PHONY: build-server
build-server: ## Server-mode binary (no GUI) in bin/nexora-server
	$(TASK) build:server $(TASK_VARS)

.PHONY: frontend
frontend: ## Typecheck and build the frontend into frontend/dist
	pnpm --filter frontend build

##@ Test and check

.PHONY: test
test: ## All Go tests, including extension end-to-end tests
	$(TASK) test $(TASK_VARS)

.PHONY: test-quick
test-quick: ## Go tests without rebuilding the extensions
	go test $(TAGS) ./internal/...

.PHONY: test-race
test-race: ## Go tests with the race detector
	go test $(TAGS) -race ./internal/...

.PHONY: typecheck
typecheck: ## Typecheck the frontend
	cd frontend && npx tsc --noEmit -p .

.PHONY: vet
vet: ## go vet
	go vet $(TAGS) $(GO_PKGS)

.PHONY: fmt
fmt: ## Format Go code
	gofmt -w main.go webview_linux.go internal

.PHONY: fmt-check
fmt-check: ## Fail if Go code is not formatted
	@out=$$(gofmt -l main.go webview_linux.go internal); \
	if [ -n "$$out" ]; then echo "Not gofmt'd:"; echo "$$out"; exit 1; fi

.PHONY: check
check: fmt-check vet typecheck test-quick ## Everything CI should run: format, vet, typecheck, tests

##@ Clean

.PHONY: clean
clean: ## Remove build output (bin/, frontend/dist, .task)
	rm -rf bin frontend/dist .task

.PHONY: clean-preview
clean-preview: ## Delete the throwaway profile used by `make preview`
	rm -rf "$(PREVIEW_DIR)"
