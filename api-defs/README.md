# What TrueNAS says about its API, and what it leaves out

`truenas-methods-<version>.json` is a TrueNAS's own description of its API: every method it has, with what each takes and what each answers. It is the answer to `core.get_methods`, taken as an administrator on a NAS of that version with `midclt call core.get_methods`, and named for the release it was taken from.

| | |
|---|---|
| `truenas-methods-25.10.7.json` | TrueNAS 25.10.7: 769 methods in 114 groups |
| `truenas-secrets-25.10.7.json` | where the secrets are in those methods: 1,040 places in 135 of them |

Neither holds anything of the NAS it was taken from: no names, addresses or settings, only the API's shape.

## Where the secrets are

The description does not say which values are secrets. The TrueNAS source does, so `truenas-secrets-<version>.json` is made from the source at the release's tag by `make secrets` (`.scripts/secrets-from-source.py`), which reads it without running it, follows every method to what it takes and answers, and checks every place it finds against the description. It followed all 769 methods of 25.10.7 and found nothing the description lacks.

Each method lists the places in its arguments (`params`) and in its answer (`result`):

| place | meaning |
|---|---|
| `$[1]` | the second argument, whole |
| `$[0].password` | the `password` of the first argument |
| `$` | the answer, whole |
| `$[].provider.token` | the `token` of the `provider` of each row of the answer |
| `$.attributes.*` | every value in `attributes` |

A method that is not listed has no secret the source marks.

## What the description does not say

Each of these was read in the TrueNAS source (`truenas/middleware` at the release's tag) and is held by a test, named after it.

**A long-running call answers a job's number unless the connection asks it to wait.** The description says what a method such as `pool.scrub.scrub` answers when it is done. A new connection is answered the number of a job at once instead, until it calls `core.set_options` with `legacy_jobs` off. 109 methods are jobs. The connection asks before it logs in. `TestALongCallAnswersItsResultNotTheNumberOfAJob`

**A message over 64 kB ends the connection.** Every other call waiting on it fails with it. Before login the limit is 8 kB, and two methods that take a file are allowed 2 MB. A call that long is refused before it is sent. `TestACallTooLongToSendIsRefusedBeforeItLeaves`

**Ten calls run at once for a connection, ten more wait, and the twenty-first is refused.** The connection sends ten and holds the rest back. `TestNoMoreCallsAreSentAtOnceThanTheNASWillRun`

**An API key sent without TLS is revoked on the spot.** The login answers `EXPIRED` and the key is gone for good. An address without TLS is refused before anything is sent. `TestSettingsThatCannotWorkAreRefusedWhenTheConnectionIsMade`, `TestARevokedKeySaysANewOneIsNeeded`

**Logging in with a key and no user name is going away.** `auth.login_with_api_key` is removed in TrueNAS 27; `auth.login_ex` wants the name of the user the key was made for. Both are supported. `TestAKeyAloneLogsInTheOlderWay`

**A TrueNAS 26 takes a login in which the key is never sent, and says so only when asked.** `auth.mechanism_choices` lists the logins a NAS takes, and `SCRAM` is among them from 26.0. Each side proves it holds the key, in two calls of `auth.login_ex`, and the number of the key rides beside the user's name (`user:7`). The connection asks, and uses it wherever it is offered. `TestANewerNASIsLoggedIntoWithoutTheKeyEverBeingSent`, `TestTheNewerLoginsMessagesAreTheOnesWorkedOutByHand`

**A 25.10 cannot say which logins it takes until someone is logged in.** Asked before, `auth.mechanism_choices` fails on the session it has not got yet (`'NoneType' object has no attribute 'may_create_auth_token'`). That failure, and a NAS with no such method, are both read as a NAS that takes a key only as it is. `TestACallSendsItsArgumentsInOrderAndReadsTheAnswer` and every other test against a stand-in 25.10

**The newer login is tied to the certificate the NAS showed, and the NAS checks the tie against its own.** Something in the middle that shows another certificate and passes every message along is refused by the NAS, as is a caller that comes through a reverse proxy. The NAS lets a login in before it has proved itself, so the connection checks the NAS's proof before making any call. `TestALoginPassedAlongBySomethingInTheMiddleIsRefused`, `TestSomethingAnsweringInTheNASsPlaceIsFoundOutBeforeAnyCallIsMade`, `TestALoginIsTiedToACertificateByTheHashItWasSignedWith`

**What a NAS answers the first message with is not trusted until it has proved itself.** It names how many times the key is to be stretched, and a number in the billions would keep a caller busy for hours: the count is held to the 50,000 to 5,000,000 a NAS uses. `TestAChallengeThatIsNotOneIsTurnedAway`

**Which logins a NAS takes is said before anything is proved, so it is only believed over a checked certificate.** Something in the middle could leave the newer login off the list to be sent the key. A key is sent as it is only where the certificate was checked, or the caller says it may be. `TestAKeyIsNotSentAsItIsToANASNothingVouchesFor`

**A job is answered by its number, and so are other things.** A new user's number is a number too, so an answer that is a number does not make a call a job. The NAS says a job was added before it answers the call that added it, naming the message that asked, and that is what a started job is known by. `TestAMethodThatIsNotAJobIsCarriedOutAndSaidNotToBeOne`

**A call that waits for a job holds one of the connection's ten places until the job ends.** A dozen replications waited for would leave no room for anything else. Jobs that are started go over a second connection, left answering the number, where they hold nothing. `TestAJobThatWasStartedTakesNoneOfThePlacesCallsAreMadeIn`

**What the NAS says of a job carries the job's answer with its secrets in.** Asked how a job stands, a NAS hands anyone but itself a copy with the secrets removed, unless asked for the answer as it was (`raw_result`); what it says unasked is never the copy. Both are asked for and hidden in a trace by the job's own method, at every one of the 1,040 places. `TestAJobIsFoundAgainByItsNumberFromAnotherConnection`, `TestAJobsSecretsAreHiddenInATraceAsItsCallsWouldBe`, `TestEverySecretTheSourceMarksIsHiddenInATrace`

**A job that fails gives the number of what went wrong and not its name.** The name is what tells "not found" from "not allowed", so it is looked up: Linux's numbers, and ten of TrueNAS's own from 201 to 210. `TestAJobThatFailsIsTheErrorACallThatFailedTheSameWayWouldBe`, `TestAnErrorsNumberIsNamedAsTheNASNamesIt`

**A NAS that restarts forgets its jobs, and keeps only so many that are over.** A job being waited for is then said to be lost, not to have failed. `TestAJobTheNASForgotWhenItRestartedIsSaidToBeLost`

**Five things a NAS says of, it says once for each time it was asked.** Live figures, a followed log and three more are sent again for every subscription a connection holds, where anything else is sent once. The NAS is asked once per name however many are listening. `TestWatchingTheSameThingTwiceAsksTheNASOnce`

**A subscription ends with the connection, and the NAS can end one itself.** Nothing is kept across a restart, so everything watched is asked for again; and a log that comes to its end, or a right taken away, is said in a message of its own. `TestWatchingGoesOnThroughANASThatRestartsAndSaysThereWasAGap`, `TestTheNASEndingWhatWasWatchedEndsTheSubscriptionWithItsReason`

**Four calls carry another method's arguments or answer, or an address good for one download by whoever holds it.** `core.bulk`, `core.job_wait`, `core.download` and `core.job_download_logs` show nothing of either in a trace. `TestACallThatCarriesAnotherCallsArgumentsOrAnswerShowsNeither`

**A file goes to and from a NAS by a web request, not over the connection.** Eight methods hand a file back and eight take one (`downloadable` and `uploadable` in the description). A download is asked for with `core.download`, which answers a job and an address good for one fetch; an upload is one request to `/_upload` carrying the method, its arguments and the files, answered with the job that deals with them. An upload is let in by the key itself or by a token asked for with `auth.generate_token`, and the token is what is used, so that the key never rides on a web request. `TestAFileTheNASMakesIsFetchedWholeAndNothingOfItReachesALog`, `TestAFileIsHandedToTheNASAndTheJobThatDealsWithItAnswered`

**A NAS starts sending a file before it knows it can finish it.** One that fails part way is simply cut short, with a web answer that says all is well. The job that made the file is asked how it ended once the file stops, and its failure is the download's. `TestAFileTheNASCouldNotFinishIsSaidNotToBeWhole`

**A thing that is not there is refused as a bad argument.** Asked for a dataset, a pool or a job by an id it has nothing for, a NAS does not answer that the call failed. It answers "Invalid params", with no argument named and the number for "not there" beside the sentence. That is told apart from a real bad argument, so `IsNotFound` is true of it and `IsInvalid` is not. Found by the first live run, where the stand-in had it wrong. `TestARefusalSaysWhatKindItIs`, and live in `TestARealNASsRefusalsAreToldApart`

**Over its own socket a NAS logs a connection in as it opens.** It can see which user is at the other end, and takes root or any user it has an account for with nothing sent. The connection asks who is logged in, so that a user the NAS does not know is told so when it connects and not at its first call. `TestAProgramOnTheNASItselfIsLoggedInByWhoItRunsAs`, `TestAUserTheNASDoesNotKnowIsToldSoAndTheSocketsLoginIsOnlyForTheSocket`

**A trace hides every secret the source marks, and three it does not.** A trace hides by name and by method, not by the list above, so the list is what holds it to account: a marker is planted at each of the 1,040 places and looked for in what a trace would print. That found a mail account's password (sent as `pass`), a Kerberos keytab (`file`) and a container registry's user name being shown, and they no longer are. The source leaves three generated secrets unmarked, the answers of `auth.generate_token`, `nvmet.host.generate_key` and `tn_connect.generate_claim_token`; those are found from the description, as bare text it calls a key, a password or a token. `TestEverySecretTheSourceMarksIsHiddenInATrace`, `TestNoMethodShowsASecretItTakesByPositionOrAnswersBare`, `TestWhatATraceShowsOfAMessage`, `TestNoSecretReachesTheLogOrAnError`

**A field can be sent under another name than the source gives it.** A mail account's password is `pass_` in the source and `pass` to a caller. The list of secrets uses the name a caller sees. `TestEverySecretTheSourceMarksIsHiddenInATrace`

**A failure can quote back what it was sent, and carries a record of where it happened.** The record holds whatever the NAS had in hand at the time. Neither is kept: the key is blanked in every error, and the record is dropped. `TestNoSecretReachesTheLogOrAnError`

**The certificate a TrueNAS comes with is made out to `localhost`.** Trusting it as an authority does not help, because the NAS is never reached by that name. Handed over as the NAS's own certificate, it is accepted by its key, whatever name and dates are on it. `TestTheCertificateANASComesWithIsAcceptedByItselfWhateverNameIsOnIt`, `TestANASIsKnownByItsKeyThroughARenewalAndAnExpiryAndNotByAnotherKey`

**A session can be ended by the NAS while the connection stays open.** The next call is answered "Not authenticated". The connection is dropped and the call after it logs in again. `TestASessionTheNASEndedIsLoggedIntoAgain`

**A user who is not an administrator is answered `********` where a secret is, whatever the secret is.** The description says `mail.config` answers its `oauth` as an object, `system.general.config` its `ui_certificate` as an object and a user's `password_history` as a list. A full administrator gets those. Anyone else, unless they hold the role that may change that part of the NAS, gets the one piece of text in place of each, so an answer read strictly as the description has it fails to read for a user who may only read, which is exactly the user a program that watches a NAS should be given. Seen on a real NAS and then read in the source (`dump_result` in `main.py`, `SECRET_VALUE` in `api/base/types/string.py`). `Secret` in `sdk/truenasrpc` reads either. Every place a real NAS kept something back is a place the secrets list names, and the administrator is shown them all. `TestASecretTheNASKeptBackIsReadAsOneAndNeverPrinted`, `TestEveryReadIsAnsweredTheWayItsDescriptionSays`

**The audit log cannot be listed without a limit.** The description says `audit.query` needs no argument. Called with none it is refused: its options have to hold a limit on the rows, or ask only for a count. `TestEveryReadIsAnsweredTheWayItsDescriptionSays`, `TestReadsThatAreToldWhatToReadAnswerTheWayTheirDescriptionSays`

**A disk has no id.** Every other kind of thing a NAS lists has an `id`, and is fetched by it. `disk.get_instance` is described as taking an `id` too, but a disk's row has none: it is fetched by its `identifier`. `TestWhatIsListedIsFetchedByItsIDAndWhatIsNotThereIsMissing`

**Which methods only read is not said.** The description lists the roles that may call each method, and a method the read-only administrator role may call is nearly always a read: 386 of the 769. Five are not, and nothing in the description tells them apart: a read-only administrator may make, change and remove API keys of its own (`api_key.create`, `api_key.update`, `api_key.delete`) and send a ticket to TrueNAS's support (`support.new_ticket`, `support.attach_ticket`), which the source gives it by name. So the role says who may call a method, not that it changes nothing, and a program that promises only to read cannot go by the role alone. Twenty-five methods are given to that role by name; each was read in the source, and a release that adds one fails a test until it has been too. 33 methods list no role at all: logging in, subscribing, asking after jobs, and a user setting their own password. `TestEveryMethodTheReadOnlyRoleIsGivenByNameHasBeenLookedAt`, `TestEveryMethodIsSortedByHowItCanBeTested`

## What a real NAS answers, held to the description

Everything generated from the description is only as right as the description. So the live tests call every read that needs no argument (252 of them) on a real TrueNAS, as a user who may only read, and hold each answer to what the description says it is: a name it says is always there, a name it does not have, a value of another kind, a value outside the ones allowed. Whatever a NAS lists and can hand over one at a time is fetched by its id as well, and an id nothing has is asked for, since "it is not there" has to be the same answer whatever was asked for. `TestEveryReadIsAnsweredTheWayItsDescriptionSays`, `TestWhatIsListedIsFetchedByItsIDAndWhatIsNotThereIsMissing`

The first time this ran, on 13 October 2026, 246 of the 252 answered as described, and "it is not there" was the same answer for every one of the 56 kinds of thing fetched by an id. Three reads are refused on a NAS like the test one, each for a reason that is its own: `app.available_space` until a pool is chosen for apps, `tn_connect.get_registration_uri` until the NAS is joined to TrueNAS Connect, and `audit.query` as above. The other three were secrets kept back, above. Asked by the administrator, who is shown the secrets, all 249 that answer do so as described.

The secrets list, which is made from the source, held against a real NAS: everything it kept back from the user who may only read (14 places in 9 methods, from an API key's hash to the SNMP passwords) is a place the list names.

Thirty-four reads that have to be told what to read are asked about what the journey made, and answer as described. One of them refuses, which is its job: `sharing.smb.share_precheck` says a NAS needs a user who may use SMB before it can share anything. `TestReadsThatAreToldWhatToReadAnswerTheWayTheirDescriptionSays`

What a real NAS does some other way is written down beside those tests (`surprises`, in `acceptance/truenasrpc/reads_test.go`), each with why. One that stops being true fails too, so the list stays what is known and does not become a way to quiet a test.

The run ends by saying how many of the NAS's methods the tests called, by kind, and which were not: how much of the API is tested is counted, not claimed. Every connection the tests make is traced to do it, so every answer a real NAS gives also passes through the code that hides secrets, and a key the tests made turning up in any line fails the run. `TestTheRunSaysHowMuchOfTheAPIItTriedAndShowedNoKey`

## How a NAS says something is wrong

None of this is in the description: an alert's text is whatever the NAS writes, and the state a task keeps has no shape there at all. Each was read from a real NAS that was given the problem, and the tests that give it the problem hold what was read.

**A pool with a disk gone says so itself, at once, and in an alert within a minute or two.** With one disk of a pair taken out, the pool's `status` is `DEGRADED`, `healthy` is false, and the disk's own `status` in the topology is `OFFLINE`. The alert is class `VolumeStatus`, level `CRITICAL`, and both clear when the disk is back. The alert's text is HTML, with a list in it, and names the disk by a model it does not have: `Pool tank state is DEGRADED: One or more devices has been taken offline by the administrator. Sufficient replicas exist for the pool to continue functioning in a degraded state.<br>The following devices are not healthy:<ul><li>Disk None TNTEST1 is OFFLINE</li></ul>`. `TestAPoolThatLosesADiskSaysSoAndIsWholeAgainWhenTheDiskIsBack`

**How full a dataset is has to be read from the dataset.** The NAS looks at quotas itself once an hour and at how full a pool is every five minutes, so its alerts say nothing for a while. A dataset's `used`, `available` and `refquota` (each with the number under `parsed`) are right as soon as the file is written. A file that would go over the limit fails with `[Errno 122] Disk quota exceeded`. `TestADatasetFilledToItsLimitShowsHowFullItIsAndTakesNoMore`

**Space kept aside counts as used where it matters.** A volume made with its space reserved takes that space from everything else in the pool though nothing was written to it. What the pool's top dataset says is `used` and `available` shows it. The pool's own `allocated` is a count of what has been written and still reads as next to nothing, so a check of space that reads only the pool calls a pool with nothing left empty. `TestAPoolWithMostOfItsSpaceSpokenForShowsLittleLeft`

**A copy task that fails says so three ways.** The run fails (`[EFAULT] rsync command returned 10 - SOCKETIO. Check logs for further information.`), the task's row keeps the job of its last run with `state` `FAILED`, and an alert of class `RsyncFailed`, level `CRITICAL`, is raised at once: `Rsync "PUSH" task for "/mnt/tank/alpha" failed.` That alert stays until somebody dismisses it, and dismissed it is still listed, marked `dismissed`. `TestATaskThatCannotReachWhereItCopiesToIsSaidToHaveFailed`

## Making a NAS to test against

TrueNAS documents only an install by someone at a keyboard. Its installer also answers a small API of its own while it sits at its menu (`truenas/truenas-installer`, read at the release's tag): a websocket on port 8080 that lists the disks, installs onto the ones named with an administrator's password, says how far along it is, and switches the machine off. That is what `.scripts/testenv.sh` drives to make the NAS the live tests run against. `TestTheInstallerIsDrivenWithNobodyAtTheKeyboard`, `TestAnInstallThatCannotBeSureOfItsDiskOrIsRefusedSaysSo`

Another repository runs its own live tests against this NAS: truenas-metrics calls `.scripts/testenv.sh` (`image`, `up`, `down`, `logs`), reads the lines `up` prints, and keeps the installed disk by the same two files this repository does. A change to any of those is a change for it too.

The installer lists no disk under 8 GB, and names disks as the machine happens to find them, so the disk to install onto is told from the others by its size.

Seen on the first run, on a GitHub runner, 13 October 2026: the installer disc starts that API unasked and answers half a minute after the machine starts; the install itself takes a little over a minute; the installed NAS takes a minute and a half to come up the first time; and a virtual disk's serial reaches the NAS as it was given. Fetching the installer disc, at seven minutes, is most of the wait.

## Not yet held by a test

- a query told to answer the one match (`get`) when there is none. The source raises something other than "not there" for it, which has not been tried against a real NAS

- a query can answer a list of rows, one row or a count, and the description of the one row leaves out the shape of any model inside it: `cloudsync.query` describes a row's `credentials` in full in the list, and as a title with no shape in the single row
- a time of day, a day, and an address with its network, as TrueNAS writes them its own way: none has been seen in a real answer. The description has 5 places that could hold the last (in `smb.config`, `smb.update` and `docker.update`). The other two forms have been seen, and the live run lists every place it meets one: a moment (`{"$date": ...}`) in `system.info`, a pool's scan times, a dataset's and a snapshot's creation, a boot environment, an API key, a session and a user's last password change; and a set (`{"$set": [...]}`) in `system.security.config`, as its `password_complexity_ruleset`, which the description calls a list
- downloads and uploads have only been run against the stand-in, written from the 25.10.7 source (`apps/file_app.py`). Whether a NAS serves either through its own socket was not checked, so both are refused there
- the newer login has only been run against the stand-in, never a real TrueNAS 26. The stand-in's half was written from the 26.0.0-BETA.3 source and TrueNAS's own Go client logs into it every way this connection does, but what a real NAS answers a revoked key with in the newer login is a guess (the stand-in refuses it as a wrong key)
- seen in a real 25.10.7's answers by the truenas-metrics exporter, and waiting for the generated client to be held by a test here:
  - a pool's `fragmentation` is text (`"14"`) where a vdev's is a number
  - a scrub's percentage can pass 100
  - a certificate's `from` and `until` are text such as `Wed Dec 23 12:57:57 2026`, where other times are `{"$date": milliseconds}`
  - `disk.temperatures` answers `null` for a disk that does not report one
  - `pool.dataset.query` with `flat` and `retrieve_children` both on still nests each dataset's children inside its row
