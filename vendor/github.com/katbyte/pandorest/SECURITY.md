# Security Policy

## Supported Versions

Only the [latest release](https://github.com/katbyte/pandorest/releases/latest) is supported — please update before reporting an issue.

## Reporting a Vulnerability

Please **do not** open a public issue for security vulnerabilities.

Instead, report privately via [GitHub's private vulnerability reporting](https://github.com/katbyte/pandorest/security/advisories/new).

I will do my best to acknowledge reports within 2 weeks and aim to release a fix or mitigation within 6 weeks for confirmed issues; timelines are best-effort.

## Scope

`pandorest` is a code generator run at development time: it reads OpenAPI documents a repository has vendored and writes Go source. Anything that lets a document make it write outside the package directory it was given, delete a file it did not generate, or emit code that does something other than call the documented operation is particularly relevant.
