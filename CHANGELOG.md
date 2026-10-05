# Changelog

## [0.4.0](https://github.com/horopter-dev/horopter/compare/v0.3.0...v0.4.0) (2026-10-05)


### Features

* **ingress:** accept Flux notifications at /v1/events/flux ([e11a206](https://github.com/horopter-dev/horopter/commit/e11a2068f347d5a8ecf286c4be974cbd4d48374f))
* **ingress:** accept Flux notifications at /v1/events/flux ([936104f](https://github.com/horopter-dev/horopter/commit/936104f9c2a3849dc4c23d102dad9b00db739cfa))
* **ingress:** map Flux events onto Horopter events ([f449d03](https://github.com/horopter-dev/horopter/commit/f449d03c1c929a2f71601b55548f9bd99620419c))


### Bug Fixes

* **ingress:** keep Flux keys apart per locus and record reverts ([64efd14](https://github.com/horopter-dev/horopter/commit/64efd146b183895a4dc3b3a3a9e5cb415649ee74))

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
