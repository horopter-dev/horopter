---
title: horoctl
description: The horoctl command line, which manages tenants and credentials and applies migrations.
---

`horoctl` works directly against the database: no daemon needs to be running, and it presents
no credential of its own, since its authority is database access. Every subcommand takes
`--dsn`, which defaults to `$DATABASE_URL`. How to use it is
[Issue Credentials](https://docs.horopter.dev/how-to/issue-credentials/).

It exits `0` on success, `1` on an error, and `2` when run without a command.

## `horoctl admin tenant create`

    horoctl admin tenant create --display-name acme

Creates a tenant. Prints its opaque id, `tnt_` and 32 hex characters, on stdout.

| flag | meaning |
| --- | --- |
| `--display-name` | a human-readable name, which can change; required |

## `horoctl admin tenant list`

Prints each tenant's id and display name, tab-separated, oldest first.

## `horoctl admin keys issue`

    horoctl admin keys issue --role reader --tenant <tenant-id> --source github

Issues a credential. Prints the key on stdout **once**: it cannot be recovered afterwards.
What it issued, its prefix, role, tenant and locus, goes to stderr.

| flag | meaning |
| --- | --- |
| `--role` | a role the `role` table holds; `producer`, `reader`, `tenant_admin` and `superuser` are seeded. Required |
| `--source` | the source every event the key appends is recorded with, e.g. `github`. Required |
| `--tenant` | the tenant the key is scoped to; omit it for a global key |
| `--locus` | the namespace the key's refs are stored under; defaults to `--source`. It may not contain `/` |
| `--subject-pattern` | restrict the key to subjects matching this `path.Match` pattern. Refused for a role holding anything but `append` |
| `--key` | an explicit key, `horo_<12 hex characters>_<secret>`, at least 32 bytes; development only |

## `horoctl admin keys list`

Prints each credential's prefix, role, tenant (`global` for none), locus, source and whether it
is revoked, one per line, oldest first.

| flag | meaning |
| --- | --- |
| `--tenant` | only credentials scoped to this tenant |

## `horoctl admin keys revoke`

    horoctl admin keys revoke --prefix <prefix>

Revokes a credential. Daemons refuse it within their credential cache's TTL, 30 seconds.

| flag | meaning |
| --- | --- |
| `--prefix` | the key prefix of the credential to revoke; required |

## `horoctl admin bootstrap`

Creates a tenant if it is absent, and issues a `producer` and a `reader` key if their prefixes
are absent: the development stack's own setup, run by its `bootstrap` service. It never issues
a `superuser`. Run again, it rebinds a key left on another tenant or role, naming what moved on
stderr, and stops at a revoked one.

| flag | meaning |
| --- | --- |
| `--display-name` | the tenant to create if absent; required |
| `--source` | the source of both keys |
| `--locus` | the locus of both keys; defaults to `--source` |
| `--producer-key`, `--reader-key` | the two keys, in `--key`'s shape; required |

## `horoctl migrate`

Applies every pending migration, the same code as `horopter-migrate`. Run it to completion
before starting a daemon at a new version: each daemon refuses to start against a schema
version it does not know. All pending migrations run in one transaction.

| flag | meaning |
| --- | --- |
| `--rebuild-tenant` | after migrating, discard a tenant's stored changes and derive them again from its events; see [Rebuild a Tenant's Projection](https://docs.horopter.dev/how-to/rebuild-a-tenants-projection/) |

## Plugins

Any other command runs `horoctl-<command>` from `PATH`, kubectl's model, with the remaining
arguments, and exits with its code. The plugin is given `HOROCTL_VERSION`, the version
`horoctl --version --format bare` prints, to check it is compatible. `migrate` and `admin` are
compiled in, so no plugin can answer for them.

`horoctl --version` and `--build-info` are described with
[`horo`'s](horo.md#version-and-build-information).
