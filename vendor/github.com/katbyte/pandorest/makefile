# recipes use bash for pipefail support (ubuntu's default sh is dash)
SHELL := /bin/bash

TEST_TIMEOUT?=5m

# dev tool binaries are built into .tools/bin (gitignored) from the versions pinned in
# .tools/go.mod - the single source of truth for make and CI; dependabot keeps them updated
TOOLS_BIN=.tools/bin
ACTIONLINT=$(TOOLS_BIN)/actionlint
GOFUMPT=$(TOOLS_BIN)/gofumpt
GOLANGCI_LINT=$(TOOLS_BIN)/golangci-lint

# non-Go tools also live in .tools/bin at pinned versions, but the pins are here (dependabot
# cannot bump them): shellcheck (used by actionlint on run: blocks), typos and zizmor are static
# binaries downloaded from their github releases, yamllint is python installed into a repo-local
# venv, markdownlint-cli2 is npm installed into a repo-local prefix. all rebuild when this makefile
# changes.
MARKDOWNLINT_CLI2_VERSION=0.23.2
SHELLCHECK_VERSION=v0.11.0
TYPOS_VERSION=v1.50.1
YAMLLINT_VERSION=1.38.0
ZIZMOR_VERSION=v1.30.1
MARKDOWNLINT=$(TOOLS_BIN)/markdownlint-cli2
SHELLCHECK=$(TOOLS_BIN)/shellcheck
TYPOS=$(TOOLS_BIN)/typos
YAMLLINT=$(TOOLS_BIN)/yamllint
ZIZMOR=$(TOOLS_BIN)/zizmor

# golangci-lint with the azproviderlint module plugin compiled in (.tools/.custom-gcl.yml);
# lint runs use this binary, the plain go.mod one exists to bootstrap `golangci-lint custom`
GOLANGCI_LINT_MODULES=$(TOOLS_BIN)/golangci-with-modules

# one rule builds any Go tool: the import path comes from the tool directives in .tools/go.mod
# (via go list tool), so the makefile never repeats it - add a tool there and a variable above
$(TOOLS_BIN)/%: .tools/go.mod .tools/go.sum
	@echo "==> building $* (version pinned in .tools/go.mod)..."
	@cd .tools && go build -o bin/$* $$(go list tool | grep "/$*$$")

# explicit rules take precedence over the pattern rule above for the non-Go tools
$(GOLANGCI_LINT_MODULES): .tools/.custom-gcl.yml $(GOLANGCI_LINT)
	@echo "==> building golangci-lint with plugins (versions pinned in .tools/.custom-gcl.yml)..."
	@cd .tools && bin/golangci-lint custom

$(SHELLCHECK): makefile
	@echo "==> downloading shellcheck $(SHELLCHECK_VERSION)..."
	@mkdir -p $(TOOLS_BIN)
	@os=$$(uname | tr 'A-Z' 'a-z'); arch=$$(uname -m); [ "$$arch" = "arm64" ] && arch=aarch64; \
		curl -sSfL "https://github.com/koalaman/shellcheck/releases/download/$(SHELLCHECK_VERSION)/shellcheck-$(SHELLCHECK_VERSION).$$os.$$arch.tar.xz" \
		| tar -xJ -O shellcheck-$(SHELLCHECK_VERSION)/shellcheck > $@ && chmod +x $@

$(TYPOS): makefile
	@echo "==> downloading typos $(TYPOS_VERSION)..."
	@mkdir -p $(TOOLS_BIN)
	@case "$$(uname)" in Darwin) target=apple-darwin;; *) target=unknown-linux-musl;; esac; \
		arch=$$(uname -m); [ "$$arch" = "arm64" ] && arch=aarch64; \
		curl -sSfL "https://github.com/crate-ci/typos/releases/download/$(TYPOS_VERSION)/typos-$(TYPOS_VERSION)-$$arch-$$target.tar.gz" \
		| tar -xz -O ./typos > $@ && chmod +x $@

$(YAMLLINT): makefile
	@command -v python3 >/dev/null || (echo "python3 is required to install yamllint (macOS: xcode CLT; Debian/Ubuntu: apt install python3-venv)" && exit 1)
	@echo "==> installing yamllint $(YAMLLINT_VERSION) into .tools/venv..."
	@mkdir -p $(TOOLS_BIN)
	@python3 -m venv .tools/venv && .tools/venv/bin/pip install -q yamllint==$(YAMLLINT_VERSION) && ln -sf ../venv/bin/yamllint $@

$(ZIZMOR): makefile
	@echo "==> downloading zizmor $(ZIZMOR_VERSION)..."
	@mkdir -p $(TOOLS_BIN)
	@case "$$(uname)" in Darwin) target=apple-darwin;; *) target=unknown-linux-gnu;; esac; \
		arch=$$(uname -m); [ "$$arch" = "arm64" ] && arch=aarch64; \
		curl -sSfL "https://github.com/zizmorcore/zizmor/releases/download/$(ZIZMOR_VERSION)/zizmor-$$arch-$$target.tar.gz" \
		| tar -xz -O zizmor > $@ && chmod +x $@

$(MARKDOWNLINT): makefile
	@command -v npm >/dev/null || (echo "npm is required to install markdownlint-cli2 (macOS: brew install node; Debian/Ubuntu: apt install npm)" && exit 1)
	@echo "==> installing markdownlint-cli2 $(MARKDOWNLINT_CLI2_VERSION) into .tools/npm..."
	@mkdir -p $(TOOLS_BIN)
	@npm install --silent --no-audit --no-fund --prefix .tools/npm markdownlint-cli2@$(MARKDOWNLINT_CLI2_VERSION) && ln -sf ../npm/node_modules/.bin/markdownlint-cli2 $@

default: fmt build

all: fmt build

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make \033[36m<target>\033[0m\n"} /^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-24s\033[0m%s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Build
build: ## Compile every package (this is a library; there is no binary to install)
	@echo "==> building..."
	go build ./...

tools: $(ACTIONLINT) $(GOFUMPT) $(GOLANGCI_LINT) $(GOLANGCI_LINT_MODULES) $(MARKDOWNLINT) $(SHELLCHECK) $(TYPOS) $(YAMLLINT) $(ZIZMOR) ## Install all pinned dev tools into .tools/bin

##@ Formatting
fmt: $(GOFUMPT) $(GOLANGCI_LINT) ## Fix Go formatting (gofmt, gofumpt, goimports)
	@echo "==> Fixing source code with gofmt..."
	find . -name '*.go' | xargs gofmt -s -w
	@echo "==> Fixing source code with gofumpt..."
	find . -name '*.go' | xargs $(GOFUMPT) -w
	@echo "==> Fixing imports with golangci-lint (goimports)..."
	$(GOLANGCI_LINT) fmt -E goimports ./...

goimports: $(GOLANGCI_LINT) ## Fix imports with golangci-lint (goimports)
	@echo "==> Fixing imports with golangci-lint (goimports)..."
	$(GOLANGCI_LINT) fmt -E goimports ./...

##@ Linting & Dependencies
lint: $(GOLANGCI_LINT_MODULES) ## Check source code with the golangci linters (incl. azproviderlint)
	@echo "==> Checking source code against linters..."
	$(GOLANGCI_LINT_MODULES) run ./...

actionlint: $(ACTIONLINT) $(SHELLCHECK) ## Check GitHub workflows with actionlint (incl. shellcheck on run blocks)
	@echo "==> Checking workflows with actionlint..."
	@$(ACTIONLINT) -shellcheck=$(SHELLCHECK)

lint-fix: $(GOLANGCI_LINT_MODULES) ## Fix source code with all golangci linters
	@echo "==> Checking source code against linters (applying autofixes)..."
	$(GOLANGCI_LINT_MODULES) run --fix ./...

yamllint: $(YAMLLINT) ## Check YAML files with yamllint (config in .yamllint.yml)
	@echo "==> Checking YAML files with yamllint..."
	@$(YAMLLINT) -s .

shellcheck: $(SHELLCHECK) ## Check shell scripts with shellcheck
	@echo "==> Checking shell scripts with shellcheck..."
	@$(SHELLCHECK) .scripts/*.sh

typos: $(TYPOS) ## Check all files for spelling mistakes with typos (config in .typos.toml)
	@echo "==> Checking for typos..."
	@$(TYPOS)

typos-fix: $(TYPOS) ## Fix spelling mistakes found by typos
	@echo "==> Fixing typos..."
	@$(TYPOS) --write-changes

zizmor: $(ZIZMOR) ## Audit GitHub workflows for security issues with zizmor
	@echo "==> Auditing workflows with zizmor..."
	@$(ZIZMOR) .

markdownlint: $(MARKDOWNLINT) ## Check the repo's markdown with markdownlint (config in .markdownlint.yml)
	@echo "==> Checking markdown with markdownlint..."
	@$(MARKDOWNLINT) '**/*.md' '!.tools' '!generator/testdata'

depscheck: ## Check that go.mod/go.sum (and .tools/go.mod) are tidy; nothing is vendored in a library
	@echo "==> Checking source code with go mod tidy..."
	@go mod tidy
	@git diff --exit-code -- go.mod go.sum || \
		(echo; echo "Unexpected difference in go.mod/go.sum files. Run 'go mod tidy' command or revert any go.mod/go.sum changes and commit."; exit 1)
	@echo "==> Checking .tools/go.mod with go mod tidy..."
	@cd .tools && go mod tidy
	@git diff --exit-code -- .tools/go.mod .tools/go.sum || \
		(echo; echo "Unexpected difference in .tools/go.mod/go.sum. Run 'cd .tools && go mod tidy' and commit."; exit 1)
	@echo "==> Checking .tools/.custom-gcl.yml golangci-lint version matches .tools/go.mod..."
	@modv=$$(cd .tools && go list -m -f '{{.Version}}' github.com/golangci/golangci-lint/v2); \
		gclv=$$(grep '^version:' .tools/.custom-gcl.yml | awk '{print $$2}'); \
		[ "$$modv" = "$$gclv" ] || \
		(echo; echo "golangci-lint version mismatch: .tools/go.mod has $$modv but .tools/.custom-gcl.yml has $$gclv - update .custom-gcl.yml to match."; exit 1)

##@ Testing
test: build ## Run the unit tests under the race detector
	go test -race ./... -timeout ${TEST_TIMEOUT}

# coverage is written under .coverage (gitignored); the coverage workflow reads the total
# from coverage.out and publishes it as the README badge
COVERDIR?=.coverage

cover: build ## Run the tests with coverage and report the total
	@rm -rf $(COVERDIR) && mkdir -p $(COVERDIR)
	go test -race -count=1 -coverprofile=$(COVERDIR)/coverage.out ./... -timeout ${TEST_TIMEOUT}
	@go tool cover -func=$(COVERDIR)/coverage.out | tail -1

cover-html: cover ## Run the tests with coverage and open the HTML report
	@go tool cover -html=$(COVERDIR)/coverage.out

check-all: build test lint actionlint yamllint shellcheck typos zizmor markdownlint depscheck ## Run build + test + all linters + depscheck

.PHONY: default all help fmt goimports build lint lint-fix actionlint yamllint shellcheck typos typos-fix zizmor markdownlint depscheck check-all tools test cover cover-html
