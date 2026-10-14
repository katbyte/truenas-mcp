# truenas-mcp - a TrueNAS MCP server, CLI and Go SDK

An [MCP](https://modelcontextprotocol.io) server, CLI and Go SDK for looking after a [TrueNAS](https://www.truenas.com) from Claude Code, Claude Desktop, or any other MCP client.

**It was made mainly to get a Go SDK for [truenas-metrics](https://github.com/katbyte/truenas-metrics).** A server with a tool for much of the NAS tests an SDK far harder than an exporter that reads a handful of things. So the SDK lives here, and the exporter imports it.

**This is being built.** What works today is the Go connection to a TrueNAS, a typed client with a method for every API call, a stand-in NAS to test against, and an MCP server with its first tools: which NAS it is, its pools down to each disk, its datasets, its disks and its alerts. The rest of the tools are being built on the typed client.

**Detection is code, correction is judgment.** The checks that are coming only report what is wrong. Nothing on a NAS is changed unless a separate tool is asked to change it.

## Install

```bash
git clone https://github.com/katbyte/truenas-mcp && cd truenas-mcp
make install
```

It needs Go, and a TrueNAS 25.04 or later. There is no release yet.

## Quick start

Make an API key in the TrueNAS interface (Credentials, then API Keys), and note the user it is made for.

```bash
export TRUENAS_SERVER=nas.example.net
export TRUENAS_USERNAME=mcp
export TRUENAS_TOKEN=...   # the API key
truenas-mcp info
```

A TrueNAS comes with a certificate nothing trusts. Download it (Credentials, then Certificates) and pass it with `--certificate-file`. [Connecting](docs/connecting.md) has the other ways, and how the key is kept safe.

Register the server with Claude Code. It only reads unless `--enable-write` is given.

```bash
claude mcp add truenas -e TRUENAS_SERVER=nas.example.net -e TRUENAS_USERNAME=mcp -e TRUENAS_TOKEN=... -- truenas-mcp serve
```

## What is here

| | |
|---|---|
| `sdk/truenasrpc` | the connection to a TrueNAS |
| `sdk/truenas` | a typed client with a method for every API call, generated from TrueNAS's own description |
| `sdk/truenastest` | a stand-in TrueNAS for tests |
| `tools` | the MCP tools, and which of them a session gets |
| `truenas-mcp serve` | the MCP server, over stdio or HTTP |
| `truenas-mcp tools` | lists the tools the flags would load, with no NAS |
| `truenas-mcp info` | checks an address, a certificate and a key |

What is coming: the rest of the tools, built on the typed client.

## Audits

Each audit only reports, and changes nothing. [Audits](docs/audits.md) has every rule.

| audit | what it catches |
|---|---|
| `audit_pools` | an unhealthy pool, a disk gone or counting errors, a scrub that found errors, a pool not scrubbed lately or with no scrub task |
| `audit_space` | a pool past 80% full, a dataset near or at its quota or refquota |
| `audit_tasks` | a snapshot task, replication, copy task, cloud sync or cloud backup that failed, is held, copies a locked dataset or is switched off |

## Settings

| flag | environment | what it is |
|---|---|---|
| `--server` | `TRUENAS_SERVER` | the NAS's address, or `unix://` on the NAS itself |
| `--username` | `TRUENAS_USERNAME` | the user the API key was made for |
| `--token` | `TRUENAS_TOKEN` | the API key |
| `--token-file` | `TRUENAS_TOKEN_FILE` | a file holding the key, in place of `--token` |
| `--certificate-file` | `TRUENAS_CERTIFICATE_FILE` | the NAS's own certificate, as a PEM file |
| `--ca-file` | `TRUENAS_CA_FILE` | the authority that signed the NAS's certificate |
| `--skip-certificate-check` | `TRUENAS_SKIP_CERTIFICATE_CHECK` | take whatever certificate the NAS shows |
| `--send-key-unchecked` | `TRUENAS_SEND_KEY_UNCHECKED` | send the key to an unchecked NAS older than 26 |
| `--no-channel-binding` | `TRUENAS_NO_CHANNEL_BINDING` | for a NAS behind a reverse proxy |
| `--enable-write` | `TRUENAS_ENABLE_WRITE` | load the tools that change something |
| `--enable-delete` | `TRUENAS_ENABLE_DELETE` | load the tools that delete too; needs `--enable-write` |
| `--toolsets` | `TRUENAS_TOOLSETS` | which groups of tools to load; `core` and `audit` by default, `all` for every one |
| `--allow-tools`, `--deny-tools` | `TRUENAS_ALLOW_TOOLS`, `TRUENAS_DENY_TOOLS` | add or take away tools by name |
| `--listen` | `TRUENAS_LISTEN` | serve over HTTP on this address, in place of stdio |
| `--auth-token` | `TRUENAS_AUTH_TOKEN` | the token an HTTP client must send |
| | `TRUENAS_LOG` | `INFO`, `DEBUG` or `TRACE` |

The same settings can go in a `.truenas-mcp` file in `$HOME`, and in the working directory over it, as `SERVER=...` or `CERTIFICATE_FILE=...` lines.

## Use it from Go

```go
nas, err := truenasrpc.New("nas.example.net", truenasrpc.APIKey("metrics", key), truenasrpc.WithCertificateFile("nas.pem"))
if err != nil {
	return err
}
defer nas.Close()

var info struct {
	Version string `json:"version"`
}
err = nas.Call(ctx, "system.info", nil, &info)
```

One connection is shared by any number of calls. It reconnects by itself, and it never writes the key to a log. The typed client calls through it: `client.New(nas)`, then `typed.System.Info(ctx)`. [Using the connection from Go](docs/sdk.md) covers the typed client, long jobs, events, files, and how it compares with TrueNAS's own Go client.

## How this is built

- I set the design, the scope and the rules, and decide what stays.
- The SDK is tested against a real TrueNAS in a virtual machine, started clean for every run. The tools get a machine of their own, and every tool must answer in that run or it fails.
- Every read the API has that needs no argument is called and checked against the API's own description.
- The test NAS is given real problems to see how it reports them: a dead disk, a disk that gives back what was never written, a full dataset, copies whose other end goes away, a restart.
- Everything truenas-metrics asks a NAS is asked the same way, with a key that may only read.
- Each run counts how much of the API it called: about two thirds of its 769 methods so far. Not every method has a live test yet.
- CI runs on every push to main: build, unit tests with the race detector, the live tests, golangci-lint, CodeQL, govulncheck, a Trivy scan of the binary, zizmor, actionlint, yamllint, shellcheck, typos, markdownlint, and a check of the vendored dependencies.

## More

- [Audits](docs/audits.md): every rule each audit holds a NAS to
- [Connecting to a TrueNAS](docs/connecting.md): certificates, and how the key is used
- [Using the connection from Go](docs/sdk.md): jobs, events, files, and TrueNAS's own client compared
- [Development](docs/development.md): the checks, and the live tests against a real TrueNAS
- [What TrueNAS says about its API, and what it leaves out](api-defs/README.md): every surprise a real NAS gave, with the test that holds it
- [Contributing](CONTRIBUTING.md) and [the changelog](CHANGELOG.md)
