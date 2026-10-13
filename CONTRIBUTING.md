# Contributing

Open an issue before a large change, so the shape can be agreed first.

## Before sending a change

```bash
make check-all
```

It builds, runs the unit tests and every linter. [Development](docs/development.md) has the rest, including the live tests against a real TrueNAS.

A change to how the SDK talks to a NAS needs a test. Something a real NAS does that its API description does not say is written into [api-defs/README.md](api-defs/README.md), with the test that holds it.

## Releases

- Changes collect under "Unreleased" in the [changelog](CHANGELOG.md), one short plain line each.
- A version is tagged when a batch is worth upgrading for, not for every change.
- Breaking changes are grouped into one release, not shipped one per release. Each is listed under "Breaking" with the before and after.
