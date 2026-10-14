# pandorest

[![GitHub release](https://img.shields.io/github/v/release/katbyte/pandorest?color=blueviolet)](https://github.com/katbyte/pandorest/releases/latest)
[![Go Version](https://img.shields.io/github/go-mod/go-version/katbyte/pandorest?color=00ADD8)](https://github.com/katbyte/pandorest/blob/main/go.mod)
[![License](https://img.shields.io/github/license/katbyte/pandorest?color=blue)](https://github.com/katbyte/pandorest/blob/main/LICENSE)
![build](https://github.com/katbyte/pandorest/actions/workflows/build.yaml/badge.svg)
![test](https://github.com/katbyte/pandorest/actions/workflows/pr-tests.yaml/badge.svg)
![lint](https://github.com/katbyte/pandorest/actions/workflows/pr-golangci-lint.yaml/badge.svg)
[![coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/katbyte/pandorest/badges/coverage.json)](https://github.com/katbyte/pandorest/actions/workflows/coverage.yaml)

pandorest turns a server's API document into a typed Go client, with a test for every operation. It reads an OpenAPI document, or a server's own list of its JSON-RPC methods, and writes a package a program can call without reading the document.

**The document is the source of truth, and what it gets wrong is fixed by a named workaround that fails the day the bug goes.** Nothing is guessed at: a document that does not say what an operation answers fails the import until a workaround says.

It follows [Pandora](https://github.com/hashicorp/pandora), the generator behind go-azure-sdk, scaled down to a few documents in one repository:

```text
api-defs/
  emby-openapi-4.10.0.40.json ──┐               ┌─ emby-4.10.0.40/*.json ──┐
  jellyfin-openapi-12.1.0.json ─┼─ import ──────┼─ jellyfin-12.1.0/*.json ─┼─ generate ─ sdk/emby, sdk/jf, sdk/tmdb
  tmdb-openapi-2026.09.22.json ─┘ (workarounds) └─ tmdb-2026.09.22/*.json ─┘
                                                        │
                                         diff ──────────┘  (what a refreshed document changes)
```

## Install

```bash
go get github.com/katbyte/pandorest
```

## Quick start

A repository keeps its documents under `api-defs/`, a description of each service, and the workarounds for its document's bugs, and hands them over from a short main:

```go
// sdk/pandorest/main.go
package main

import (
	"github.com/katbyte/pandorest"
	"github.com/katbyte/pandorest/config"

	"github.com/katbyte/sonarr-mcp/sdk/pandorest/workarounds"
)

func main() {
	pandorest.Main(pandorest.Config{
		Services: []config.Service{{
			Name:          "sonarr",
			Package:       "sonarr",
			Output:        "sdk/sonarr",
			Naming:        config.PathNaming,
			PathPrefix:    "/api/v3",
			Auth:          "APIKey",
			ClientService: "service", // sdk/sonarr/service.go, written by hand
			Paging:        &config.Paging{Start: "page", Limit: "pageSize", Items: "records", Total: "totalRecords", ByPage: true},
		}},
		Workarounds: workarounds.All,
	})
}
```

Then, from the repository root:

```bash
go run ./sdk/pandorest import      # the document into its definitions, with the workarounds applied
go run ./sdk/pandorest generate    # the package, from the definitions
go run ./sdk/pandorest diff        # what the document changes against the definitions, breaking changes marked
go run ./sdk/pandorest check       # the definitions match the document and every operation has a method
go run ./sdk/pandorest resolve     # which document and definitions each service is at
```

`-service a,b` limits a command to some services. `-root` runs it against another checkout. `-version` picks one vendored version. `diff -old <dir> -new <dir>` compares two definitions directories. `check -uncalled` lists the methods nothing in the repository calls, and `-callers tools/` counts callers in one directory.

## The clients it generates

Each lives in the repository that vendors its document, beside the MCP server built on it.

| Server | Package | Repository |
|---|---|---|
| Emby 4.10.0.40 | `github.com/katbyte/embyfin-mcp/sdk/emby` | [embyfin-mcp](https://github.com/katbyte/embyfin-mcp) |
| Jellyfin 12.1.0 | `github.com/katbyte/embyfin-mcp/sdk/jf` | [embyfin-mcp](https://github.com/katbyte/embyfin-mcp) |
| TMDB (document of 2026-09-22) | `github.com/katbyte/embyfin-mcp/sdk/tmdb` | [embyfin-mcp](https://github.com/katbyte/embyfin-mcp) |
| Sonarr 4.0.20.3014 | `github.com/katbyte/sonarr-mcp/sdk/sonarr` | [sonarr-mcp](https://github.com/katbyte/sonarr-mcp) |
| Radarr 6.4.4.10685 | `github.com/katbyte/radarr-mcp/sdk/radarr` | [radarr-mcp](https://github.com/katbyte/radarr-mcp) |
| Prowlarr 2.6.5.5623 | `github.com/katbyte/prowlarr-mcp/sdk/prowlarr` | [prowlarr-mcp](https://github.com/katbyte/prowlarr-mcp) |
| Dockhand 1.0.51 | `github.com/katbyte/dockhand-mcp/sdk/dockhand` | [dockhand-mcp](https://github.com/katbyte/dockhand-mcp) |

## How it works

**A service** is a `config.Service`: its name, package, how methods are named, how the server takes its credential, how its lists page, which dates and ids are typed. Only the first four fields are required. The rest, with examples, is in [docs/configuration.md](docs/configuration.md).

**Import** turns the document into definitions, plain JSON files that are checked in and reviewed. It is strict: anything the document leaves unsaid fails the import with every problem listed, rather than being guessed at. A bug in the document is fixed by a workaround, which must fail when the bug is gone, so a refreshed document that fixes it names the workaround to delete. The common bugs have ready-made workarounds. All of it is in [docs/importing.md](docs/importing.md), with the diff that shows what a refreshed document changes.

**Generate** writes the package from the definitions alone, in a flat layout or, like go-azure-sdk, as a package per tag. A method takes a context, the path parameters, the body and an options struct, and returns the decoded answer with the HTTP response. Every operation gets a test against a canned server. What the generated code looks like, and how to read an answer, a file or a list, is in [docs/generated-code.md](docs/generated-code.md).

**The read sweep** is the test against the real server, for a repository's live suite: every read called on the generated client, each answer held to decode and to carry something, and every operation that cannot answer excused by name. It is in [docs/sweep.md](docs/sweep.md).

**The base client** is what every generated package sends through, with retries, a size limit and a trace that hides credentials. What is one server's own about being talked to goes in a short file the repository writes by hand. It is in [docs/base-client.md](docs/base-client.md).

**A JSON-RPC server** with no OpenAPI document, such as TrueNAS, gives its own list of methods instead. Each becomes a call with its arguments in order, and the generated `New` takes a connection the repository writes. The package never sees a socket.

## Pieces

| Package | Does |
|---|---|
| `pandorest` | the commands: import, generate, diff, check, resolve |
| `config` | a service: its documents, paths, naming and options |
| `openapi` | reads the OpenAPI document |
| `jsonrpc` | reads a server's list of its JSON-RPC methods into the same form |
| `importer/workarounds` | what a workaround is, the helpers they share, and the test they must pass |
| `importer` | turns a patched document into definitions, strictly |
| `definitions` | the contract between import and generate: JSON per version per tag |
| `differ` | reports what changed between two sets of definitions |
| `generator` | writes the package from the definitions |
| `sweep` | calls every read of a generated client against a real server, for a live test |
| `client` | the base client every generated package sends through |

## How this is built

The code is written with Claude Code. I then review it and prune or rework it until it has the shape I want: I set the design and the scope, and decide what stays.

This is a library, so it has no server of its own to test against. Its tests are unit tests under the race detector, and generator tests that write a package from a small document, then build it, vet it and run its generated tests. The live tests against real servers are in the repositories that use it, one per server, where each runs its generated client against the server itself.

CI runs build, tests with coverage, golangci-lint, CodeQL, govulncheck, a check that go.mod is tidy, markdownlint, typos, shellcheck, actionlint and a workflow lint, and zizmor for workflow security.

## Development

```bash
make tools      # build the pinned dev tools into .tools/bin
make check-all  # build + test + lint + actionlint + yamllint + shellcheck + typos + depscheck
make cover      # tests with coverage
```

Nothing is vendored. The only dependency is go-kt. Tool versions are pinned in `.tools/go.mod`. Releases are described in [CONTRIBUTING.md](CONTRIBUTING.md).
