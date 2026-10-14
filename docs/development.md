# Development

```bash
make tools      # build the pinned dev tools into .tools/bin
make test       # the unit tests, with the race detector
make check-all  # build + test + every linter + depscheck
make testacc    # the live tests, against a throwaway TrueNAS in a virtual machine
```

`make help` lists the rest.

## Live tests

`make testacc` installs a real TrueNAS in a virtual machine and hands it to the tests in `acceptance/` as its installer left it: an administrator and nothing else. The machine is removed at the end.

**The first test sets the NAS up from nothing**, through the SDK, and reads the NAS back after each step. It gives it a name, a mirrored pool whose making is followed as a job, and two datasets. It makes a user who may only read and is refused a write, and a key for each user. Setting a NAS up from nothing is the hardest thing the SDK is asked to do, so it is a test and not a script. The tests after it use what it made.

**Every read that needs no argument is called**, once by the user who may only read and once by the administrator, and each answer is checked against the NAS's own description of its API. Whatever the NAS lists is also fetched by its id.

**The NAS is given real problems**, and how it reports each is checked: a pool with a disk taken out, a dataset filled to its limit, a pool with most of its space spoken for, a copy task that cannot reach where it copies to, a replication that would overwrite files, and a snapshot task that fails.

**A disk dies.** The machine the NAS runs in is told to take one of the pool's disks away, as a dead disk goes. The NAS has to notice by itself, and be whole again with a spare in its place. This runs late, since a dead disk stays dead.

**A disk lies.** A pool on a pair of disks has one disk written over from under it, then both. The first scrub has to repair everything and count the damage against the right disk. The second has to say what it could not repair.

**The NAS is restarted** with the tests' connections open. Each has to find the NAS gone and be logged in again by itself when it is back. What is different the morning after is then read: a dataset kept under a passphrase is locked, and a service set to start is running.

**Storage is set up and taken down.** A second pool goes through its whole life: a spare, a second disk, moved away and brought back. A pool is scrubbed and its care set. A dataset is made, changed, renamed and encrypted. A snapshot is rolled back, copied and held.

**Data protection is run.** A replication copies a dataset and then only what changed. Snapshot tasks and boot environments are made, changed and removed.

**Copies lose their other end.** The test NAS has no second machine and no cloud, so it is given both. It is set up to reach itself over SSH the way it would reach another NAS, and a replication and a cloud sync are sent that way; stopping the SSH server is the other end going away. A stand-in for S3 storage (`acceptance/s3stand`) runs beside the tests, and a cloud sync and a cloud backup go to it; what arrives is read from the storage itself, and it can be told to stop answering. Each copy has to work, fail and say so, and work again.

**Shares, apps and machines are run.** A share of each kind is made, served and removed, a volume is offered over iSCSI from end to end, and what rests on a locked dataset has to say it is locked. An app is run and stopped, and one that fails on starting has to be listed as crashed. A virtual machine is given a disk, copied, and started where the NAS can run one.

**The rest of the configuration is made and changed.** One of each of twenty-one kinds of thing a NAS keeps a list of (users, groups, keys, scheduled commands, routes, alert services, cloud credentials, iSCSI and NVMe parts) is made, changed and removed. One setting on each of fifteen pages is changed and put back.

**Everything truenas-metrics asks is asked its way**, with a key that may only read, on a NAS that has shares, tasks and a machine on it.

**Each run ends by counting** how many of the NAS's API methods the tests called, by kind, and which they did not. A key the tests made turning up in any line of trace fails the run.

Nothing of yours is ever a target: the tests and the setup both refuse a NAS that has a pool and is not named `throwaway-nas`.

## Where it runs

It needs QEMU and an x86-64 Linux machine with `/dev/kvm`, which a GitHub runner is, so the `acceptance` workflow is where it runs. TrueNAS does not run on ARM, so on a Mac it would have to be emulated, which is refused unless `TRUENAS_TEST_ALLOW_EMULATION=1`.

## A NAS left up

`eval "$(make testenv-up)"` starts the same NAS, set up by the same steps (`acceptance/setup`), and leaves it up with what is needed to log into it in the environment. `make test-live` runs the tests against it and `make testenv-down` removes it. What the NAS holds is written down in `acceptance/layout`.

## A NAS with more on it

`TRUENAS_TEST_FURNISHED=1 make testenv-up` also gives the NAS one of each thing a NAS is watched for: a share of each kind, an iSCSI target, a virtual machine, a snapshot task that has run, a replication that has not, and a copy task that failed. `acceptance/layout` names them. It is for anything that reads a NAS and wants something to read, as [truenas-metrics](https://github.com/katbyte/truenas-metrics) does.

## What the tests find

What a real NAS does differently from its source and its description is what the live tests are there to find. Each thing found is written into [api-defs/README.md](../api-defs/README.md) with the test that now holds it.
