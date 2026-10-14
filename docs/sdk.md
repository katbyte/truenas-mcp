# Using the connection from Go

`sdk/truenasrpc` is the connection to a TrueNAS. `sdk/truenas` is a typed client with a method for each of the API's 769 methods, generated from TrueNAS's own description of its API, which calls through the connection. `sdk/truenastest` is a stand-in NAS for tests, which behaves the way the TrueNAS source says a real one does.

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

One connection is shared by any number of calls. It reconnects by itself when the NAS restarts, sends no more calls at once than the NAS will run, and never writes the key to a log.

A person can log in too, with `truenasrpc.Password("admin", password, truenasrpc.WithOTP(ask))`. `ask` is called for a one-time password when the account wants one. `truenasrpc.APIKeyFile` reads the key from a file.

## The typed client

`sdk/truenas/client` groups the methods as the API does, one group per namespace, each named short: `pool.query` is `Pool.Query`, and `pool.dataset.query` is `PoolDataset.Query`. Arguments and answers are types in `sdk/truenas`, named after the whole method (`truenas.PoolQueryResponse`).

```go
typed, err := client.New(nas) // nas is the connection above
if err != nil {
	return err
}

info, err := typed.System.Info(ctx)
pools, err := typed.Pool.Query(ctx, nil, truenas.PoolQueryOptions{})
pool, err := typed.Pool.QueryOne(ctx, []any{[]any{"name", "=", "tank"}}, truenas.PoolQueryOptions{})
```

A query also has a form that answers its one row (`QueryOne`, `IsNotFound` when nothing matches) and one that answers how many (`QueryCount`). A value that is one of several records says which with `Kind`, and is read as one with `As<Member>`. A method that changes nothing says so in its comment, and one the NAS runs as a job says that. `truenas.Operations` lists every method with the same: whether it only reads, and whether it is a job. A method only reads when the NAS lets a user who may only read call it, less the five that such a user may call and that change something (`api_key.create`, `update` and `delete`, and `support.new_ticket` and `attach_ticket`), and with `directoryservices.status`, which changes nothing and whose description names no role.

Where the description leaves a shape out and a NAS always writes the same one, the client is given it: a pool's scrub, its auto-trim setting and its vdevs with their disks read as records of their own (`truenas.PoolScan`, `truenas.PoolVdev`), and so do a task's state (`truenas.TaskState`) and its last run, as a job record. Secrets a reader is sent as `********` read as `client.Secret`, and the lists a NAS writes two ways as `truenasrpc.Set`.

The client is generated, never edited: `make generate` makes it again from `api-defs`, and the build workflow fails when what is committed is not what that makes.

## Long jobs

A scrub or a replication runs for hours. `Call` waits for one to finish. `Start` returns as soon as the NAS has taken it on, and the job can be followed, waited for, stopped, or found again later by its number.

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

## Hearing what changes

```go
sub, err := nas.Subscribe(ctx, "alert.list", func(e truenasrpc.Event) {
	// e.Kind is truenasrpc.Added, Changed or Removed; e.Fields is how it now stands
})
defer sub.Close()
```

When the NAS restarts, the connection is made again and the NAS asked again without anyone noticing. The function is handed one `truenasrpc.Resumed` event. Whatever was said in between was not heard, so a program keeping a copy asks for the whole of it again.

## Files

Eight methods hand a file back (a saved configuration, a debug bundle, a dataset's keys) and eight take one. Neither goes over the connection calls are made on, so each has a function of its own.

```go
var backup bytes.Buffer
_, err := nas.Download(ctx, "config.save", []any{map[string]any{"secretseed": true}}, &backup)

var restored map[string]any
err = nas.Upload(ctx, "config.upload", nil, &restored, file) // waits for the NAS to deal with it

job, err := nas.StartUpload(ctx, "config.upload", nil, file) // or follow it as a job
```

The key is not sent with either: the NAS is asked, over the connection, for leave that is good once and for a few minutes. Nothing of a file's contents is written to a trace. A download that the NAS could not finish is an error, not a short file.

## On the NAS itself

```go
nas, err := truenasrpc.New("unix://", truenasrpc.LocalUser())
```

## Values TrueNAS writes its own way

`truenasrpc.Time`, `Date`, `TimeOfDay`, `Set[T]` and `IPInterface` read and write the five kinds of value TrueNAS wraps in an object (`{"$date": ...}` and so on), as fields of whatever struct an answer is read into.

A user who is not an administrator is sent `********` in place of every secret, even where the API's description says a list or an object belongs. `truenasrpc.Secret[T]` reads either, says which it was, and never prints its value. It is the same type as pandorest's `client.Secret[T]`, which the generated client uses.

A call the NAS refused is a `*truenasrpc.Error`, and `errors.As` also reads it as pandorest's `*client.CallError`, with the same fields, for code written against a generated client.

What else the NAS does that its API description does not say is written down in [api-defs/README.md](../api-defs/README.md).

## Beside TrueNAS's own Go client

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
