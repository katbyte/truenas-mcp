# Changelog

## Unreleased

### Added

- a connection to a TrueNAS for Go programs, `sdk/truenasrpc`: it logs in with an API key, shares one connection between many calls, sends no more at once than the NAS will run, and reconnects by itself after the NAS restarts
- the API key is never written to a log or an error, and a trace hides passwords and keys that pass through
- an address without TLS is refused, because TrueNAS revokes a key sent over one
- a NAS still on the certificate it came with can be trusted by handing over that certificate (`--certificate-file`, `WithCertificateFile`). It is known by the certificate's key, so the name on it does not matter and a renewal with the same key changes nothing
- a TrueNAS 26 or later is logged into without the key being sent: each side proves it holds the key, and the login is tied to the certificate the NAS showed, so it cannot be passed along by something in the middle. `--no-channel-binding` (`WithoutChannelBinding`) leaves it untied, for a NAS behind a reverse proxy
- a key is not sent as it is to a NAS whose certificate was not checked, which is the only login a TrueNAS before 26 has: `--skip-certificate-check` on such a NAS is refused unless `--send-key-unchecked` (`EvenUnchecked`) is given
- the key can be read from a file, in any form TrueNAS hands one out in (`--token-file`, `APIKeyFile`), including a key file that holds only the parts worked out of a key
- a person can log in with a password, and a one-time password where the account asks for one (`Password`, `WithOTP`)
- long jobs can be started without waiting for them (`Start`), followed as they progress (`OnUpdate`), waited for (`Wait`), stopped (`Abort`) and found again by number from another program (`Conn.Job`). A started job takes none of the places calls are made in, and is followed through a connection that drops
- a program can hear what changes on the NAS as it happens (`Subscribe`). After the NAS restarts it is asked again by itself, and the listener is told there was a gap
- files the NAS makes can be fetched (`Download`: a saved configuration, a debug bundle, a dataset's keys) and files handed to it (`Upload`), and a job's log read (`Job.Logs`). The key is not sent with any of them, and nothing of a file reaches a trace
- a program running on the NAS itself can use the NAS's own socket and needs no key (`--server unix://`, `LocalUser`)
- `Ping`, `WithPrivateMethods`, TrueNAS's own error numbers with their names (`ErrnoName`), and types for the values TrueNAS writes its own way (`Time`, `Date`, `TimeOfDay`, `Set`, `IPInterface`)
- a job's arguments and answer are hidden in a trace the same way its own call's would be, wherever the NAS repeats them
- a stand-in TrueNAS for tests, `sdk/truenastest`, which can be a 25.10 or a 26, something answering in a NAS's place, or have something sitting in the middle of its connection. It runs jobs with progress, takes subscriptions and says what it is told to
- live tests against a real TrueNAS (`make testacc`, and the `acceptance` workflow): TrueNAS is installed in a virtual machine with nobody at the keyboard, and the first test sets it up from nothing through the SDK, reading it back after every step
- `Secret`, for a value the NAS keeps secret: a user who may only read is answered `********` in its place whatever the value is, a list or an object included, which would otherwise fail to read. It never prints its value
- every read that needs no argument is called on a real TrueNAS and its answer held to what the NAS's own description says it is, once by a user who may only read and once by the administrator; everything listed is fetched by its id; and the run ends by saying how many of the NAS's methods it called and which it did not
- the live tests give the NAS real problems and check how it reports each: a disk taken out, a disk that dies and is replaced by a spare, a full dataset, a nearly full pool, and a copy, a replication and a snapshot task that fail
- a list the NAS keeps as a set reads whether the NAS wrote it wrapped as one or as a plain list, which a real NAS does both of
- the live tests set up and take down storage and data protection (a second pool's whole life, scrubs, datasets, encryption, replication, snapshot tasks, boot environments), and ask everything truenas-metrics asks, its way, with a key that may only read
- the live tests make, change and remove one of each of twenty-one kinds of thing a NAS keeps, and change one setting on each of fifteen pages; a test NAS can be furnished with shares, a target, a machine and tasks on request (`TRUENAS_TEST_FURNISHED=1`), and has a disk that reports a temperature
- a trace hides two secrets TrueNAS itself does not treat as secret and hands to any key that may read: the private keys its SSH server proves itself with (`ssh.config`) and the key a replication encrypts with (`replication.query`). The list of secrets under `api-defs` keeps them apart, as `unmarked`
- the live tests start and stop a service every way there is, send a replication over SSH and a cloud sync over SFTP and take the other end away from each, write over a pool's disks from under it to see what a scrub finds and what it cannot repair, and bring the NAS certificates that have run out and are about to
- the connection meets what a client generated by pandorest v0.9.0 asks of one: `Upload` waits for the NAS and reads its answer as `Call` does, with `StartUpload` for the job; `Secret` is pandorest's `client.Secret`; and a refusal reads as `*client.CallError` with `errors.As`
- something asked for by an id the NAS has nothing for is `IsNotFound`, as it is on a real NAS, which reports it as a refused argument; so is a query told to answer its one row when no row matches, which a real NAS reports as `MatchNotFound()`
- `truenas-mcp info` checks an address, a certificate and a key, and prints the version the NAS runs
- a typed client for every method of the TrueNAS API, `sdk/truenas`, generated by pandorest v0.9.1 from the NAS's own description and calling through the connection; secrets a reader is sent as `********` and lists a NAS writes two ways read as they come; the build fails when the committed client is not what the description makes
- the MCP server, `truenas-mcp serve`, over stdio or over HTTP behind a token, with its first tool, `server_info`: which NAS this is, whether it is ready, and what the key's user may do
- tools for pools (`pool_list`, `pool_get`, down to each disk and its error counts), datasets (`dataset_list`, paged by the NAS itself, and `dataset_get`), disks (`disk_list`, with their temperatures) and alerts (`alert_list`, the gravest first); text the NAS wrote is cleaned before a client sees it
- audits, which only report: `audit_pools` (an unhealthy pool, a disk gone or counting errors, a scrub that found errors or is overdue) `audit_space` (a pool filling up, a dataset near its quota) `audit_tasks` (a snapshot, replication, copy or cloud task that failed, is held or copies a locked dataset) and `audit_disks` (a disk too hot, or failing its own SMART checks); they load by default beside core
- a session only reads unless `--enable-write` is given, and deletes nothing without `--enable-delete` beside it; `--toolsets`, `--allow-tools` and `--deny-tools` choose which tools load, and `truenas-mcp tools` lists them with no NAS
- a pool's scrub, expansion, auto-trim setting and vdevs with their disks read as types in the typed client (`truenas.PoolScan`, `truenas.PoolVdev`), where TrueNAS's description gives them no shape
- a task's state (`truenas.TaskState`) and its last run, as a job record, read as types in the typed client, where TrueNAS's description gives them no shape
- every question truenas-metrics asks, and every read that needs no argument, is also asked through the typed client in the live tests, with a key that may only read and as the administrator, and has to read into its types
- the tools have live tests of their own, on a NAS of their own, which fail when a tool never answers or says a key, and run the built binary as a client does

### Fixed

- a `.truenas-mcp` file now has its two-word settings read (`CERTIFICATE_FILE=...`), and one in the working directory changes only what it names of the one in `$HOME`; before, the first was ignored and the second replaced the whole file
