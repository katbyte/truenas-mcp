## v0.9.1 (2026-10-13)

- **A union member keeps every kind the document sends to it.** Where several kinds name one record, such as a mirror, a stripe and the raidz levels all being one vdev, one kind was kept at random, so two runs of the generator differed and a record of the other kinds was refused. Every kind is kept, in order, and a record of any of them is that member.
- **`check` finds a call's method under its short name.** In a package per namespace it reported every method of a JSON-RPC service missing, since it looked for the full name. It now looks on each group's own client for the name the method is generated under, so one group's `Query` does not stand for another's.

## v0.9.0 (2026-10-13)

- **A server with no OpenAPI document can be generated from its own method list.** TrueNAS is retiring its REST API and describes its JSON-RPC methods instead. The config says `Document: config.JSONRPCDocument`, and each method becomes a call with its arguments in order, grouped by namespace.
- **The package never sees a socket.** The repository writes the connection and hands it to `New` as a `client.Caller`. A refusal from the server is a `client.CallError` with its code, name and reason.
- **Method names drop their namespace in a package per namespace.** `pool.query` is `c.Pool.Query`. The response type keeps the whole name.
- **A record written out many times is one model, and an enum met in many places is one type.** Both are folded by what they hold, and named by the words their places share. A group named `Service` goes in `Service.group.json`.
- **A query has three forms.** `Query` answers the list, `QueryOne` the first match and `QueryCount` how many. The two flags that choose the form are gone from the options, since the method sets them.
- **A value that is one of several records can be a type of its own.** With `TypedUnions` on, it hands the record over as the member asked for and says which kind came. Off, it stays raw JSON, so no repository moves.
- **A method that answers a file, or takes one, works.** It gets a `To` form that writes the file out, or takes files as readers at the end. The connection carries the file through `client.Downloader` and `client.Uploader`.
- **A job's call can be had without the wait.** `<Name>Call` answers the method name and arguments for a connection that starts a job and follows it.
- **The read sweep walks a service of calls.** Each read is called through the generated client, every answer judged as it came, and a refusal held to the error name its case expects.
- **A program can read what each operation is.** Every package has `Operations`, a table of each operation with its method and path, its group, whether it changes nothing, runs as a job, is a query form, or carries a file. A repository regenerating on this version gains the one file.
- **A flag in a record a call sends is left out unless set.** Changing a scrub's threshold no longer switches the scrub off.
- **A list sent unset goes as an empty list.** The server reads null as no list at all.
- **A method counts as a read by the roles that may call it.** `ReadRole` names the role, `Reads` and `NotReads` the exceptions, and the import refuses a name that is no method.
- **A field can be held as a Go type the workaround names.** `NamedTypes` puts any type on any field. `client.Secret[T]` is the one for a value the server hides behind `********`, and prints as `***` so a logged record gives nothing away.
- **An open field can be given a shape.** `DeclaredShapes` puts a declared record on a field the document leaves empty.
- **Query filters are written for you.** `client.Where`, `client.Filters` and `client.AnyOf` build the nested lists the server reads.
- **A date written as `{"$date": milliseconds}` is read and written.** `client.DateObjectMillis` names the form.
- **An enum's empty value is named `Empty`, and a value whose plain name another has is spelled out.** `!data` beside `data` becomes `NotData`.

## v0.8.3 (2026-10-12)

- **`KeepNull` reaches an answer declared inline.** It was checked before the operations were read, and an inline answer is a model only once its operation is, so a document with no components could name nothing. Dockhand's registry test answer.
- **The import says when a text parameter named like a list is typed as one id.** Emby's `ExcludeItemIds` carries several ids in one text value, which only `Joined` says, and by name it came out as a single id with nothing said. The line names each such parameter and where it is.
- **A service's `Notes` come before the Ids heading in `doc.go`**, where they landed under it and read as part of it.
- **`Of` reads a model whose id is spelt `Id`.** Emby and Jellyfin capitalise theirs, and the lookup wanted the exact lower-case name, so the kind was refused as naming a model with no id. Embyfin got past it with a `Base` beside `Of`; that can go.
- **The generated test of a joined list sets one form.** `JoinedLists` puts a list beside its string under one name on the wire, and the test set both on one call and then expected each alone, so every such test failed; on Emby that was 70 across 21 packages. The test now sets the string alone.

## v0.8.2 (2026-10-12)

- **A name in both `Names` and `Joined` imports.** Prowlarr's `tags` is a joined string parameter and a field of fifteen models; v0.8.1 claimed the parameter by name first and then refused the `Joined` entry as reaching nothing, and without the entry refused the parameter as a string. The parameter is claimed as joined before it is claimed by name.

## v0.8.1 (2026-10-11)

- **`doc.go` carries no generated header after all.** A linter skips a file marked generated and then misses the package comment, and flags the hand-written file beside it as lacking one, which sonarr's move found; the comment's first line says what wrote it, and every check knows that form.
- **A test naming a method is not a tool wrapping it.** `check` counts only non-test files, and `-callers` with it.
- **`KeepNull` names a field of an inline record by the model's name** (`PostRegistriesTestAnswer.authenticated`), where it took only the document's schema name and a document with no components could name nothing. Dockhand's one answered boolean where absent is not false.
- **A command's wait says which way it ended.** `WaitForCommand` wraps `client.ErrCommandFailed` for a command the server failed and `client.ErrCommandRunning` for one still running when the wait ran out, so a caller that takes the second as an answer tests `errors.Is` rather than reading the status; the failure names the command and carries its exception beside its message where the answer has them. A command model's comment no longer says the method twice. Radarr's move, 16 commands declared.
- **`Joined` reaches parameters alone and says when it reaches none.** A field of the same name is no longer claimed by it, where prowlarr's `indexers` needed an `Except`, and an entry written as a place rather than a name is refused saying so.
- **The sweep's closing lines say what a place is**: "32 places in what a GET answers", so a reader does not take the fields no GET can answer for missing. Embyfin's note.
- **`NeverCalled` counts a call for the one operation it was for.** A generated method names its operation on the request, so no path matching is needed; a hand-written call goes to the most literal template it fits, where a lookup beside a by-id path used to count for both; and an excused operation the run called after all is reported, so the list cannot go stale. Sonarr read both against the lines it replaces.

## v0.8.0 (2026-10-11)

- **For a server that queues work by name.** `Commands` declares each command and its fields once, and the client gets a method per command, an `AndWait` form that queues it and waits until the server says it finished, and `WaitForCommand` for one queued the plain way. The hand-written maps and poll loops in the three *arr repositories go.

- **For a live suite.** `client.WithCalls` records every request sent, and `sweep.NeverCalled` says which operations none reached and are not excused with a reason. `check -callers tools/` counts the methods a tool names rather than every file in the repository.

- **Secrets stay out of errors.** The credential, and any value named with `client.WithSecrets`, reads `***` wherever an error would have repeated it. The generated `New` names the credential a secret; a service that writes its own `New` passes `client.WithSecrets` itself, or the generated test says so.

- **A 200 that says no.** `Outcome` on a service names the field and the values that mean the call failed, and such a call is an error with the answer still decoded.

- **Every generated file carries the header**, `doc.go` included; the old form is still recognised, so the first generation after upgrading replaces it. (Undone in the next release: see above.)

## v0.7.0 (2026-10-11)

- **Renames, each one replacement.** The answer is `Answer`, not `Model`. The options struct is `<Name>Options`, not `<Name>OperationOptions`. The response is `<Name>Response`, not `<Name>OperationResponse`, and a model invented for an answer the document declares inline is `<Name>Answer`, not `<Name>Response`; rename those first where a document has them (TMDB, Dockhand), or the two collide.

- **Booleans by value.** A boolean in a record the server only ever answers is a plain `bool`, false when left out, instead of a pointer every caller read through a helper. One a request sends keeps the pointer unless the record is written back whole or the document requires the field, so a flag never set is not sent as false; a nullable one, or one named in `KeepNull`, keeps it anywhere.

- **Reading an answer.** `Answered()` says whether the server answered with a body, which a nil check could not tell from an empty list. A record answered is also embedded, so `res.Name` reads beside `res.Answer.Name`, and is a zero record rather than a crash when nothing came; `Record()` hands it back by value or an error. A file answer has `WriteTo`, `Bytes(max)`, `Reader()`, `ContentLength()` and `Filename()`. A raw answer reads into a type of the caller's own with `client.Decode[T]`, strictly with `DecodeStrict[T]`. `GetName()` and `GetId()` sit on every record with both, so one piece of code can take several kinds.

- **Calling.** An operation whose options are all optional takes them variadically, so a call with no filters is `c.GetSessions(ctx)`. With `InlineSingleOptions`, an operation with one option takes it as the argument. A list operation has `Each` beside `Complete`, a range that stops paging when the loop breaks. `IsBadRequest`, `IsConflict`, `IsTooManyRequests` and `IsServerError` join `IsNotFound`, `IsUnauthorized` and `IsForbidden`.

- **Dates, ids and enums, each a setting, off by default.** `Dates` and `Days` turn every date into a `time.Time` read from the layouts given and written in the one the server wants; a date no layout reads fails loudly, or is kept as text with `KeepUnread`. `IDs` names the kinds of id the compiler keeps apart, inferred from the models they belong to; `Shape` refuses a malformed one before any request, and `Plain` quiets the log for the rest. Enums gain `Known()`, and the sweep fails an answer carrying a value the document does not list.

- **Layouts.** `Grouped: true` lays the SDK out as packages the way go-azure-sdk does, one package per tag under a types package, with `groups.json` beside them for a caller moving over; `ClientPackage` renames the client package where `client` collides. `Outputs` generates every version of a document a service vendors, each a package of its own.

- **Workarounds in the default set**, each applied before import and refused when the document no longer has the fault: `UndeclaredDates`, `UndeclaredDays` and `UndeclaredUnixDates` for dates declared as strings or numbers; `UndeclaredEnums` for a string with a fixed set of values; `EnumValues` for a value a declared enum lacks; `JoinedLists` for a string the server reads as a comma-joined list; `NumberedPathIDs` for a text path id that is a number elsewhere on the path; `OneRecord` for a record declared inline in several places; `DeclaredSchemas` and `AlsoAnswers` for a record the document leaves out and a body at a status it gives none. The ready-made ones are now called the default ones. A property under a map's values is reached, which it was not. The import logs what each reached.

- **Checks.** `check` counts the methods a hand-written file names, and `-uncalled` lists the rest. `workarounds.Verify` checks each workaround against the document as the ones before it left it, in order, as the import does. `generate` refuses to overwrite a hand-written file. The sweep ends with a count of what the run came to and notes of what its answers left unproven: enum places and listed values never seen, date places and by-value booleans no answer carried; `MissingIDs` probes what the server says to an id nothing has.

- **Paging shapes beyond the first two.** A page size the server fixes is learnt from the first page, an answer that names its list differently each time has its one list taken, and `Pagings` lists further shapes for a service whose lists do not all page alike; the import says which list operations get no pager and why.

- **Small things.** `Auth` may name an authorizer declared in the service's own file. A credential described in more than two words names the parameter `credential`. `client.Logger` and `client.Retry` name what `WithLog` and `WithRetry` take.

## v0.6.1 (2026-10-10)

- `KeepNull` takes a string field as well as a nullable number: the field is held as a `*string`, left out when nil and sent as `""` when it points at one, for a server that keeps whatever a write leaves out and clears a field only when it is sent empty (Dockhand's `publicIp`). A string held by value is left out when empty, so such a field could not be cleared
- the README lists the SDKs generated with pandorest, with their import paths and repositories

## v0.6.0 (2026-10-10)

- add ready-made workarounds to `importer/workarounds`, for the bugs most documents have and each repository patched by hand: `UndeclaredAnswers` (a success with no content), `WrongAnswers` (one answer declared, another given), `WrongStatus` (a create documented as 200 that answers 201), `UndeclaredParameters`, `UndeclaredProperties` (fields the server sends that a schema leaves out) and `NotAPI` (the web interface's paths). A repository gives an `About` (name, service, bug) and says which operations; the patching and the check that the bug is still in the document are here. An answer is said with `JSON`, `JSONString`, `Model`, `ListOf`, `Shape`, `File` or `Text`
- `workarounds.Verify` says a workaround "does not apply to the vendored document, so the bug is not there or the workaround is written wrong", where it said only that the bug was not there: a schema name misspelt in a workaround's own table read as the bug having gone
- add text as a kind of answer: short text that is not JSON (a path, a version) is read whole into the method's `Model`, a `*string`, as it was sent and whatever the server calls it, where it was a file its caller had to read and close. Only a workaround asks for it (`workarounds.Text`), since a log is text too; nothing moves for an operation that does not. A base client of a repository's own needs `Response.Text` if it has one
- `openapi.JSONMedia` and `openapi.PickJSON` are the importer's rules for which media types are JSON, for a workaround to share

## v0.5.0 (2026-10-10)

- a workaround can give an operation an `Accept` option, for a server that chooses its answer by the header where the choice is the caller's to make call by call (Dockhand's slow actions wait and answer how it went when asked for JSON alone, and answer at once with a job to follow when asked for anything else). The importer keeps a header parameter named `Accept` that a workaround adds (`AddParameters` with `Param{Name: "Accept", In: openapi.InHeader}`), where it ignores a document's own as OpenAPI says to; the option is generated like any header option, and when set its value is the whole header. Nothing moves for a repository that adds none

## v0.4.0 (2026-10-10)

- a call asks for what its operation is documented to answer: one that answers a file or an event stream sends that type first in `Accept` with JSON after it (`text/event-stream, application/json;q=0.9`), where every call asked for `application/json` alone. A server that chooses its answer by the header (Dockhand's dashboard stream answers JSON to a caller that asks for JSON alone) now sends what its document says. Calls that answer JSON, or nothing, are unchanged
- generator (breaking for a base client of a repository's own): a method that answers a file sets `Accept` in its `RequestOptions`, so such a client needs the field; a regenerated package gains one line in each such method and, in its test, the check of the header (three lines, and the `strings` import where the test had none)
- sweep: an event stream (`text/event-stream`, by its document or by what the server answers) is read as far as its first event and then hung up on, where it was read like a file and ran out of time, so that each one needed a `Skip`. One that sends no event in `EventWait` (ten seconds unless set), or ends without one, is an answer with nothing in it and takes an `Empty` or `MayBeEmpty` case like an empty list; one that answers another content type than the event stream its document says, or breaks off part way, fails

## v0.3.2 (2026-10-10)

- sweep: a path parameter's fixture is looked up by as many of the literal segments before it as there are, the most of them first, where it was only ever the one: `config/indexer/id` is found before `indexer/id`, so the settings under `/config/indexer/{id}` can be given another id than the indexers at `/indexer/{id}` with no case for it. A fixture keyed by one segment or by the name alone is found as before

## v0.3.1 (2026-10-10)

- a service words the generated `New` without copying it: `Credential` is what it calls its credential ("API key" names the parameter `apiKey` and refuses an empty one with "API key is required"), and `NewDoc` is New's documentation; a `Client` source is now only for a `New` of another shape
- the generated `doc.go` no longer starts with the generated header, so that a linter sees the package comment in it: a file written by hand beside the generated ones (the service's own) was otherwise told its package has none, and every repository carried an exception for it. `generator.Generated` still knows `doc.go` as generated; a test that tells generated files by `generator.Header` must use it instead
- `client.Service.Example` can be a host alone (`nas:7878`, shown as `http://nas:7878`), so a service's file holds no `http://` address for a linter to object to

## v0.3.0 (2026-10-10)

- add `client`: the base client every generated package sends its requests through, one copy in place of the one each repository kept, built on go-kt's `chttp`; what is a service's own (its name, how long it may take, what its refusals mean, how its error body reads) is a `client.Service` the repository writes by hand beside its generated package and names with `ClientService`
- generator (breaking): the default `client.go` is built on `client` unless `ClientImport` names another, and its `New` takes the base client's options after the token, so a base client of a repository's own needs an `Option` type and a `New` that takes them
- a status error keeps what the server said as a list (`StatusError.Messages`, `client.Messages`) as well as joined in its text
- what happens to a redirect is the document's to say, per operation: one that documents a redirect and no 2xx answers the redirect, handed back unfollowed (it used to fail the import); one that documents a redirect beside a 2xx expects both and has the redirect followed to its answer; a redirect no operation documents is refused or followed as the service says
- `internal/client`, the stand-in the tests built generated packages against, is gone: they build against `client`

## v0.2.0 (2026-10-10)

- add `sweep`: the read sweep a repository runs against a real server, every GET in a service's definitions called on the generated client and its answer held to decoding, carrying something and losing no object whole; one package made of the copies in embyfin-mcp, sonarr-mcp, radarr-mcp and prowlarr-mcp, with radarr's check for undeclared fields as `Strict` and prowlarr's `Sometimes`
- importer (breaking): two operations that make the same method name fail the import, where the second used to be numbered (`GetAudit2`) with a warning; which one took the number followed the order of the paths, so a refreshed document could rename a method
- a workaround can name an operation (`Name` on `openapi.Operation`), which is how such a clash is settled

## v0.1.0 (2026-10-09)

- initial release: the SDK generator extracted from embyfin-mcp's `sdk/pandorest`, where it writes the Emby, Jellyfin and TMDB SDKs, so the copies in sonarr-mcp, radarr-mcp and prowlarr-mcp can be replaced by one
- a repository hands its services and its workarounds to `pandorest.Main` from a main of a few lines; the service list, the workarounds for a document's bugs and the base client stay in the repository that owns the documents
- the base client a generated package is built on is named per service (`ClientImport`, `Auth`), and a service can give its own `client.go` (`Client`) and a paragraph for the package documentation (`Notes`)
- which operations are lists is said per service (`Paging`): the two parameters and two properties, and whether the list pages by start index (Emby, Jellyfin) or by page number (Sonarr, Radarr, Prowlarr)
- taken from the Sonarr, Radarr and Prowlarr copies, each off unless a service asks: `PathPrefix` and `Words` for method names, `WrittenWhole` for settings saved whole, `SendRequired` for required properties, `PreferJSON` for a response that lists JSON beside another type, and `ExpectSameShape` for a failure that answers the success shape
- a generated package declares `DocumentVersion`, the version its document's file is named by, beside `APIVersion`, the document's own, which for some servers does not move from release to release
- `workarounds.Verify` is the test every workaround must pass, in one call: it applies to its vendored document, fails the second time, and does not panic on a document that has lost its 200 responses
- generated files start `// Code generated by pandorest; DO NOT EDIT.`; a file with the header of one of the earlier copies (`sdk/pandorest`, `internal/pandorest`) is still recognised as generated, so stale ones are removed on the first run
- a service with no lists gets no paged test server in its generated tests and no word of paging in its package documentation
