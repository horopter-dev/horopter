# Changelog

## [0.5.0](https://github.com/horopter-dev/horopter/compare/v0.4.1...v0.5.0) (2026-10-06)


### Features

* **egress:** match subject= as a glob against every subject a change touched ([#57](https://github.com/horopter-dev/horopter/issues/57)) ([ec810ea](https://github.com/horopter-dev/horopter/commit/ec810ea457fc68fa276379d7c6522cc7eec100f9))
* **ingress:** record the objects each Flux apply changed ([#54](https://github.com/horopter-dev/horopter/issues/54)) ([0387417](https://github.com/horopter-dev/horopter/commit/0387417a0e70fc5d8d02c9699cb7c02e9622551b))


### Code Refactoring

* **store:** keep each change's subjects and when each first appeared ([#55](https://github.com/horopter-dev/horopter/issues/55)) ([0e3309f](https://github.com/horopter-dev/horopter/commit/0e3309f5638fca35796c7a3c55e643eb72d63ca1))

## [0.4.1](https://github.com/horopter-dev/horopter/compare/v0.4.0...v0.4.1) (2026-10-06)

### Bug Fixes

* **telemetry:** HTTP metric exemplars no longer carry the request's `Host`. A caller using a
  long host name, such as Flux calling ingress by its full service name, pushed an exemplar
  past Prometheus's 128-rune label limit, so the exemplar was dropped and every scrape logged
  `exemplar labels have … runes, exceeding the limit of 128`. Exemplars now carry only their
  trace and span IDs. ([#51](https://github.com/horopter-dev/horopter/pull/51), closes
  [#50](https://github.com/horopter-dev/horopter/issues/50))

## [0.4.0](https://github.com/horopter-dev/horopter/compare/v0.3.0...v0.4.0) (2026-10-05)

### Features

* **ingress:** `POST /v1/events/flux` takes Flux notification-controller's `generic` provider
  payload and records one change per Git revision: `NewArtifact` starts it, and each
  Kustomization's first success or failure at that revision ends it. A Kustomization failure is
  also recorded as a point of its own, and HelmRelease actions are recorded as points. Anything
  else is answered `200` with the reason it was ignored, which Flux counts as delivered. See the
  README's "Flux" section. ([#47](https://github.com/horopter-dev/horopter/pull/47))

## [0.3.0](https://github.com/horopter-dev/horopter/compare/v0.2.0...v0.3.0) (2026-10-05)


### ⚠ BREAKING CHANGES

* **ingress:** an event's `source` now comes from the credential that posted it, and a request body carrying `source` is refused with `400`. Producers must stop sending it. ([#37](https://github.com/horopter-dev/horopter/pull/37))
* **egress:** `GET /v1/changes` and `GET /v1/events` answer an envelope, `{"changes": [...], "next": "..."}` and `{"events": [...], "next": "..."}`, instead of a bare array. Readers must take the rows from the named field. ([#39](https://github.com/horopter-dev/horopter/pull/39))

### Features

* **horo:** a command-line tool. `horo changes` and `horo show` explore recorded changes and events ([#39](https://github.com/horopter-dev/horopter/pull/39)). `horo run` wraps a command and records its start and outcome, and never blocks the command unless `--strict`. `horo event` records one event ([#40](https://github.com/horopter-dev/horopter/pull/40)). `horo refold` repairs one change ([#41](https://github.com/horopter-dev/horopter/pull/41)).
* **egress:** reads page. A cut result carries `next`, also sent as a `Link` header, and following it returns every row ([#39](https://github.com/horopter-dev/horopter/pull/39)).
* **ingress:** `POST /v1/admin/refold` re-folds one change from its event log while ingest keeps running. It needs a key with the `admin` capability ([#41](https://github.com/horopter-dev/horopter/pull/41)).
* **store:** a change no longer depends on the order its events arrive in. An out-of-order append re-folds its ref, so the live projection equals a rebuild's. `horopter_refold_total` counts re-folds, and a warning names a re-fold that moved a recorded outcome ([#34](https://github.com/horopter-dev/horopter/pull/34)).
* **logging:** both daemons write one log line per request, carrying its route, status, duration, credential, tenant and the event's or query's identifiers ([#38](https://github.com/horopter-dev/horopter/pull/38)).

### Upgrading

* **Migration `0009`** rebuilds an index on `event` under an exclusive lock, so appends wait while it runs ([#34](https://github.com/horopter-dev/horopter/pull/34)).
* **Re-folding is not retroactive.** A change that drifted before this version keeps its drift until `horopter-migrate --rebuild-tenant` or `horo refold` repairs it ([#34](https://github.com/horopter-dev/horopter/pull/34)).
