# Contributing to Horopter

Thanks for your interest. This document covers the one legal requirement — a
`Signed-off-by` line on every commit — and the conventions the repository
enforces automatically.

## Licence and sign-off

Horopter is licensed under the Apache License 2.0 (`SPDX-License-Identifier:
Apache-2.0`); see [`LICENSE`](LICENSE). Contributions are accepted under that
same licence.

There is no CLA. Instead, every commit must carry a `Signed-off-by` line
certifying the Developer Certificate of Origin, reproduced in full below:

    git commit -s -m "fix: stop the reader holding its permit across the write"

`-s` appends the line using your configured `user.name` and `user.email`, so
both must be set to a real name and a reachable address:

    Signed-off-by: Jane Doe <jane@example.com>

No configuration adds the line for you. `format.signOff` enables the flag for
`format-patch` rather than for `git commit`, and Git has no `commit.signOff` at
all: its own documentation says the trailer "should be a conscious act and means
that you certify you have the rights to submit this work under the same open
source license". If typing `-s` every time grates, an alias or a
`prepare-commit-msg` hook will add it — at the cost of moving that conscious act
to the moment you install the hook.

The [DCO app][dco-app] checks this on every pull request and fails one whose
commits are not all signed off. Bots and merge commits are excluded, so
Dependabot's pull requests are unaffected.

If you forgot, you have two ways out and the check's own output gives you the
text for the first. Add a **remediation commit** to the same pull request,
naming the commit it covers:

    git commit -s --allow-empty -m "I, Jane Doe <jane@example.com>, hereby add my Signed-off-by to this commit: <sha>"

The wording is matched exactly, and the remediation commit must be authored by
the same person as the commit it signs off for. This is the gentler option: it
adds a commit rather than rewriting ones someone may already have pulled.

Or rewrite the branch, which leaves cleaner history if nobody else has it:

    git rebase --signoff main

The two do not conflict: `--signoff` adds the trailer to every commit it
rewrites. A remediation commit you had already pushed survives the rebase — Git
keeps commits that started empty — where it is then redundant rather than
harmful. `git rebase -i` lets you drop it if you would rather it were gone.

### Developer Certificate of Origin 1.1

```
Developer Certificate of Origin
Version 1.1

Copyright (C) 2004, 2006 The Linux Foundation and its contributors.

Everyone is permitted to copy and distribute verbatim copies of this
license document, but changing it is not allowed.


Developer's Certificate of Origin 1.1

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I
    have the right to submit it under the open source license
    indicated in the file; or

(b) The contribution is based upon previous work that, to the best
    of my knowledge, is covered under an appropriate open source
    license and I have the right under that license to submit that
    work with modifications, whether created in whole or in part
    by me, under the same open source license (unless I am
    permitted to submit under a different license), as indicated
    in the file; or

(c) The contribution was provided directly to me by some other
    person who certified (a), (b) or (c) and I have not modified
    it.

(d) I understand and agree that this project and the contribution
    are public and that a record of the contribution (including all
    personal information I submit with it, including my sign-off) is
    maintained indefinitely and may be redistributed consistent with
    this project or the open source license(s) involved.
```

The licence conveys the code and nothing else. Section 6 withholds trademark
rights explicitly: the name Horopter and the project's domains are not granted
by it.

## Getting set up

You need Go (the version in `go.mod`), Docker, [go-task][task] and
[golangci-lint][lint]. Run `task` on its own to list every target.

    task db:up      # start the Postgres the tests need
    task check      # gofmt, golangci-lint and the full test suite

The test suite needs a database. `task test` sets `HOROPTER_TEST_DSN` for you
and refuses to run if Postgres is unreachable; running `go test` directly
without that variable makes the database tests **skip silently**, so prefer the
task target.

To run the stack from your working tree rather than published images:

    task up:local   # builds with goreleaser --snapshot
    task seed       # fills it with changes in all three shapes

`task up` pulls the tag `docker-compose.yaml` names instead; while the packages
are private, log in first:

    gh auth refresh -s read:packages
    gh auth token | docker login ghcr.io -u "$(gh api user --jq .login)" --password-stdin

`task stop` halts the stack and `task down` removes its containers, both
keeping the database; `task down:wipe` also **destroys the database volume**.

The stack publishes ingress on `localhost:8080`, egress on `localhost:8081`,
their metrics on `localhost:9464` and `localhost:9465`, and Postgres on
`localhost:5432` as user `horopter`, password `horopter`. Its `bootstrap`
service issues two fixed development keys, which are also `hack/seed.sh`'s
defaults:

    export HOROPTER_DEV_PRODUCER_KEY=horo_dead00000001_00000000000000000000000000000000000000000000000000000000000000
    export HOROPTER_DEV_READER_KEY=horo_dead00000002_11111111111111111111111111111111111111111111111111111111111111

Direct access to that Postgres bypasses every check the daemons make. Keep the
stack on loopback, and do not reuse its credentials.

[task]: https://taskfile.dev
[lint]: https://golangci-lint.run
[cc]: https://www.conventionalcommits.org/en/v1.0.0/
[dco-app]: https://github.com/apps/dco

## Before you open a pull request

Install the hooks once, so they run on every commit:

    prek install
    prek run --all-files

They are a **subset of CI, not a mirror of it**: gofmt, golangci-lint, the raced
test suite, and some whitespace and YAML checks. Passing them locally is
necessary, not sufficient. The test suite needs the Docker CLI with Compose 2.23.1
or later: `deploy/compose_test.go` renders the standalone compose file and checks
what `docker compose config` makes of it, with and without its `grafana` profile.

CI runs two jobs and checks considerably more. The first adds `go mod tidy
-diff`, a build, `go vet`, `actionlint` and `zizmor` over the workflows, and a
separate un-raced run of the read-expansion test. The second has no local
equivalent at all: `goreleaser check`, a snapshot release build, `shellcheck`
and `shfmt` over `hack/*.sh`. If you
are touching the release configuration or the shell scripts, expect CI to be the
first thing that tells you.

## Commit messages and pull requests

Commits follow [Conventional Commits][cc]. A `commit-msg` hook rejects a
non-conforming message once you have run `prek install` above. CI does not check
commit messages, so that hook is the only thing that will tell you:

    fix: log which migrations horopter-migrate applied
    feat(egress): return Retry-After when a read is shed
    chore: pin the dev compose stack to v0.1.0-dev.3

Keep the subject in the imperative mood and under 72 characters.

Pull requests are squash-merged; it is the only merge button. The squash commit
takes the **pull request's title** as its subject, so that title is what lands on
`main`, and it is the only conventional-commit line Release Please reads. The
types on your branch's own commits are not read. Give the title a conventional
type and write it as the changelog entry it will become. Nothing checks the title,
so check it yourself before merging.

The type matters beyond tidiness — it determines the next version number. `fix:`
selects a patch bump, `feat:` a minor, and a `!` or a `BREAKING CHANGE:` footer a
major (a minor while the project is pre-1.0). Choose it deliberately.

Because each pull request becomes one changelog entry under one type, keep pull
requests small and open them often:

- **One user-visible change per pull request.** A change that is both a fix and a
  feature is two pull requests; squashed together, one of them disappears from
  the changelog, and a `feat:` under a `fix:` title ships as a patch.
- **Open it when that change is ready**, rather than carrying the next change on
  the same branch. Review fixes for the change belong in its pull request; they
  squash away.

Releases are cut by merging the release pull request. On every push to `main`,
Release Please keeps one pull request open that bumps the version by those rules and
adds the changes to `CHANGELOG.md`. Merging it tags the release, and the release
workflow builds and publishes it. Nobody picks a version number or pushes a tag by
hand.

## Tests

Test behaviour, not implementation: a refactor that breaks a test without
breaking the code means the test was wrong.

Cover the error paths, not only the happy one — empty inputs, boundaries,
malformed data, and every error branch the code handles. Mock only what is slow,
non-deterministic, or outside the project; the database is none of those here,
which is why the suite uses a real Postgres.

Tests declare their environment rather than inheriting it. A test that needs a
variable sets it explicitly, so a branch that falls back to the environment
cannot be satisfied by whatever happens to be in your shell.

When you fix a bug, write the test first and watch it fail. A regression test
that has never been red proves nothing.

## Amending an unreleased migration

`applied_migration` tracks migrations by filename, not content, so editing an
already-applied file leaves `Migrate` reporting the schema current when it is
not. Drop what the old version created and delete its row from
`applied_migration` before a test run will pick up the edit. This only
applies to a migration that is unmerged and unreleased — once released, write
a new migration instead.

## Reporting security issues

Please do not open a public issue for a security problem. Check
[Known limits](docs/reference/known-limits.md) and
[Credentials and Capabilities](https://docs.horopter.dev/concepts/credentials-and-capabilities/)
first — a caveat already documented there is not a new finding; anything beyond
what is documented, raise privately with the maintainer first.
