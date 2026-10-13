---
title: Known Limits
description: What Horopter does not do yet, what happens when you meet it, and what to do instead.
---

Each limit says what happens and what to do. A release that removes a limit removes its entry.

## Reads Assume a WAF in Front of Egress

Read admission protects egress's database connections and memory; it is not protection against
a flood of requests. Put a WAF, or an equivalent, in front of egress.

- **A read larger than the memory pool takes the whole pool.** It is cut to the oldest rows that
  fit, and while it runs every other read on that replica is shed with `503`. One client
  repeating such a read keeps them shed, at a volume no WAF would notice. Raise
  `HOROPTER_EGRESS_READ_MEMORY_BYTES`, or narrow the reads: a shorter window, or a filter.
- **Slow clients hold read capacity** until their response is written. Run egress behind a
  buffering reverse proxy.

[Read Admission](https://docs.horopter.dev/concepts/read-admission/) explains the gates.

## No Admission Control on Writes

Ingress has no rate limit, per-tenant quota or load shedding, so a burst of appends is not shed
gracefully, and one busy producer or tenant slows every other on that ingress. An append the
store cannot take is answered with a retryable `503`.

## Rebuilding a Tenant Halts Its Ingest

`--rebuild-tenant` holds the tenant's lock for its whole run, so appends to that tenant fail
with a retryable `503` until it finishes. It is not batched and reports no progress. Run it when
the tenant's writes are quiet, and prefer re-folding one change when you can name it:
[Rebuild a Tenant's Projection](https://docs.horopter.dev/how-to/rebuild-a-tenants-projection/).

## The Last Terminal Event Wins

A change's status is the outcome of the last event that ended it, by event id. When two
reporters end one change, one's success can clear the other's failure. `horopter-flux` also
records each Kustomization failure as a point of its own, so a failure stays visible after a
later success.

## No Management API

Tenants and credentials are managed with [`horoctl`](horoctl.md), against the database, and no
other way.

## The GitHub Action

[`horopter-dev/record-action`](https://github.com/horopter-dev/record-action#limits):

- It records `run:` commands. A step that `uses:` another action cannot be wrapped.
- Each re-run attempt is its own change, so a failure and its retry are two refs.
- It runs on Linux x64 and arm64, and macOS arm64; there is no Windows or Intel macOS build.
- With an organisation secret, the credential attests only `github/<org>`; the repository is
  named only by the ref.

## The Flux Client

[Record Flux Commits and Releases](https://docs.horopter.dev/how-to/flux/):

- **Git sources only.** Only a GitRepository's artifact starts a commit change.
- **No link from a commit to the releases it caused.**
- **Kubernetes Events expire**, after about an hour by default. Changeset detail and Event-only
  points from a longer outage are lost; changes are recovered from status.
- **An outage loses intermediate states.** A rollout that finished while the client was down has
  an approximate start; of several, only the newest is recorded. A Kustomization that failed and
  then succeeded at one revision during the outage records only the success.
- **A reinstall after an uninstall remediation is not recorded**, as Helm reinstalls at v1 under
  the same HelmRelease. Its `Uninstall*` point is.
- **A Kustomization that health-checks a HelmRelease** records a stuck release as a failure of
  every later commit it applies. Leave the release's health to the release change.

## The Docs Describe the Latest Release Only

These pages, and the rest of the documentation, describe the latest release. Upgrade notes are
in each release's notes.
