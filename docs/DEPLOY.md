# Deployment

Nothing is deployed. Per [ADR-017](DECISIONS.md), WorldZero is developed local-first until
the project earns a server and a domain; the seven-day soak is an M5 activity.

`deploy/compose.yaml` is the local stack — see the [README](../README.md) quickstart.

## Why this file is short

A survey of a candidate host was done on 2026-08-22 and is kept in `local/DEPLOY.md`, which
is gitignored. It stays out of the repository on purpose: it records a public address, a
root login, a full inventory of the production services running on that machine, and its
firewall rules. That is a reconnaissance document for a live box, and this repository is
intended to be public one day. Convenience is not worth publishing it.

The general lesson, which is safe to record here: **the eventual host is a shared machine
under memory pressure**, so whatever runs there carries hard `mem_limit`s, builds its images
in CI rather than on the box, and never runs the bot fleet alongside `worldd`. When
deployment becomes real, read `local/DEPLOY.md` and re-survey — a shared box drifts.

## Rules that will apply whenever deployment happens

1. Never take ports 80/443 on a shared host, and never add a second reverse proxy.
2. WorldZero owns its own Postgres instance. The `events` table needs its own roles and its
   own `UPDATE`/`DELETE` denials; an append-only log sharing an instance with unrelated
   production is not defensible.
3. Every container gets a `mem_limit`. No exceptions.
4. Never build images on the box.
5. Never run the bot fleet on the box — colocating bots with `worldd` makes the soak measure
   the wrong thing.
6. Back up off-box from day one. The event log is irreplaceable, and losing it is the only
   unrecoverable failure this project has.
7. Do a restore drill. A backup nobody has restored is not a backup.
