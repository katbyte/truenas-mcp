# Contributing

pandorest is a library used by a handful of repositories, each generating a client for one server. A change here shows up in every one of them, so the first question of any change is what it does to their generated code.

## Before a change

- Run `make check-all`. It builds, tests under the race detector, and runs every linter CI runs.
- A change to the importer or the generator must leave the repositories' checked-in definitions and generated packages as they were, unless the change is the point. Regenerate one of them against your tree and diff it before calling the change done.
- Behaviour a repository did not ask for goes behind a setting in `config.Service` that is off by default.

## Releases

Changes collect under `Unreleased` in [CHANGELOG.md](CHANGELOG.md), as bullets a reader of the repositories can act on. A version is tagged when a batch is worth upgrading for, not per change. Breaking changes are grouped into one release rather than shipped one per release, so a repository moves once. A release that adds something is a minor version; one that only fixes is a patch.

The release is cut from the changelog: date the section as `## vX.Y.Z (date)`, push, wait for CI, then push an annotated tag. The release workflow writes the GitHub release from that section. A Go tag cannot be taken back once fetched, so CI goes green before the tag.
