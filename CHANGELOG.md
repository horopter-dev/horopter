# Changelog

## [0.3.0](https://github.com/horopter-dev/horopter/compare/v0.2.0...v0.3.0) (2026-10-05)


### ⚠ BREAKING CHANGES

* **egress:** both routes' bodies change from a bare array to {"changes": [...]} and {"events": [...]}, to make room for paging.
* derive the event source from the credential

### Features

* **client:** post events with bounded retries ([587999d](https://github.com/horopter-dev/horopter/commit/587999da51f251ccdef2cf48dc41fe029c48cccc))
* **client:** read changes and events, following next ([9c0368d](https://github.com/horopter-dev/horopter/commit/9c0368d7d6ba79f9ff274ef4d36b6b8de1acae21))
* derive the event source from the credential ([311b650](https://github.com/horopter-dev/horopter/commit/311b650e821e7c01c41f6539c2fd778b07635050))
* **egress:** add an opaque, query-bound cursor token ([6cf0746](https://github.com/horopter-dev/horopter/commit/6cf07466bfea00335c6608d9448694cd952afbfc))
* **egress:** answer /v1/changes and /v1/events with an envelope ([417ac48](https://github.com/horopter-dev/horopter/commit/417ac486319fbb699ddd313ef64277cc740332e8))
* **egress:** log every read with its query ([d52140f](https://github.com/horopter-dev/horopter/commit/d52140fcc5fcee574312d6915ff44d81d8614e49))
* **egress:** page /v1/changes and /v1/events through next ([a9fdc8a](https://github.com/horopter-dev/horopter/commit/a9fdc8aa2a09aa494adb2b7e5469eb6ebe3cbe5d))
* **horo:** explore changes and events from the command line ([c595783](https://github.com/horopter-dev/horopter/commit/c59578350ae8997f9d54ca767ec70fec16301df8))
* **horo:** re-fold one change with horo refold ([e32c6ad](https://github.com/horopter-dev/horopter/commit/e32c6adf78136e59b858d267097788250ca629b4))
* **horo:** record changes with horo run and horo event ([848f9d6](https://github.com/horopter-dev/horopter/commit/848f9d6bbb6fe905e00a372febc750e854d4a809))
* **horo:** record one event ([1edd727](https://github.com/horopter-dev/horopter/commit/1edd727d2050529699dd843c1c65a987babec77d))
* **horo:** run a command and record its start and outcome ([6e1f2a4](https://github.com/horopter-dev/horopter/commit/6e1f2a40832656e4070b3d6ff801dc03ced20108))
* **ingress:** log every request with its event's identifiers ([45849d0](https://github.com/horopter-dev/horopter/commit/45849d0e9032b061f06ab62c7e34082c462eeebf))
* **ingress:** re-fold one ref at POST /v1/admin/refold ([3c602ba](https://github.com/horopter-dev/horopter/commit/3c602ba3b42bd07a5821ced595ad51682bfca1d2))
* log every request on both daemons ([3a645e9](https://github.com/horopter-dev/horopter/commit/3a645e99faacecf61511122087ab7c96c4993495))
* **logging:** annotate request lines, read errors, cut long fields ([ff00427](https://github.com/horopter-dev/horopter/commit/ff00427ad3bccd0198ad3f2eb1a717c6f73c2c17))
* **logging:** write one line per request ([4a43c63](https://github.com/horopter-dev/horopter/commit/4a43c63ec93bd13158923168bdae68ed2341c75f))
* re-fold one ref with POST /v1/admin/refold and horo refold ([7073a62](https://github.com/horopter-dev/horopter/commit/7073a627933ca978ab919c65c5ce91f9c0a7b4f8))
* **store:** carry id in the ref index for the per-ref re-fold ([c00f641](https://github.com/horopter-dev/horopter/commit/c00f6419b76ed0537c7236905f6cb99fa11ecbf6))
* **store:** converge a ref to id order on an out-of-order append ([9985f96](https://github.com/horopter-dev/horopter/commit/9985f96b6517143a3053961c4b2bc0b5fb104269))
* **store:** converge the projection to an id-ordered fold ([a3d62a1](https://github.com/horopter-dev/horopter/commit/a3d62a1dcb5723161e5312a6e58ca31de4c3511b))
* **store:** count re-folds and warn when one moves an outcome ([9ffa40b](https://github.com/horopter-dev/horopter/commit/9ffa40bb0d6f9868ea0311f2f90b25a4206bf403))
* **store:** re-fold one ref from the log ([0107050](https://github.com/horopter-dev/horopter/commit/0107050da2fb730519813a97f5b99b65f82182dc))
* **store:** report what a re-fold read and repaired ([9108be4](https://github.com/horopter-dev/horopter/commit/9108be4c3e13f97d1b6658ab9d5e1abcfb00b6b4))


### Bug Fixes

* **horo:** describe run's --type as the bare change type ([d8fe94b](https://github.com/horopter-dev/horopter/commit/d8fe94b7606faa7091670d2b9ee7fcc4f11eb3f4))
* **horo:** exit 3 only for a ref with no events; log the ref on every outcome ([c832b36](https://github.com/horopter-dev/horopter/commit/c832b3622846b40cc1d5cd100c952fa2b4dbd771))
* **horo:** keep signals ignored that horo inherited as ignored ([cf2cc6d](https://github.com/horopter-dev/horopter/commit/cf2cc6d305ff32f5cc0dcc75334860150cc707a6))
* **horo:** quote the close command by allowlist ([780ad96](https://github.com/horopter-dev/horopter/commit/780ad9636529fb84c2961dfcf7c848cb33807990))
* **horo:** show each change once, and neutralise control characters ([3be2437](https://github.com/horopter-dev/horopter/commit/3be24377a90acb349318fc1d1e5166ed7fdeac9a))
* **horo:** survive hangup and quit, and say when --strict skipped the command ([4e1de29](https://github.com/horopter-dev/horopter/commit/4e1de293f6d6b1ab986b6f195aedd20fd717b4b7))
* **ingress:** use monotonic ULID entropy in the test fixture ([faacfee](https://github.com/horopter-dev/horopter/commit/faacfeeb0dd3009a97a194ce1425f681a5544227))
* **logging:** name the original on a replay, no phantom refs, keep long fields ([f87dd1a](https://github.com/horopter-dev/horopter/commit/f87dd1a4d2ff7d4999c530bfaf4355731ec57c9a))
* **store:** correct refold's End precision and altered label ([d30288b](https://github.com/horopter-dev/horopter/commit/d30288b478bae7bb25049932dcdd39c5d2a0d06d))
* **store:** correct refold's redundant read, drift, and labels ([b7577a3](https://github.com/horopter-dev/horopter/commit/b7577a3ec53c1fce736bc1be463d4ea8f0191488))
* **store:** correct stale fixtures and docs review found on the refold ([1d6a402](https://github.com/horopter-dev/horopter/commit/1d6a402e3855130d4d2e18ab754434622d17bcd7))


### Code Refactoring

* **otlp:** delete the dead span-name fallback, as for Grafana ([f489ca7](https://github.com/horopter-dev/horopter/commit/f489ca73c54df75e78cb34217bb8ef6c236fdf76))
* **store:** fold through one query builder and one scan loop ([635821e](https://github.com/horopter-dev/horopter/commit/635821e11fd5a9805a4b1f37d79b2cddeb41adba))
