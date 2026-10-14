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
- something asked for by an id the NAS has nothing for is `IsNotFound`, as it is on a real NAS, which reports it as a refused argument
- `truenas-mcp info` checks an address, a certificate and a key, and prints the version the NAS runs
