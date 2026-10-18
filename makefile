# recipes use bash for pipefail support (ubuntu's default sh is dash)
SHELL := /bin/bash

# the short commit, as a release stamps it; how far past the tag and whether the tree was dirty is in GIT_VERSION
GIT_COMMIT=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
# falls back to dev when there is no tag yet (the || must wrap git, not sed, or it never fires)
GIT_VERSION=$(shell (git describe --tags --dirty 2>/dev/null || echo dev) | sed 's/-\([0-9]*\)-g/+\1@g/')
TEST_TIMEOUT?=15m

# dev tool binaries are built into .tools/bin (gitignored) from the versions pinned in
# .tools/go.mod - the single source of truth for make and CI; dependabot keeps them updated.
# overridable because binaries cannot execute from a noexec mount (e.g. an smb checkout):
# make TOOLS_BIN=~/.cache/truenas-mcp/bin lint
TOOLS_BIN?=.tools/bin
ACTIONLINT=$(TOOLS_BIN)/actionlint
GOFUMPT=$(TOOLS_BIN)/gofumpt
GOLANGCI_LINT=$(TOOLS_BIN)/golangci-lint

# non-Go tools also live in .tools/bin at pinned versions, but the pins are here (dependabot
# cannot bump them): shellcheck, typos and zizmor are static binaries downloaded from their github
# releases, yamllint is pip installed into a repo-local venv and markdownlint-cli2 is npm installed
# into a repo-local prefix. all rebuild when this makefile changes.
MARKDOWNLINT_CLI2_VERSION=0.23.2
SHELLCHECK_VERSION=v0.11.0
TYPOS_VERSION=v1.51.1
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
	@mkdir -p $(TOOLS_BIN)
	@cd .tools && go build -o $(abspath $@) $$(go list tool | grep "/$*$$")

# explicit rules take precedence over the pattern rule above for the non-Go tools.
# `golangci-lint custom` always writes to .tools/bin (destination in .custom-gcl.yml), so move
# the result when TOOLS_BIN points elsewhere (e.g. a local dir because the checkout is noexec)
$(GOLANGCI_LINT_MODULES): .tools/.custom-gcl.yml $(GOLANGCI_LINT)
	@echo "==> building golangci-lint with plugins (versions pinned in .tools/.custom-gcl.yml)..."
	@cd .tools && mkdir -p bin && $(abspath $(GOLANGCI_LINT)) custom
	@if [ "$(abspath $(TOOLS_BIN))" != "$(abspath .tools/bin)" ]; then mv .tools/bin/golangci-with-modules $@; fi

$(SHELLCHECK): makefile
	@echo "==> downloading shellcheck $(SHELLCHECK_VERSION)..."
	@mkdir -p $(TOOLS_BIN)
	@os=$$(uname | tr 'A-Z' 'a-z'); arch=$$(uname -m); [ "$$arch" = "arm64" ] && arch=aarch64; \
		curl -sSfL "https://github.com/koalaman/shellcheck/releases/download/$(SHELLCHECK_VERSION)/shellcheck-$(SHELLCHECK_VERSION).$$os.$$arch.tar.xz" \
		| tar -xJ -O shellcheck-$(SHELLCHECK_VERSION)/shellcheck > $@ && chmod +x $@

$(YAMLLINT): makefile
	@command -v python3 >/dev/null || (echo "python3 is required to install yamllint (macOS: xcode CLT; Debian/Ubuntu: apt install python3-venv)" && exit 1)
	@echo "==> installing yamllint $(YAMLLINT_VERSION) into $(TOOLS_BIN)/../venv..."
	@mkdir -p $(TOOLS_BIN)
	@python3 -m venv $(TOOLS_BIN)/../venv && $(TOOLS_BIN)/../venv/bin/pip install -q yamllint==$(YAMLLINT_VERSION) && ln -sf ../venv/bin/yamllint $@

$(ZIZMOR): makefile
	@echo "==> downloading zizmor $(ZIZMOR_VERSION)..."
	@mkdir -p $(TOOLS_BIN)
	@case "$$(uname)" in Darwin) target=apple-darwin;; *) target=unknown-linux-gnu;; esac; \
		arch=$$(uname -m); [ "$$arch" = "arm64" ] && arch=aarch64; \
		curl -sSfL "https://github.com/zizmorcore/zizmor/releases/download/$(ZIZMOR_VERSION)/zizmor-$$arch-$$target.tar.gz" \
		| tar -xz -O zizmor > $@ && chmod +x $@

$(TYPOS): makefile
	@echo "==> downloading typos $(TYPOS_VERSION)..."
	@mkdir -p $(TOOLS_BIN)
	@case "$$(uname)" in Darwin) target=apple-darwin;; *) target=unknown-linux-musl;; esac; \
		arch=$$(uname -m); [ "$$arch" = "arm64" ] && arch=aarch64; \
		curl -sSfL "https://github.com/crate-ci/typos/releases/download/$(TYPOS_VERSION)/typos-$(TYPOS_VERSION)-$$arch-$$target.tar.gz" \
		| tar -xz -O ./typos > $@ && chmod +x $@

$(MARKDOWNLINT): makefile
	@command -v npm >/dev/null || (echo "npm is required to install markdownlint-cli2 (macOS: brew install node; Debian/Ubuntu: apt install npm)" && exit 1)
	@echo "==> installing markdownlint-cli2 $(MARKDOWNLINT_CLI2_VERSION) into $(TOOLS_BIN)/../npm..."
	@mkdir -p $(TOOLS_BIN)
	@npm install --silent --no-audit --no-fund --prefix $(TOOLS_BIN)/../npm markdownlint-cli2@$(MARKDOWNLINT_CLI2_VERSION) && ln -sf ../npm/node_modules/.bin/markdownlint-cli2 $@

default: fmt build

all: fmt build

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make \033[36m<target>\033[0m\n"} /^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-24s\033[0m%s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Build
build: ## Compile truenas-mcp with version info from git
	@echo "==> building..."
	go build -ldflags "-X github.com/katbyte/go-kt/version.GitCommit=${GIT_COMMIT} -X github.com/katbyte/go-kt/version.Version=${GIT_VERSION}"

install: ## Install truenas-mcp into GOPATH/bin with version info from git
	@echo "==> installing..."
	go install -ldflags "-X github.com/katbyte/go-kt/version.GitCommit=${GIT_COMMIT} -X github.com/katbyte/go-kt/version.Version=${GIT_VERSION}" .

docker: ## Build the truenas-mcp container image with version info from git
	@echo "==> building docker image..."
	docker build --build-arg VERSION=${GIT_VERSION} --build-arg COMMIT=${GIT_COMMIT} -t truenas-mcp .

tools: $(ACTIONLINT) $(GOFUMPT) $(GOLANGCI_LINT) $(GOLANGCI_LINT_MODULES) $(MARKDOWNLINT) $(SHELLCHECK) $(TYPOS) $(YAMLLINT) $(ZIZMOR) ## Install all pinned dev tools into .tools/bin

##@ API description
generate: ## Import api-defs/truenas-methods-<release>.json into api-defs/truenas-<release>/, then generate sdk/truenas from it
	@echo "==> importing TrueNAS's description of its API into api-defs/truenas-<release>/..."
	go run ./sdk/pandorest import
	@echo "==> generating sdk/truenas from api-defs/truenas-<release>/..."
	go run ./sdk/pandorest generate

# gencheck regenerates and asks git whether anything moved, so it needs a tree with nothing uncommitted: the build workflow runs it after each
# push, and check-all, which runs before a commit, does not
gencheck: generate ## Check that api-defs/ and sdk/truenas are what the description makes (regenerate, then diff)
	@test -z "$$(git status --porcelain -- api-defs sdk/truenas)" || \
		(git status --short -- api-defs sdk/truenas; echo; \
		echo "api-defs/ or sdk/truenas is stale. Run 'make generate' and commit."; exit 1)

apicheck: ## Check that the definitions match the description and every method has a call in sdk/truenas
	@echo "==> Checking that sdk/truenas has every method of the description..."
	@go run ./sdk/pandorest check -quiet

secrets: ## Write api-defs/truenas-secrets-<release>.json from a checkout of the TrueNAS source: make secrets SOURCE=../middleware RELEASE=25.10.7
	@[ -n "$(SOURCE)" ] && [ -n "$(RELEASE)" ] || (echo 'name a checkout of truenas/middleware at the release, and the release: make secrets SOURCE=../middleware RELEASE=25.10.7'; exit 1)
	@go run ./sdk/pandorest secrets $(SOURCE) $(RELEASE)

##@ Formatting
fmt: $(GOFUMPT) $(GOLANGCI_LINT) ## Fix Go formatting (gofmt, gofumpt, goimports)
	@echo "==> Fixing source code with gofmt..."
	find . -name '*.go' | grep -v vendor | xargs gofmt -s -w
	@echo "==> Fixing source code with gofumpt..."
	find . -name '*.go' | grep -v vendor | xargs $(GOFUMPT) -w
	@echo "==> Fixing imports with golangci-lint (goimports)..."
	$(GOLANGCI_LINT) fmt -E goimports ./...

goimports: $(GOLANGCI_LINT) ## Fix imports with golangci-lint (goimports)
	@echo "==> Fixing imports with golangci-lint (goimports)..."
	$(GOLANGCI_LINT) fmt -E goimports ./...

##@ Linting & Dependencies
# markdown checked by markdownlint: every .md the repo owns
MARKDOWN_INPUTS='*.md' 'api-defs/**/*.md' 'docs/**/*.md' '.github/**/*.md'

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

zizmor: $(ZIZMOR) ## Audit GitHub workflows for security issues with zizmor
	@echo "==> Auditing workflows with zizmor..."
	@$(ZIZMOR) .

typos: $(TYPOS) ## Check spelling in code and docs with typos (config in .typos.toml)
	@echo "==> Checking spelling with typos..."
	@$(TYPOS) || \
		(echo; echo "Spelling errors found. Fix them with 'make typos-fix', or add false positives (names, API words) to .typos.toml."; exit 1)

typos-fix: $(TYPOS) ## Fix spelling errors found by typos
	@$(TYPOS) --write-changes

markdownlint: $(MARKDOWNLINT) ## Check the repo's markdown with markdownlint (config in .markdownlint.yml)
	@echo "==> Checking markdown with markdownlint..."
	@$(MARKDOWNLINT) $(MARKDOWN_INPUTS)

depscheck: ## Check that go.mod/go.sum and vendor/ are in sync
	@echo "==> Checking source code with go mod tidy..."
	@go mod tidy
	@git diff --exit-code -- go.mod go.sum || \
		(echo; echo "Unexpected difference in go.mod/go.sum files. Run 'go mod tidy' command or revert any go.mod/go.sum changes and commit."; exit 1)
	@echo "==> Checking source code with go mod vendor..."
	@go mod vendor
	@git diff --compact-summary --exit-code -- vendor || \
		(echo; echo "Unexpected difference in vendor/ directory. Run 'go mod vendor' command or revert any go.mod/go.sum/vendor changes and commit."; exit 1)
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
# -race because every call shares one connection, and what they share went unseen without it
test: build ## Run the unit tests, with the race detector
	go test -race ./... -timeout ${TEST_TIMEOUT}

# the live suites (acceptance/) run against a real TrueNAS: a throwaway one in a virtual machine, which testacc installs, sets up, runs the suite
# against and removes. it needs qemu and an x86-64 Linux machine with /dev/kvm, which a GitHub runner is and a Mac is not, so it is not part of
# check-all: the acceptance workflow runs it. the SDK's suite and the tools' each get a NAS of their own: the SDK's first test sets its NAS up from
# nothing, and the tools' suite would otherwise have done that before it
LIVE_TIMEOUT?=70m
LIVE_SDK=$$(go list -tags acceptance ./acceptance/... | grep -v /acceptance/tools)
LIVE_TOOLS=./acceptance/tools/...

test-live: ## Run the SDK's live tests against a test NAS that is already up (see testenv-up)
	@[ -n "${TRUENAS_TEST_SERVER}" ] && [ -n "${TRUENAS_TEST_PASSWORD}" ] || \
		(echo 'TRUENAS_TEST_SERVER and the rest must be set: eval "$$(make testenv-up)", or use "make testacc"'; exit 1)
	go test -tags acceptance -count=1 -p 1 ${LIVE_SDK} -timeout ${LIVE_TIMEOUT} -v

test-live-tools: ## Run the tools' live tests against a test NAS that is already up (see testenv-up)
	@[ -n "${TRUENAS_TEST_SERVER}" ] && [ -n "${TRUENAS_TEST_PASSWORD}" ] || \
		(echo 'TRUENAS_TEST_SERVER and the rest must be set: eval "$$(make testenv-up)", or use "make testacc-tools"'; exit 1)
	go test -tags acceptance -count=1 ${LIVE_TOOLS} -timeout ${LIVE_TIMEOUT} -v

# start the machine, run the suite, remove the machine: what the machine said and showed is printed and kept when anything fails. the NAS is
# handed to the suite as its installer left it, since making everything on it is the first thing the suite tests. the suite is told where the
# machine's console is, since one test breaks a disk from it
testacc: ## Run the SDK's live tests against a throwaway TrueNAS in a virtual machine
	@echo "==> live tests..."
	@set -e; \
		env=".testenv.sh"; \
		trap 'st=$$?; [ $$st -eq 0 ] || .scripts/testenv.sh logs || true; .scripts/testenv.sh down; rm -f '"$$env"'; exit $$st' EXIT; \
		.scripts/testenv.sh boot > "$$env"; \
		. ./"$$env"; \
		TRUENAS_TEST_MONITOR="$$(.scripts/testenv.sh dir)/run/monitor.sock" go test -tags acceptance -count=1 -p 1 ${LIVE_SDK} -timeout ${LIVE_TIMEOUT} -v

testacc-tools: ## Run the tools' live tests against a throwaway TrueNAS of their own
	@echo "==> live tests of the tools..."
	@set -e; \
		env=".testenv.sh"; \
		trap 'st=$$?; [ $$st -eq 0 ] || .scripts/testenv.sh logs || true; .scripts/testenv.sh down; rm -f '"$$env"'; exit $$st' EXIT; \
		.scripts/testenv.sh boot > "$$env"; \
		. ./"$$env"; \
		go test -tags acceptance -count=1 ${LIVE_TOOLS} -timeout ${LIVE_TIMEOUT} -v

testenv-up: ## Start the throwaway TrueNAS and print the environment the live tests need: eval "$(make testenv-up)"
	@.scripts/testenv.sh up

testenv-down: ## Stop and remove the throwaway TrueNAS
	@.scripts/testenv.sh down

COVERDIR?=.coverage
COVERPKG=./sdk/...,./cli/...

cover: ## Run the tests with coverage and report the total
	@rm -rf $(COVERDIR)
	@mkdir -p $(COVERDIR)
	@go test -count=1 -coverpkg=$(COVERPKG) -coverprofile=$(COVERDIR)/coverage.out ./... >/dev/null
	@go tool cover -func=$(COVERDIR)/coverage.out | tail -1

cover-html: cover ## Run the tests with coverage and open the HTML report
	@go tool cover -html=$(COVERDIR)/coverage.out

check-all: build test lint actionlint yamllint shellcheck typos markdownlint depscheck apicheck ## Run build + tests + all linters + depscheck + apicheck

.PHONY: default all help generate gencheck apicheck secrets fmt goimports build docker lint lint-fix actionlint yamllint shellcheck zizmor typos typos-fix markdownlint depscheck check-all install tools test test-live test-live-tools testacc testacc-tools testenv-up testenv-down cover cover-html
