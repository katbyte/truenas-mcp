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

**A disk dies.** The machine the NAS runs in is told to take one of the pool's disks away, as a dead disk goes. The NAS has to notice by itself, and be whole again with a spare in its place. This runs last, since a dead disk stays dead.

**More is made and removed**: a second pool on one disk, grown to a pair; a snapshot rolled back, copied, held and removed.

**Each run ends by counting** how many of the NAS's API methods the tests called, by kind, and which they did not. A key the tests made turning up in any line of trace fails the run.

Nothing of yours is ever a target: the tests and the setup both refuse a NAS that has a pool and is not named `throwaway-nas`.

## Where it runs

It needs QEMU and an x86-64 Linux machine with `/dev/kvm`, which a GitHub runner is, so the `acceptance` workflow is where it runs. TrueNAS does not run on ARM, so on a Mac it would have to be emulated, which is refused unless `TRUENAS_TEST_ALLOW_EMULATION=1`.

## A NAS left up

`eval "$(make testenv-up)"` starts the same NAS, set up by the same steps (`acceptance/setup`), and leaves it up with what is needed to log into it in the environment. `make test-live` runs the tests against it and `make testenv-down` removes it. What the NAS holds is written down in `acceptance/layout`.

## What the tests find

What a real NAS does differently from its source and its description is what the live tests are there to find. Each thing found is written into [api-defs/README.md](../api-defs/README.md) with the test that now holds it.
