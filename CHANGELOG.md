# Changelog

## [0.7.0](https://github.com/horopter-dev/horopter/compare/v0.6.0...v0.7.0) (2026-10-06)


### Features

* **ingress:** keep Flux event metadata as attributes ([#68](https://github.com/horopter-dev/horopter/issues/68)) ([cc001d9](https://github.com/horopter-dev/horopter/commit/cc001d92fed6fc1a02c2d2088830785422e57319))

## [0.6.0](https://github.com/horopter-dev/horopter/compare/v0.5.2...v0.6.0) (2026-10-06)

### Features

* **core:** a change is titled by its **start**: the title of its earliest start event that has
  one, instead of its last event's. The start says what changed; later events say how it went,
  and keep their titles on themselves. Without a titled start the old rule stands (last
  non-empty title, else the first event's type). **Behaviour change:** a change whose start
  and outcome both carry titles now shows the start's — e.g. "Deploy v2.5.0" rather than
  "Done". ([#64](https://github.com/horopter-dev/horopter/pull/64))
* **ingress:** a Flux rollout is titled by its commit: the subject source-controller's
  `NewArtifact` quotes (truncated by Flux to about 50 characters), so a Grafana annotation of
  the rollout reads as the commit rather than `Reconciliation finished in …`.
  ([#65](https://github.com/horopter-dev/horopter/pull/65))

### Upgrading

* Schema version 11: `horopter-migrate` adds `change.title_started_at` and **retitles existing
  changes** that have a titled start event by the same rule, so a rebuild afterwards changes
  nothing. Existing Flux rollouts take their `NewArtifact` title as it was stored, in the
  `stored artifact for commit '…'` form; rollouts after the upgrade show the bare subject.

## [0.5.2](https://github.com/horopter-dev/horopter/compare/v0.5.1...v0.5.2) (2026-10-06)


### Bug Fixes

* **egress:** size reads by the columns they return ([#61](https://github.com/horopter-dev/horopter/issues/61)) ([acf7781](https://github.com/horopter-dev/horopter/commit/acf778107255c96812aee63adcde76c9b6d7f43c))

## [0.5.1](https://github.com/horopter-dev/horopter/compare/v0.5.0...v0.5.1) (2026-10-06)


### Bug Fixes

* **egress:** refuse subject patterns too costly for Postgres to compile ([#58](https://github.com/horopter-dev/horopter/issues/58)) ([b83d34c](https://github.com/horopter-dev/horopter/commit/b83d34cd5bf4c77f0fb539613da30ec87d2dc21f))

## [0.5.0](https://github.com/horopter-dev/horopter/compare/v0.4.1...v0.5.0) (2026-10-06)

### Features

* **egress:** `subject` is now a pattern, matched against **every** subject a change touched up
  to its end rather than only its last event's. It uses a credential's `--subject-pattern`
  syntax (`path.Match`: `*` within one `/` segment, `?`, `[...]`) plus `{a,b}` alternation;
  repeat `subject` to OR several. `subject=*/lastlight/*` finds every change that touched the
  `lastlight` namespace. A malformed pattern, invalid UTF-8 or a reversed range such as `[9-0]`
  is answered `400`. **Behaviour change:** a plain `subject=X` now also matches a change in which
  any earlier event had subject `X`. ([#57](https://github.com/horopter-dev/horopter/pull/57))
* **ingress:** the Flux route records kustomize-controller's `Progressing` changeset, one event
  per changed object (`Deployment/lastlight/cloudflared configured`) on the rollout's change, so a
  change records what a commit changed. **Behaviour change:** these events' subjects are the
  applied objects, so a Flux credential issued with `--subject-pattern` is now answered `403` on
  every changeset; issue the Flux credential unscoped.
  ([#54](https://github.com/horopter-dev/horopter/pull/54))

### Upgrading

* Schema version 10: `horopter-migrate` adds `change.subject_first_seen` and backfills it from
  the event log. ([#55](https://github.com/horopter-dev/horopter/pull/55))

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
