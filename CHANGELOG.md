# Changelog

## [0.8.0](https://github.com/horopter-dev/horopter/compare/v0.7.1...v0.8.0) (2026-10-08)


### ⚠ BREAKING CHANGES

* **egress:** both routes' bodies change from a bare array to {"changes": [...]} and {"events": [...]}, to make room for paging.
* derive the event source from the credential
* issue credentials in the compose stack
* authenticate egress and fetch a change by ref
* derive the ingress tenant from the credential

### Features

* accept actor on the CloudEvents ingest path ([e16baff](https://github.com/horopter-dev/horopter/commit/e16baff2f9236c92cd07384854fafbd0f9a8c5e6))
* accept POST for the Grafana annotations endpoint ([8208a1a](https://github.com/horopter-dev/horopter/commit/8208a1a5703e2af494d1951ad4055eaf4a890caf))
* add a rebuild CLI and bound every projection lock wait ([72bc14a](https://github.com/horopter-dev/horopter/commit/72bc14a967ff060bdc2debdda8dce44ecec51bd9))
* add Change projection type with point, open and closed shapes ([b687b39](https://github.com/horopter-dev/horopter/commit/b687b390384ec8ba7d3769f766124b71e6621f5d))
* add core Event type with validation and ULID generation ([aa5192d](https://github.com/horopter-dev/horopter/commit/aa5192dc5d764e865b40fec61104d279f3f1a15d))
* add credential key format and ref validation ([7a4decd](https://github.com/horopter-dev/horopter/commit/7a4decd25890e3870a40a8dc184aa5b5da520fc7))
* add egress daemon serving queries and vendor adapters ([e018ac2](https://github.com/horopter-dev/horopter/commit/e018ac258cb488e903fc72c148642b115b88a3d8))
* add horoctl with compiled-in verbs and PATH fallthrough ([81bb02a](https://github.com/horopter-dev/horopter/commit/81bb02a6fb1396217c837957032deb5449ffb2cc))
* add HTTP server spans and the tenant attribute ([67d7bef](https://github.com/horopter-dev/horopter/commit/67d7bef273c5e6b6c08dabb5ae166296012abc03))
* add identity tables and backfill tenants and refs ([5cb4a30](https://github.com/horopter-dev/horopter/commit/5cb4a301afecd5301060fdfe62d25755d84f8c47))
* add ingress daemon with native and CloudEvents intake ([7f8ab40](https://github.com/horopter-dev/horopter/commit/7f8ab4094e7dd08c07f847c7bb2710cdcf22b39b))
* add migrate binary and startup schema-version guard ([94bf8a5](https://github.com/horopter-dev/horopter/commit/94bf8a5c3858ed885135735e6b79da1433544bbf))
* add OTLP export adapter proving the render boundary ([a7ac669](https://github.com/horopter-dev/horopter/commit/a7ac669516d6bdd1cf08741b1217a8fbf1d5fbae))
* add Postgres store with advisory-locked migrations ([3726970](https://github.com/horopter-dev/horopter/commit/3726970e6307eb7b2845edf9e985af60dc3d7aa6))
* add Prometheus and OTLP metrics ([cb932f1](https://github.com/horopter-dev/horopter/commit/cb932f1b64d7acd13b31a55db4dad73557e864d1))
* add pure Fold projecting events onto changes ([b36bb90](https://github.com/horopter-dev/horopter/commit/b36bb905b167857386c96560411e9c00a6e169de))
* add render boundary and Grafana JSON datasource adapter ([bf1e355](https://github.com/horopter-dev/horopter/commit/bf1e35504a57216b1d1bafa731c2b8fcf1d88472))
* add store and SQL spans, and rebuild attributes ([ac84a8a](https://github.com/horopter-dev/horopter/commit/ac84a8a4af5e0ce41691e78a3cc6ca36781e2b29))
* add TenantID and Origin domain types ([8cde55d](https://github.com/horopter-dev/horopter/commit/8cde55d3e068362a998cfdaf776eb8a0df3d5958))
* add the telemetry package's environment configuration ([6b01417](https://github.com/horopter-dev/horopter/commit/6b01417de31012e306e8e4fbdfc071e72835a34a))
* **admission:** add a byte pool for read memory reservations ([6f9cdf8](https://github.com/horopter-dev/horopter/commit/6f9cdf85dee3f9d0bc7148db083585e7d2b94787))
* **admission:** add an AIMD read concurrency limiter ([975e4d6](https://github.com/horopter-dev/horopter/commit/975e4d64b0176e1f7027c77fdc28d6c1ff7c1c24))
* append events idempotently with a row-locked projection fold ([2d81f58](https://github.com/horopter-dev/horopter/commit/2d81f5877ef623f04fc68ec345f150f9297d540c))
* authenticate egress and fetch a change by ref ([2294d5b](https://github.com/horopter-dev/horopter/commit/2294d5ba691ceea3b4bcfffec2b5e5c7583788fb))
* build images with GoReleaser and ko ([a148afc](https://github.com/horopter-dev/horopter/commit/a148afce9ec4b05cd22d53200d40e78dc170ef04))
* cap event attributes and admit egress reads by capacity ([6693bad](https://github.com/horopter-dev/horopter/commit/6693bad86be504df128d6caca6577ec6f0988e0d))
* carry source onto the change, and derive its locus ([1b1b340](https://github.com/horopter-dev/horopter/commit/1b1b340e85e277ef449d1504411b297f80ab9199))
* carry the appending credential's locus on the event ([3eb441b](https://github.com/horopter-dev/horopter/commit/3eb441be6010d3c8d9cf2f9d326eee580e1572e6))
* **client:** post events with bounded retries ([587999d](https://github.com/horopter-dev/horopter/commit/587999da51f251ccdef2cf48dc41fe029c48cccc))
* **client:** read changes and events, following next ([9c0368d](https://github.com/horopter-dev/horopter/commit/9c0368d7d6ba79f9ff274ef4d36b6b8de1acae21))
* containerise ingress, egress and migrate ([56c0ffe](https://github.com/horopter-dev/horopter/commit/56c0ffeb37d412edb58211a8ea6eb54647df3e47))
* **core:** a change is titled by its start ([#64](https://github.com/horopter-dev/horopter/issues/64)) ([cedfd24](https://github.com/horopter-dev/horopter/commit/cedfd2421653016f409353fc1957038034fa1059))
* **core:** let an event assert its change's lifecycle ([0db9e5c](https://github.com/horopter-dev/horopter/commit/0db9e5c752a8f84e5cb95a179f050936549010ac))
* **core:** measure an event's attributes against a byte limit ([1dab41e](https://github.com/horopter-dev/horopter/commit/1dab41e2feefea02b0234597960e5f146684329e))
* count event appends by outcome ([be2e91d](https://github.com/horopter-dev/horopter/commit/be2e91dbe5435d1687cfba84d6a904797a89c2e7))
* derive the event source from the credential ([311b650](https://github.com/horopter-dev/horopter/commit/311b650e821e7c01c41f6539c2fd778b07635050))
* derive the ingress tenant from the credential ([9c901fc](https://github.com/horopter-dev/horopter/commit/9c901fc00e40f51098875a5beae232fc18b1a5a3))
* drive load at every egress rendering ([e720246](https://github.com/horopter-dev/horopter/commit/e7202467f20fe5c9cbc0c61ac82fc6074b611b8f))
* **egress:** add an opaque, query-bound cursor token ([6cf0746](https://github.com/horopter-dev/horopter/commit/6cf07466bfea00335c6608d9448694cd952afbfc))
* **egress:** add the memory gate, cutting or refusing oversized reads ([320a435](https://github.com/horopter-dev/horopter/commit/320a435adce5c82b7d52a1c43cc199597e58caf4))
* **egress:** answer /v1/changes and /v1/events with an envelope ([417ac48](https://github.com/horopter-dev/horopter/commit/417ac486319fbb699ddd313ef64277cc740332e8))
* **egress:** halve the read limit when reads wait for a connection ([6fa6d25](https://github.com/horopter-dev/horopter/commit/6fa6d25b93bcc44805637f2ca0822b523bd16143))
* **egress:** log every read with its query ([d52140f](https://github.com/horopter-dev/horopter/commit/d52140fcc5fcee574312d6915ff44d81d8614e49))
* **egress:** match subject= as a glob against every subject a change touched ([#57](https://github.com/horopter-dev/horopter/issues/57)) ([ec810ea](https://github.com/horopter-dev/horopter/commit/ec810ea457fc68fa276379d7c6522cc7eec100f9))
* **egress:** page /v1/changes and /v1/events through next ([a9fdc8a](https://github.com/horopter-dev/horopter/commit/a9fdc8aa2a09aa494adb2b7e5469eb6ebe3cbe5d))
* **egress:** read admission limits from the environment ([6338ea7](https://github.com/horopter-dev/horopter/commit/6338ea75f0b5b19ca3f9f234d59cdeeb9f5129fb))
* **egress:** render lifecycle on GET /v1/events ([1474bd2](https://github.com/horopter-dev/horopter/commit/1474bd22f966540037d24c529c8c7365c5ed2ba1))
* **egress:** render locus on the audit route ([2414e1a](https://github.com/horopter-dev/horopter/commit/2414e1a2a7720ec50ac69b890d57160df3c59618))
* **egress:** report read admission through metrics ([f13bee8](https://github.com/horopter-dev/horopter/commit/f13bee8d86be990db255c83f26364aa7681c8db0))
* **egress:** shed reads over an adaptive concurrency limit ([7c8c8cf](https://github.com/horopter-dev/horopter/commit/7c8c8cf9f54c2cb90308c83bd1d8d3b27f39d923))
* export connection pool statistics under a fixed pool name ([ff3130c](https://github.com/horopter-dev/horopter/commit/ff3130c2b56400b8f993a5e4eca0ee8d52de1db6))
* filter changes by locus, source and allowlisted attributes ([7c73efd](https://github.com/horopter-dev/horopter/commit/7c73efd5065ce54875669de697704c5a80731bf5))
* filter changes by locus, source and attribute over HTTP ([30d58cb](https://github.com/horopter-dev/horopter/commit/30d58cb7dc3c54d622b44dd5b98904500b41d45e))
* filter changes by locus, sources and attributes ([f490e76](https://github.com/horopter-dev/horopter/commit/f490e76b687229f9c6e6fd7694ca4aca292b4bdc))
* filter tracing by instrumentation area ([8ebe390](https://github.com/horopter-dev/horopter/commit/8ebe39034b7fb8cfb780cf98cb0f8f7ca5cfe6f8))
* **horo:** explore changes and events from the command line ([c595783](https://github.com/horopter-dev/horopter/commit/c59578350ae8997f9d54ca767ec70fec16301df8))
* **horo:** re-fold one change with horo refold ([e32c6ad](https://github.com/horopter-dev/horopter/commit/e32c6adf78136e59b858d267097788250ca629b4))
* **horo:** record changes with horo run and horo event ([848f9d6](https://github.com/horopter-dev/horopter/commit/848f9d6bbb6fe905e00a372febc750e854d4a809))
* **horo:** record one event ([1edd727](https://github.com/horopter-dev/horopter/commit/1edd727d2050529699dd843c1c65a987babec77d))
* **horo:** run a command and record its start and outcome ([6e1f2a4](https://github.com/horopter-dev/horopter/commit/6e1f2a40832656e4070b3d6ff801dc03ced20108))
* **ingress:** accept an explicit lifecycle assertion ([e6825a5](https://github.com/horopter-dev/horopter/commit/e6825a52e1398075d5dc8cdf290b87165b5d1a2b))
* **ingress:** accept Flux notifications at /v1/events/flux ([e11a206](https://github.com/horopter-dev/horopter/commit/e11a2068f347d5a8ecf286c4be974cbd4d48374f))
* **ingress:** accept Flux notifications at /v1/events/flux ([936104f](https://github.com/horopter-dev/horopter/commit/936104f9c2a3849dc4c23d102dad9b00db739cfa))
* **ingress:** cap event attributes, configurable, default 4 KiB ([6134ffb](https://github.com/horopter-dev/horopter/commit/6134ffb0caa5b5133a6ceb1c0a30e54ad3f5492d))
* **ingress:** keep Flux event metadata as attributes ([#68](https://github.com/horopter-dev/horopter/issues/68)) ([cc001d9](https://github.com/horopter-dev/horopter/commit/cc001d92fed6fc1a02c2d2088830785422e57319))
* **ingress:** log every request with its event's identifiers ([45849d0](https://github.com/horopter-dev/horopter/commit/45849d0e9032b061f06ab62c7e34082c462eeebf))
* **ingress:** map Flux events onto Horopter events ([f449d03](https://github.com/horopter-dev/horopter/commit/f449d03c1c929a2f71601b55548f9bd99620419c))
* **ingress:** re-fold one ref at POST /v1/admin/refold ([3c602ba](https://github.com/horopter-dev/horopter/commit/3c602ba3b42bd07a5821ced595ad51682bfca1d2))
* **ingress:** record the objects each Flux apply changed ([#54](https://github.com/horopter-dev/horopter/issues/54)) ([0387417](https://github.com/horopter-dev/horopter/commit/0387417a0e70fc5d8d02c9699cb7c02e9622551b))
* **ingress:** refuse a correlation_ref prefixed with its own locus ([b00494a](https://github.com/horopter-dev/horopter/commit/b00494a149cd7390d461b4513dd28e11721285d9))
* **ingress:** return the handle the request takes ([fd0c1d6](https://github.com/horopter-dev/horopter/commit/fd0c1d69c8efd0fc8f7f6744219c88dc7f93df7d))
* **ingress:** title a Flux rollout by its commit subject ([#65](https://github.com/horopter-dev/horopter/issues/65)) ([04ab911](https://github.com/horopter-dev/horopter/commit/04ab911ecad1fb88a2edaa29ff92e9fe3fe2c338))
* install a meter provider with a Prometheus reader ([b60e055](https://github.com/horopter-dev/horopter/commit/b60e055a076eedd229ccf59c3f085017e49b3253))
* install the JSON log handler in every binary ([8a6311f](https://github.com/horopter-dev/horopter/commit/8a6311faf2a53b2bcf69c9f07c18c0e979593f35))
* issue credentials in the compose stack ([5d3af3c](https://github.com/horopter-dev/horopter/commit/5d3af3c16281a3f636b0bb7bb92233e83707a2fb))
* log every request on both daemons ([3a645e9](https://github.com/horopter-dev/horopter/commit/3a645e99faacecf61511122087ab7c96c4993495))
* **logging:** annotate request lines, read errors, cut long fields ([ff00427](https://github.com/horopter-dev/horopter/commit/ff00427ad3bccd0198ad3f2eb1a717c6f73c2c17))
* **logging:** log JSON with trace ids at a configurable level ([bea26b8](https://github.com/horopter-dev/horopter/commit/bea26b897b8a3feebd4777ff452b562104f2a659))
* **logging:** write one line per request ([4a43c63](https://github.com/horopter-dev/horopter/commit/4a43c63ec93bd13158923168bdae68ed2341c75f))
* make &lt;locus&gt;/&lt;ref&gt; the only ref shape (migration 0006) ([e9d5bcf](https://github.com/horopter-dev/horopter/commit/e9d5bcf872356a99c7ecc59276a3cd159d89e033))
* open a change, and return the handle the request takes ([283e844](https://github.com/horopter-dev/horopter/commit/283e844ea260da75d9bdef0531582f032c045c20))
* publish images and a release on a tag ([83404dd](https://github.com/horopter-dev/horopter/commit/83404dd47cf72098f9ea8082cf18f1c2f482c409))
* query changes by interval overlap rather than start containment ([9ed684f](https://github.com/horopter-dev/horopter/commit/9ed684f38223dab2ebfebdeb2e60b26aed07938f))
* re-fold one ref with POST /v1/admin/refold and horo refold ([7073a62](https://github.com/horopter-dev/horopter/commit/7073a627933ca978ab919c65c5ce91f9c0a7b4f8))
* rebuild the projection by replaying the event log in ULID order ([568992b](https://github.com/horopter-dev/horopter/commit/568992b5367ef7ae10c33fd763a4c3f8993978b2))
* record the append path's outcome and error kind on its span ([f526ac9](https://github.com/horopter-dev/horopter/commit/f526ac9b3008b5637f7cd4ff1a181b9adc0a8349))
* record the read path's shape and tenant source ([1673928](https://github.com/horopter-dev/horopter/commit/167392863d0fc8452d63715f4f65b46ce90b7ecb))
* refuse a correlation_ref prefixed with its own locus ([d6b15d9](https://github.com/horopter-dev/horopter/commit/d6b15d9c76f6b271d37b2694686ba39f263c8bc5))
* render locus on the audit route ([86e9d64](https://github.com/horopter-dev/horopter/commit/86e9d64b89d6ec626f6075a02947512a3c63c383))
* resolve credentials to principals behind a TTL cache ([c8bbda5](https://github.com/horopter-dev/horopter/commit/c8bbda5ac8ad244745f31540f2919e14eca62162))
* resolve telemetry configuration per signal ([330ba65](https://github.com/horopter-dev/horopter/commit/330ba659c4653ed5ad08647bbf6c3c7be70de0dc))
* run the compose stack from published images ([d27cfc2](https://github.com/horopter-dev/horopter/commit/d27cfc262d15e149625c0e8249293e34db47c1e7))
* seed a stack with changes in all three shapes ([b8a0030](https://github.com/horopter-dev/horopter/commit/b8a00304d16c98d675c529d04eb46aec6a29df9a))
* serve Prometheus metrics from both daemons ([c31cfd2](https://github.com/horopter-dev/horopter/commit/c31cfd290c267d17ac03aa7f4948ea2b49c914df))
* set up and flush telemetry in all three binaries ([f8d415f](https://github.com/horopter-dev/horopter/commit/f8d415f334f3e4224a44a5b22c279d56fa56525b))
* split a change's start into an assertion and an inference ([8b2dc0d](https://github.com/horopter-dev/horopter/commit/8b2dc0dcba2fd677421cff622a6b8ddd443d15e9))
* store the locus on the event and prefix point refs ([9fc39a8](https://github.com/horopter-dev/horopter/commit/9fc39a8a633e9ff73215d77e9511efe203b6dd2d))
* **store:** carry id in the ref index for the per-ref re-fold ([c00f641](https://github.com/horopter-dev/horopter/commit/c00f6419b76ed0537c7236905f6cb99fa11ecbf6))
* **store:** converge a ref to id order on an out-of-order append ([9985f96](https://github.com/horopter-dev/horopter/commit/9985f96b6517143a3053961c4b2bc0b5fb104269))
* **store:** converge the projection to an id-ordered fold ([a3d62a1](https://github.com/horopter-dev/horopter/commit/a3d62a1dcb5723161e5312a6e58ca31de4c3511b))
* **store:** count re-folds and warn when one moves an outcome ([9ffa40b](https://github.com/horopter-dev/horopter/commit/9ffa40bb0d6f9868ea0311f2f90b25a4206bf403))
* **store:** estimate a read's row text and cut reads at a byte budget ([b8993c7](https://github.com/horopter-dev/horopter/commit/b8993c74f73bdae44f92f867c053f4b5835d05e4))
* **store:** persist the lifecycle assertion ([a298dd4](https://github.com/horopter-dev/horopter/commit/a298dd43f1947f1c174504f5cd2ae5901693a498))
* **store:** re-fold one ref from the log ([0107050](https://github.com/horopter-dev/horopter/commit/0107050da2fb730519813a97f5b99b65f82182dc))
* **store:** refuse events whose attributes exceed a configured cap ([7a1ccc2](https://github.com/horopter-dev/horopter/commit/7a1ccc21a92d0272e1d9ac3a7758b85a9c1a0299))
* **store:** report pool connection waits to an observer on the context ([0f103ef](https://github.com/horopter-dev/horopter/commit/0f103ef74f305c0f54cf1ff503e694fcc6509f45))
* **store:** report what a re-fold read and repaired ([9108be4](https://github.com/horopter-dev/horopter/commit/9108be4c3e13f97d1b6658ab9d5e1abcfb00b6b4))


### Bug Fixes

* answer 503 when the credential store is unreachable ([651b01c](https://github.com/horopter-dev/horopter/commit/651b01c1d9cfa6a96374eb906da25611d0270866))
* answer transient append failures 503, not 400 ([19a4345](https://github.com/horopter-dev/horopter/commit/19a4345f73a688efcb608e9e53492393182c7c9c))
* bound the key prefix and collapse concurrent lookups ([f1b2e6f](https://github.com/horopter-dev/horopter/commit/f1b2e6f0f5cef4f841e165bdcf6827f28289f0e9))
* bound the shared credential lookup with its own deadline ([00f9b94](https://github.com/horopter-dev/horopter/commit/00f9b94d71a937139964642988ad3d954eb27220))
* build snapshot images for the host architecture ([644e909](https://github.com/horopter-dev/horopter/commit/644e9090c8a55ffdef390316cbd3e89f689db4d7))
* build snapshot images for the host architecture ([1bf21ea](https://github.com/horopter-dev/horopter/commit/1bf21ea6446234b8a9766f22b39fb93f7ea02b6c))
* cap egress result sets, generalize the no-write-route guard ([0909485](https://github.com/horopter-dev/horopter/commit/0909485ba66907fefbcfe05b7d980912e8d920eb))
* cite mergedAttributes, not Render, for the horopter.* precedent ([f97be77](https://github.com/horopter-dev/horopter/commit/f97be7756dca6a39e737a7f7aa4b798d4977dfa7))
* close round-1 review findings on batch B ([45722c6](https://github.com/horopter-dev/horopter/commit/45722c6a172a0a62d7313599511d34535a0fc8a2))
* close the metrics listener in Flush rather than relying on the server ([c6e4187](https://github.com/horopter-dev/horopter/commit/c6e4187f93958c57a5504345b1b7197031f83468))
* close the metrics listener in Flush rather than relying on the server ([65f836b](https://github.com/horopter-dev/horopter/commit/65f836b1d14546f83a54f3d353317413a1b50e32))
* close the review findings on egress auth ([36dc2b1](https://github.com/horopter-dev/horopter/commit/36dc2b15a6a57edfd687e1f65ca6ed2571310074))
* close the tenant leak, stream the replay, and fix task test args ([cf7c666](https://github.com/horopter-dev/horopter/commit/cf7c666b4a3c23e18e810b85cd3745d86bb1b63b))
* close the whole-branch review's composition findings ([9e06166](https://github.com/horopter-dev/horopter/commit/9e061666389db218ae21401fbb00b65d82309e5c))
* correct epoch constants and Task 6 compile order in the plan ([4564cf5](https://github.com/horopter-dev/horopter/commit/4564cf57ee0a7931537b380752bca24242dae920))
* correct the store's two understated comments and bound the whole append ([63d2bbc](https://github.com/horopter-dev/horopter/commit/63d2bbcdf5fac60125b05c2a57550e34384640de))
* derive the recorded schema version from the ledger ([8f3749c](https://github.com/horopter-dev/horopter/commit/8f3749c9e440725d35b0d05ff308845fdae7ee11))
* drop the schema_version wire field, scan origin/edited on duplicates ([0815a8a](https://github.com/horopter-dev/horopter/commit/0815a8a9f27fc3466bb1ed2b5d0dc3319f023dd1))
* **egress:** give the expansion test a pool the gate cannot cut ([b8bfe50](https://github.com/horopter-dev/horopter/commit/b8bfe50c76ddbf291a5cba5879c80b51f6a14856))
* **egress:** give the read-too-large 422 a remedy that works ([#71](https://github.com/horopter-dev/horopter/issues/71)) ([bd8fa83](https://github.com/horopter-dev/horopter/commit/bd8fa838f6f6920578704af9e094c82e36a97736))
* **egress:** guard permit release and fail fast in holdRead ([030a373](https://github.com/horopter-dev/horopter/commit/030a3739c2cbf4e5626506c25b72b6490c18abcd))
* **egress:** mutation-proof the connection-wait guards ([6a51305](https://github.com/horopter-dev/horopter/commit/6a5130530dc4b235f2e3c245670eb4ea30d59bbe))
* **egress:** mutation-proof the memory gate on both read routes ([667892f](https://github.com/horopter-dev/horopter/commit/667892fb1a2728d28acfd751a4246131660b62e7))
* **egress:** pin metric shapes and isolate tests from otel delegation ([d173cd7](https://github.com/horopter-dev/horopter/commit/d173cd7101afc2a6636f5269bdc4df65417b3252))
* **egress:** refuse subject patterns too costly for Postgres to compile ([#58](https://github.com/horopter-dev/horopter/issues/58)) ([b83d34c](https://github.com/horopter-dev/horopter/commit/b83d34cd5bf4c77f0fb539613da30ec87d2dc21f))
* **egress:** size reads by the columns they return ([#61](https://github.com/horopter-dev/horopter/issues/61)) ([acf7781](https://github.com/horopter-dev/horopter/commit/acf778107255c96812aee63adcde76c9b6d7f43c))
* emit real OTLP/JSON from the trace-span adapter ([a22b8ad](https://github.com/horopter-dev/horopter/commit/a22b8ad6680da062d6f2751c9ee072e23d7c4137))
* enforce that event ids are well-formed ULIDs ([af13f8d](https://github.com/horopter-dev/horopter/commit/af13f8d586b96e90fb14096503bcafc4e10046a8))
* escape the release tag, validate the deploy compose file ([6208113](https://github.com/horopter-dev/horopter/commit/6208113df0b033d4222b76a517f330f418d4004a))
* exclude build artifacts and dev tooling from the docker context ([63bac28](https://github.com/horopter-dev/horopter/commit/63bac282ad8738e6396e4f4bb9c86479bab1306c))
* harden Fold purity coverage and close two spec gaps ([f7604e6](https://github.com/horopter-dev/horopter/commit/f7604e630c9de605d3d7ea167a3e7b5b4bf843a3))
* harden telemetry setup, flushing and the endpoint gate ([585034e](https://github.com/horopter-dev/horopter/commit/585034e8557c019bd342f68b6793d6d336c302de))
* **horoctl:** reject subject patterns on non-append roles, reserve legacy locus, guard issue writes ([b2de71f](https://github.com/horopter-dev/horopter/commit/b2de71fb2593b3f38af2673bb8895b932c99b29a))
* **horoctl:** serialize bootstrap against concurrent runs ([d89ebbb](https://github.com/horopter-dev/horopter/commit/d89ebbbb0a035bbe7c611c7370dbb85e2601358b))
* **horoctl:** stop echoing a malformed --key into its rejection error ([5668b10](https://github.com/horopter-dev/horopter/commit/5668b109e21d983dfe93ab08400a7cbb83a0e2c0))
* **horo:** describe run's --type as the bare change type ([d8fe94b](https://github.com/horopter-dev/horopter/commit/d8fe94b7606faa7091670d2b9ee7fcc4f11eb3f4))
* **horo:** exit 3 only for a ref with no events; log the ref on every outcome ([c832b36](https://github.com/horopter-dev/horopter/commit/c832b3622846b40cc1d5cd100c952fa2b4dbd771))
* **horo:** keep signals ignored that horo inherited as ignored ([cf2cc6d](https://github.com/horopter-dev/horopter/commit/cf2cc6d305ff32f5cc0dcc75334860150cc707a6))
* **horo:** quote the close command by allowlist ([780ad96](https://github.com/horopter-dev/horopter/commit/780ad9636529fb84c2961dfcf7c848cb33807990))
* **horo:** show each change once, and neutralise control characters ([3be2437](https://github.com/horopter-dev/horopter/commit/3be24377a90acb349318fc1d1e5166ed7fdeac9a))
* **horo:** survive hangup and quit, and say when --strict skipped the command ([4e1de29](https://github.com/horopter-dev/horopter/commit/4e1de293f6d6b1ab986b6f195aedd20fd717b4b7))
* **ingress:** keep Flux keys apart per locus and record reverts ([64efd14](https://github.com/horopter-dev/horopter/commit/64efd146b183895a4dc3b3a3a9e5cb415649ee74))
* **ingress:** use monotonic ULID entropy in the test fixture ([faacfee](https://github.com/horopter-dev/horopter/commit/faacfeeb0dd3009a97a194ce1425f681a5544227))
* inline DSN check in horopter-migrate, remove unnecessary helper ([7969266](https://github.com/horopter-dev/horopter/commit/7969266e9f00bdca133b00a9bc4ea96d855cd1c6))
* key CloudEvents idempotency on source+id and signal truncation ([0892d2d](https://github.com/horopter-dev/horopter/commit/0892d2dec3380005d05d803b96a9ffb1531bb037))
* log applied migrations, and stop tests leaving rows behind ([731b876](https://github.com/horopter-dev/horopter/commit/731b876d56d755b29e80e5d228e4e3a79f131c0c))
* log failed egress reads and stop relaying store errors ([5bcdc36](https://github.com/horopter-dev/horopter/commit/5bcdc36c54a578b2ce6d4a7594f6d0b4f7bd39b5))
* log failed reads and withhold internal error text from callers ([4c479b4](https://github.com/horopter-dev/horopter/commit/4c479b417e7df3b654c266f1384b0024ba36fc6f))
* log which migrations horopter-migrate applied ([5ed1ee1](https://github.com/horopter-dev/horopter/commit/5ed1ee18f38bdcb7283583fdec06ee37d8dfa737))
* **logging:** name the original on a replay, no phantom refs, keep long fields ([f87dd1a](https://github.com/horopter-dev/horopter/commit/f87dd1a4d2ff7d4999c530bfaf4355731ec57c9a))
* make flushTelemetry's comment true of horopter-migrate ([9641ec3](https://github.com/horopter-dev/horopter/commit/9641ec333e63507b8ddea26e59e6b0b596cd86cf))
* make the pre-commit test hook exercise the whole suite ([7597c0a](https://github.com/horopter-dev/horopter/commit/7597c0afe3aa2f58ef88242385384600e72ffd70))
* mark semver prerelease tags as GitHub prereleases ([85c93f7](https://github.com/horopter-dev/horopter/commit/85c93f7d968e9e2a72197787da9dd13fa377a62e))
* match a point change's events by its prefixed ref ([ff2a57a](https://github.com/horopter-dev/horopter/commit/ff2a57a82e02c7133dcdb6c3fcbf7ae9492b0d3e))
* model change.sources as a commutative union ([ff964f3](https://github.com/horopter-dev/horopter/commit/ff964f3febb074b410a6a41b6bf6050535475f73))
* move tests to core_test package and remove redundant comments ([3535c9d](https://github.com/horopter-dev/horopter/commit/3535c9d1fa8350ad481c83af3e6f07154372d96b))
* name the database volume and close the plan's round-2 defects ([4f7d86b](https://github.com/horopter-dev/horopter/commit/4f7d86ba4ffd7b3eee68db74bcc2d59d9b847d15))
* namespace the Grafana annotation body's description key ([dd2cb84](https://github.com/horopter-dev/horopter/commit/dd2cb84e2510108f8b795cde4e7ed7c46fd2baa9))
* overlay computed Grafana tags over colliding attribute keys ([e79ce0a](https://github.com/horopter-dev/horopter/commit/e79ce0a992d2cb518666e6c97fda1c920f1e16f2))
* point CI's migration step at horoctl-migrate ([4400a83](https://github.com/horopter-dev/horopter/commit/4400a83d5a675f79a992203758ae902441b06256))
* preserve delegated exit codes and reject unusable --key prefixes ([8aacb1c](https://github.com/horopter-dev/horopter/commit/8aacb1cb3bd9e470dc85761bc2ba48f907038585))
* record store span attributes on failure paths ([698ca8b](https://github.com/horopter-dev/horopter/commit/698ca8ba65da1871cb437dd41a186ed588fa1aaf))
* record the adapter before the query, not after it ([956d5e7](https://github.com/horopter-dev/horopter/commit/956d5e7bf0ae74ade04132b444c6f109a6662d2e))
* record the append span from the submitted event, not the zeroed one ([154db2a](https://github.com/horopter-dev/horopter/commit/154db2a51cccb7b1089e865847b48e3dc1ff3cd6))
* reject an invalid query window at egress's parsers ([9b13352](https://github.com/horopter-dev/horopter/commit/9b13352b8467a48187c897723141db48d7c498cd))
* render seed.sh's commit attribute as a hex ref ([b4f624b](https://github.com/horopter-dev/horopter/commit/b4f624b38fdcbb831271512fef95a46878005036))
* repair the tenant-span test Task 6 broke, and close review gaps ([14918da](https://github.com/horopter-dev/horopter/commit/14918dae69be6bee09f25676542756e71ac4c8f3))
* resolve the tenant after the permit, not before ([82195c0](https://github.com/horopter-dev/horopter/commit/82195c05131200144f7efff8cb95d3a5d8bb99cf))
* restore DSN validation and reduce complexity with helper functions ([d8d6fd2](https://github.com/horopter-dev/horopter/commit/d8d6fd2a65c8b1d7a23c9c1d8580c126046b5f91))
* restore the plugin name the delegation test stages ([a9233ac](https://github.com/horopter-dev/horopter/commit/a9233ac80f339a4d5f4baea082b667a810890629))
* restore upsertChange comment antecedent, pin locus value ([50699db](https://github.com/horopter-dev/horopter/commit/50699db6e8b9ea9f42b83883a993ba946846bca0))
* route the SDK's internal logger to slog as well ([c5eaf5e](https://github.com/horopter-dev/horopter/commit/c5eaf5e002d44caec0ba006f9323c15143e9b4b8))
* satisfy shellcheck on the CI workflow's run blocks ([20c6ec0](https://github.com/horopter-dev/horopter/commit/20c6ec05a6a4a39759448eebcaa76daa360f435d))
* scope Grafana annotations to a path tenant and cap the body ([4f58a5b](https://github.com/horopter-dev/horopter/commit/4f58a5b86f089f152187d3f30355f7caa342cbb8))
* serialise projection folds and close the store's correctness gaps ([c0aff21](https://github.com/horopter-dev/horopter/commit/c0aff21624013ba67a4e2c6e39215eb9c06c8669))
* shorten the go-fmt hook past the 100-char line limit ([2a362d0](https://github.com/horopter-dev/horopter/commit/2a362d0ccaf01146db9193bc93fc731afcc7753b))
* stop /readyz returning database connection detail ([e38d0c2](https://github.com/horopter-dev/horopter/commit/e38d0c2db147adf4300be35fac5ab9eb6fb6aa06))
* stop a titleless event replacing a recorded title with its type ([aee3b1d](https://github.com/horopter-dev/horopter/commit/aee3b1d3d19adb16b209b30c686deb514d339ac7))
* stop an actorless event erasing a change's recorded actor ([5720361](https://github.com/horopter-dev/horopter/commit/572036142ca72a6668a0f320ef7dfb5e66131d63))
* stop double-counting failures, drop bogus events window ([f6d4850](https://github.com/horopter-dev/horopter/commit/f6d48507f784ca29b8074f9c0334edfc2b61aa24))
* stop the OTLP adapter's comments overclaiming spec support ([ff77501](https://github.com/horopter-dev/horopter/commit/ff775015edabba7e3926af0146d44271a348bfa0))
* stop version_test.go from bricking the shared database ([372bc02](https://github.com/horopter-dev/horopter/commit/372bc023121a9117ee930539a026ce76a1480050))
* **store:** correct refold's End precision and altered label ([d30288b](https://github.com/horopter-dev/horopter/commit/d30288b478bae7bb25049932dcdd39c5d2a0d06d))
* **store:** correct refold's redundant read, drift, and labels ([b7577a3](https://github.com/horopter-dev/horopter/commit/b7577a3ec53c1fce736bc1be463d4ea8f0191488))
* **store:** correct stale fixtures and docs review found on the refold ([1d6a402](https://github.com/horopter-dev/horopter/commit/1d6a402e3855130d4d2e18ab754434622d17bcd7))
* **telemetry:** close the metrics listener and install metrics first ([1f587b9](https://github.com/horopter-dev/horopter/commit/1f587b91e116839c922a52c4b984d974cf8080af))
* **telemetry:** keep the request host out of HTTP exemplars ([06b6a67](https://github.com/horopter-dev/horopter/commit/06b6a67847291b69ffc644f5f555b76d54385e45))
* **telemetry:** keep the request host out of HTTP exemplars ([ffa39fc](https://github.com/horopter-dev/horopter/commit/ffa39fc7b1ae411acbe66a909ced0b754df87c3f)), closes [#50](https://github.com/horopter-dev/horopter/issues/50)
* use transactional advisory lock and isolate the migration race test ([ada32a7](https://github.com/horopter-dev/horopter/commit/ada32a7665424a63cd8ed32cab4dd4fdbedce83a))
* validate CloudEvents before mapping, prove healthz needs no store ([dfc09f5](https://github.com/horopter-dev/horopter/commit/dfc09f5482ea9a77c073b17403d0ddef08244dc0))
* validate ref before prefixing, wrap CloudEvents ref errors, harden tests ([392e065](https://github.com/horopter-dev/horopter/commit/392e06564c1ef82ee5387e7e1ddb9e6de40661fe))
* wrap long error messages to stay within 100-char line limit ([31a5d8c](https://github.com/horopter-dev/horopter/commit/31a5d8c6b4ebc8832863064206c1ed1a123e4c87))


### Code Refactoring

* close the change-filters branch review's fix-wave findings ([731741e](https://github.com/horopter-dev/horopter/commit/731741e6b7aebf521bde2c9b985a15d8537b4e1f))
* close the ingress safety gaps and strengthen two weak tests ([f96dbee](https://github.com/horopter-dev/horopter/commit/f96dbee35c07c8a4e006287a62e2df1fed626e9c))
* **core:** add Ref and Locus value objects ([7e86809](https://github.com/horopter-dev/horopter/commit/7e868092b2a560a04354fe55faa41e853092e767))
* **core:** extract composeRef as the encoding's one statement ([1d81df0](https://github.com/horopter-dev/horopter/commit/1d81df072a8e0e333b661202f5d03cb912121dba))
* delete the unused IsPoint predicate ([35665f9](https://github.com/horopter-dev/horopter/commit/35665f9ab78346c970786baa5b854d0033f5aa0d))
* enforce complexity, length and line-length limits mechanically ([f61f324](https://github.com/horopter-dev/horopter/commit/f61f32445d58b67bd865dff2d1526260b9e0d2bd))
* **horoctl:** pass the issue flags as a named struct ([46184bc](https://github.com/horopter-dev/horopter/commit/46184bcb276238aa1d8b97c29c5088f045c0c5ae))
* **horoctl:** pass the issue flags as a named struct ([626ec4c](https://github.com/horopter-dev/horopter/commit/626ec4c8e59579502524709d5e2da9841e00db58))
* make auth.ValidateLocus extend core rather than restate it ([05be7c6](https://github.com/horopter-dev/horopter/commit/05be7c692de34acc2ca906682ebbfa526ef0949b))
* model Ref and Locus as value objects ([#44](https://github.com/horopter-dev/horopter/issues/44)) ([2c30d2a](https://github.com/horopter-dev/horopter/commit/2c30d2a8a339be9c74f2d5c690fe66d4eafb1174))
* **otlp:** delete the dead span-name fallback, as for Grafana ([f489ca7](https://github.com/horopter-dev/horopter/commit/f489ca73c54df75e78cb34217bb8ef6c236fdf76))
* rename horoctl-migrate back to horopter-migrate ([a250a06](https://github.com/horopter-dev/horopter/commit/a250a06028b19be4973907a3d9178b5a6e95698d))
* rename horoctl-migrate back to horopter-migrate ([c350fea](https://github.com/horopter-dev/horopter/commit/c350fea467412e53304ab316abe42aa5dfa91b0d))
* render an end through Change.IsOpen rather than re-deriving it ([aab54ab](https://github.com/horopter-dev/horopter/commit/aab54abe7734ec9de8550ee2cab46417c08b7bb7))
* share the postgres connect path and satisfy golangci-lint ([6c945a7](https://github.com/horopter-dev/horopter/commit/6c945a711d638691cdfa5908db4ebd9aa0f0412b))
* split postgres.go along its four responsibilities ([45a1ef0](https://github.com/horopter-dev/horopter/commit/45a1ef0bdc108b71dbfc39234e66d298265e3e1b))
* split postgres.go along its four responsibilities ([de79891](https://github.com/horopter-dev/horopter/commit/de7989100d3816b0cb4e639098602709cb8f1ed0))
* **store:** fold through one query builder and one scan loop ([635821e](https://github.com/horopter-dev/horopter/commit/635821e11fd5a9805a4b1f37d79b2cddeb41adba))
* **store:** keep each change's subjects and when each first appeared ([#55](https://github.com/horopter-dev/horopter/issues/55)) ([0e3309f](https://github.com/horopter-dev/horopter/commit/0e3309f5638fca35796c7a3c55e643eb72d63ca1))
* type Change.Ref, Event.Locus and Event.CorrelationRef ([41d803f](https://github.com/horopter-dev/horopter/commit/41d803fc3642b6f95c46e2bb852b2cc3a876066d))

## [0.7.1](https://github.com/horopter-dev/horopter/compare/v0.7.0...v0.7.1) (2026-10-08)


### Bug Fixes

* **egress:** give the read-too-large 422 a remedy that works ([#71](https://github.com/horopter-dev/horopter/issues/71)) ([bd8fa83](https://github.com/horopter-dev/horopter/commit/bd8fa838f6f6920578704af9e094c82e36a97736))

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
