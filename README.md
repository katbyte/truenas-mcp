# truenas-mcp - a TrueNAS MCP server, CLI and Go SDK

An [MCP](https://modelcontextprotocol.io) server, CLI and Go SDK for looking after a [TrueNAS](https://www.truenas.com) from Claude Code, Claude Desktop, or any other MCP client.

**This is being built, and the MCP server is not here yet.** What works today is the part everything else stands on: the connection to a TrueNAS, and a stand-in NAS to test against.

## Why this exists

I needed a Go SDK for TrueNAS, for [truenas-metrics](https://github.com/katbyte/truenas-metrics), and an MCP server is the best way I know to test an SDK. An exporter reads a handful of things; a server with a tool for everything uses the whole API, and every tool is tried against a real TrueNAS. So the SDK lives here, where it is tested hardest, and the exporter imports it.

## What is here

| | |
|---|---|
| `sdk/truenasrpc` | the connection: logs in with an API key or a password, shares one connection between any number of calls, starts and follows long jobs, hears what the NAS says when something changes, reconnects when the NAS restarts, and never writes the key down |
| `sdk/truenastest` | a stand-in TrueNAS for tests, which behaves the way the TrueNAS source says a real one does |
| `truenas-mcp info` | checks an address, a certificate and a key against a real NAS and prints the version it runs |

## What is coming

- a Go client with a method for every TrueNAS API call, generated from the NAS's own description of its API (`api-defs/`)
- the MCP tools, built on that client

## Requirements

TrueNAS 25.04 or later. It is talked to over its websocket API; the older REST API is being removed from TrueNAS and is not used.

## Checking a connection

Make an API key in the TrueNAS interface (Credentials, then API Keys), and note the user it is made for.

```bash
export TRUENAS_SERVER=nas.example.net
export TRUENAS_USERNAME=mcp
export TRUENAS_TOKEN=...   # the API key
truenas-mcp info
```

A TrueNAS comes with a certificate it signed itself and made out to `localhost`, which nothing trusts unasked and which can never match the name the NAS is reached by. Save that certificate to a file (in the TrueNAS interface: Credentials, then Certificates, then download) and hand it over with `--certificate-file` (`TRUENAS_CERTIFICATE_FILE`). The NAS is then known by that certificate's key, whatever name and dates are on it: renewing the certificate with the same key changes nothing, and a new key is refused until the file is replaced.

A NAS that has been given a proper certificate for its own name needs nothing, or `--ca-file` (`TRUENAS_CA_FILE`) for an authority of your own. `--skip-certificate-check` (`TRUENAS_SKIP_CERTIFICATE_CHECK=true`) switches the check off, which means nothing proves it is your NAS that answers.

TrueNAS revokes an API key that is ever sent without TLS, so an `http://` address is refused before anything is sent.

Run on the NAS itself, the address can be the NAS's own socket, `--server unix://` (or `unix:///run/middleware/middlewared.sock` in full). No key is needed: the NAS can see which user is asking, and takes root or any user it has an account for.

## How the key is used

**TrueNAS 26 and later: the key is never sent.** The program and the NAS each prove to the other that they hold the key, so nothing that listens, and nothing that answers in the NAS's place, learns it. The login is also tied to the certificate the NAS showed, so it cannot be passed along by something sitting in the middle. This is used whenever the NAS offers it, and it needs the user name.

**Before TrueNAS 26: the key is sent, over TLS.** That is the only login those versions have. Because of it, the key is not sent to a NAS whose certificate was not checked: with `--skip-certificate-check` an older NAS is refused, with a message saying why, unless `--send-key-unchecked` says to send it anyway. Without that rule, something in the middle of a newer NAS's connection could claim to be an older one and be handed the key.

**A NAS behind a reverse proxy** shows the proxy's certificate, not its own, so a login tied to the certificate is refused even with the right key. `--no-channel-binding` leaves the login untied; the key is still never sent.

**The key can come from a file** with `--token-file` (`TRUENAS_TOKEN_FILE`): the key on a line of its own, or a key file as TrueNAS hands one out. A key file that holds only the parts worked out of a key, and not the key, works with TrueNAS 26 and later.

| flag | environment | what it is |
|---|---|---|
| `--server` | `TRUENAS_SERVER` | the NAS's address: a host, a host and port, an `https://` address, or `unix://` on the NAS itself |
| `--username` | `TRUENAS_USERNAME` | the user the API key was made for. Without it the key is logged in the older way, which TrueNAS 27 removes |
| `--token` | `TRUENAS_TOKEN` | the API key |
| `--certificate-file` | `TRUENAS_CERTIFICATE_FILE` | the NAS's own certificate as a PEM file, accepted whatever name is on it |
| `--ca-file` | `TRUENAS_CA_FILE` | a PEM file of the certificate authority that signed the NAS's certificate, for one made out to the NAS's name |
| `--skip-certificate-check` | `TRUENAS_SKIP_CERTIFICATE_CHECK` | take whatever certificate the NAS shows |
| `--token-file` | `TRUENAS_TOKEN_FILE` | a file holding the API key, or a TrueNAS key file, in place of `--token` |
| `--send-key-unchecked` | `TRUENAS_SEND_KEY_UNCHECKED` | with `--skip-certificate-check`, send the key to a TrueNAS before 26 all the same |
| `--no-channel-binding` | `TRUENAS_NO_CHANNEL_BINDING` | do not tie the login to the NAS's certificate: for a TrueNAS 26 or later behind a reverse proxy |
| | `TRUENAS_LOG` | how much is logged: `INFO`, `DEBUG`, or `TRACE` for every message with its secrets hidden |

The same settings can be kept in a `.truenas-mcp` file in the working directory or in `$HOME`, as `SERVER=...` lines.

## Using the connection from Go

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

A person can log in too, with `truenasrpc.Password("admin", password, truenasrpc.WithOTP(ask))`, where `ask` is called for a one-time password when the account wants one. `truenasrpc.APIKeyFile` reads the key from a file.

### Long jobs

A scrub or a replication runs for hours. `Call` waits for one to finish; `Start` returns as soon as the NAS has taken it on, and the job can be followed, waited for, stopped, or found again later by its number.

```go
job, err := nas.Start(ctx, "pool.scrub.scrub", []any{"tank"})
if err != nil {
	return err
}

job.OnUpdate(func(s truenasrpc.JobStatus) { log.Printf("%s: %.0f%% %s", s.State, s.Percent, s.Description) })

err = job.Wait(ctx, nil)       // an *Error if it failed, ErrJobAborted if it was stopped, ErrJobLost if the NAS restarted
same, err := nas.Job(ctx, job.ID()) // from another program, or after this one restarts
```

A job that was started takes none of the ten places a NAS gives a connection for calls, however long it runs. Waiting goes on through a connection that drops.

### Hearing what changes

```go
sub, err := nas.Subscribe(ctx, "alert.list", func(e truenasrpc.Event) {
	// e.Kind is truenasrpc.Added, Changed or Removed; e.Fields is how it now stands
})
defer sub.Close()
```

When the NAS restarts, the connection is made again and the NAS asked again without anyone noticing, and the function is handed one `truenasrpc.Resumed` event: whatever was said in between was not heard, so a program keeping a copy asks for the whole of it again.

### Files

Eight methods hand a file back (a saved configuration, a debug bundle, a dataset's keys) and eight take one. Neither goes over the connection calls are made on, so each has a function of its own.

```go
var backup bytes.Buffer
_, err := nas.Download(ctx, "config.save", []any{map[string]any{"secretseed": true}}, &backup)

job, err := nas.Upload(ctx, "config.upload", nil, file) // then job.Wait
```

The key is not sent with either: the NAS is asked, over the connection, for leave that is good once and for a few minutes. Nothing of a file's contents is written to a trace. A download that the NAS could not finish is an error, not a short file.

### On the NAS itself

```go
nas, err := truenasrpc.New("unix://", truenasrpc.LocalUser())
```

### Values TrueNAS writes its own way

`truenasrpc.Time`, `Date`, `TimeOfDay`, `Set[T]` and `IPInterface` read and write the five kinds of value TrueNAS wraps in an object (`{"$date": ...}` and so on), as fields of whatever struct an answer is read into.

What the NAS does that its API description does not say is written down in [api-defs/README.md](api-defs/README.md).

### Beside TrueNAS's own Go client

TrueNAS publishes a Go client of its own, [truenas/api-client-go](https://github.com/truenas/api-client-go). This connection does everything that one does but one thing, and is tested against it: TrueNAS's client logs into the stand-in NAS here every way this one does, runs jobs on it and hears its events.

| | TrueNAS's client | this one |
|---|---|---|
| API key login that never sends the key (TrueNAS 26) | yes | yes |
| key files, password and one-time password logins | yes | yes |
| start a job, follow its progress, wait for it | yes | yes, and stop it or find it again by number |
| hear what changes | yes | yes |
| the NAS's own socket | yes | yes |
| fetch a file the NAS makes, or hand it one | no | yes |
| connect from a privileged port | yes | no: only one NAS of a pair talking to the other needs it |
| reconnect after the NAS restarts | no | yes, calls, jobs and subscriptions alike |
| hold back calls the NAS would refuse for being too many at once | no | yes |
| trust a NAS's own certificate, or an authority of your own | no: check it against the system's, or not at all | yes |
| pick the login by what the NAS offers | no: the older login has to be asked for | yes, and never sends a key to a NAS it could not check |
| keep secrets out of logs | n/a: it logs no messages | yes, held to every secret the TrueNAS source marks |

## Development

```bash
make tools      # build the pinned dev tools into .tools/bin
make test       # the unit tests, with the race detector
make check-all  # build + test + every linter + depscheck
make testacc    # the live tests, against a throwaway TrueNAS in a virtual machine
```

### Live tests

`make testacc` installs a real TrueNAS in a virtual machine and hands it to the tests in `acceptance/` as its installer left it: an administrator and nothing else. The first test then sets it up through the SDK, a step at a time, and reads the NAS back after each step: a name, a mirrored pool whose making is followed as a job, two datasets, a user who may only read and is refused a write, and a key for each user. Setting a NAS up from nothing is the hardest thing the SDK is asked to do, so it is a test and not a script. The tests after it use what it made, and the machine is removed at the end.

Nothing of yours is ever a target: the tests and the setup both refuse a NAS that has a pool and is not named `throwaway-nas`.

It needs QEMU and an x86-64 Linux machine with `/dev/kvm`, which a GitHub runner is, so the `acceptance` workflow is where it runs. TrueNAS does not run on ARM, so on a Mac it would have to be emulated, which is refused unless `TRUENAS_TEST_ALLOW_EMULATION=1`.

`eval "$(make testenv-up)"` starts the same NAS, set up by the same steps (`acceptance/setup`), and leaves it up with what is needed to log into it in the environment; `make test-live` runs the tests against it and `make testenv-down` removes it. What the NAS holds is written down in `acceptance/layout`.

What a real NAS does differently from its source and its description is what this is there to find, and each thing found is written into [api-defs/README.md](api-defs/README.md) with the test that now holds it.
