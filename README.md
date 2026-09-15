# Horopter

**horopter** (/həˈrɒptər/, hor-**OP**-ter) — in binocular vision, the locus of
points that fall on corresponding retinal points in both eyes: where two
independent views agree and fuse into one.

One eye gives a flat image; two give depth. Metrics alone are flat. Metrics plus
change events give causal depth.

A vendor-neutral store for change events. Humans and machines write change
events in; observability tools read them out to enrich graphs.

## Status

v0.1 in development. See `docs/superpowers/specs/` for the design.

## Quick start

    task up
    curl -X POST localhost:8080/v1/events \
      -H 'Authorization: Bearer local-dev-token' -H 'Content-Type: application/json' \
      -d '{"source":"ci","type":"deployment.started","subject":"acme/prod",
           "occurred_at":"2026-09-14T10:00:00Z","correlation_ref":"dep-1"}'
    curl "localhost:8081/adapters/grafana/annotations?from=1789376400000&to=1789383600000"
    task down

`task up` builds and starts the full stack with `docker compose`: Postgres, then
`horopter-migrate` (which must run to completion before the daemons start — it is the
only binary permitted to apply schema DDL), then `horopter-ingress` on port 8080 and
`horopter-egress` on port 8081. `task down` stops the stack and removes the database
volume.

Each service is configured with environment variables:

- `DATABASE_URL` — PostgreSQL connection string, required by all three binaries.
- `HOROPTER_INGRESS_TOKEN` — bearer token `horopter-ingress` requires on writes; it
  refuses to start without one.
- `HOROPTER_ADDR` — listen address for `horopter-ingress` and `horopter-egress`
  (defaults to `0.0.0.0:8080` and `0.0.0.0:8081` respectively).

## Licence

TBD
