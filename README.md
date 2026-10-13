# Horopter

**horopter** (/həˈrɒptər/, hor-**OP**-ter) — in binocular vision, the locus of
points that fall on corresponding retinal points in both eyes: where two
independent views agree and fuse into one.

One eye gives a flat image; two give depth. Metrics alone are flat. Metrics plus
change events give causal depth.

Horopter is a vendor-neutral store for change events. Deploys, rollouts, migrations and
incidents are written in by people, CI and cluster controllers, and read out by the tools you
already watch, so each change shows as a region on your graphs: when it started, when it
ended, and how.

## Status

<!-- x-release-please-start-version -->
The latest release is v0.13.0.
<!-- x-release-please-end -->
Until 1.0, a release that breaks the HTTP API, the storage schema or a command line bumps the
minor version; a patch release never does. What Horopter does not do yet is in
[Known limits](docs/reference/known-limits.md).

## Try It

[Try Horopter](https://docs.horopter.dev/tutorials/try-horopter/) runs it on your machine in a
few minutes: record a change, then see it in the terminal and on a Grafana graph.

## Install `horo`

`horo` records changes and reads them back. Download it from a
[release](https://github.com/horopter-dev/horopter/releases), as
`horo_<version>_<os>_<arch>.tar.gz`, or install it with Homebrew:

    brew install horopter-dev/tap/horo

or with Go:

    go install horopter.dev/horopter/cmd/horo@latest

## Documentation

- **[Concepts](https://docs.horopter.dev/concepts/)**: what a change is, recording one,
  credentials, attributes and read admission.
- **How-to**: record changes from
  [Flux](https://docs.horopter.dev/how-to/flux/) and from
  [GitHub Actions](https://github.com/horopter-dev/record-action); show them on
  [Grafana dashboards](https://docs.horopter.dev/how-to/grafana/);
  [install in Kubernetes](https://docs.horopter.dev/how-to/install-kubernetes/),
  [issue credentials](https://docs.horopter.dev/how-to/issue-credentials/) and
  [observe Horopter](https://docs.horopter.dev/how-to/observe-horopter/). All of them are on
  [docs.horopter.dev](https://docs.horopter.dev/how-to/).
- **Reference**: the [HTTP API](docs/reference/http-api.md),
  [configuration](docs/reference/configuration.md), [`horo`](docs/reference/horo.md),
  [`horoctl`](docs/reference/horoctl.md), [metrics](docs/reference/metrics.md) and
  [known limits](docs/reference/known-limits.md).

## Security

Report a vulnerability as the
[security policy](https://github.com/horopter-dev/horopter/security/policy) describes, not in a
public issue.

## Licence

Apache License 2.0 — `SPDX-License-Identifier: Apache-2.0`. [`LICENSE`](LICENSE)
holds the canonical text, unmodified.

The licence conveys the code and nothing else. Section 6 withholds trademark
rights explicitly: the name Horopter, the `horopter-dev` organisation and the
project's domains are not granted by it.

Contributions are accepted under the same licence, certified by a `Signed-off-by`
line per the Developer Certificate of Origin. There is no CLA. See
[`CONTRIBUTING.md`](CONTRIBUTING.md).
