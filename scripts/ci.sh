#!/usr/bin/env bash
#
# The same checks .github/workflows/ci.yml runs, but locally.
#
# This exists because CI is not optional for this project: ADR-016 makes a
# green build the thing that lets an autonomous agent open a pull request
# safely. While GitHub Actions is unavailable (see the open constraint in
# ADR-016), this script IS the gate. Run it before every push.
#
#   scripts/ci.sh
#
# Requires a Postgres to test against. If TEST_DATABASE_URL is unset it will
# use the local compose one.

set -euo pipefail

cd "$(dirname "$0")/.."

: "${TEST_DATABASE_URL:=postgres://worldzero:worldzero_dev@127.0.0.1:5433/worldzero?sslmode=disable}"
export TEST_DATABASE_URL

step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }

step "gofmt"
unformatted=$(gofmt -l ./cmd ./internal ./migrations)
if [ -n "$unformatted" ]; then
    echo "These files need gofmt:"
    echo "$unformatted"
    exit 1
fi
echo "clean"

step "go vet"
go vet ./...

step "build"
go build ./...

step "test"
# -race needs cgo and a C toolchain, which a Windows dev box usually lacks.
# CI runs it on Linux; locally we degrade rather than skip testing entirely.
if go env CGO_ENABLED | grep -q 1 && command -v gcc >/dev/null 2>&1; then
    go test -race -count=1 -timeout 10m ./...
else
    echo "note: no C toolchain, running without -race (CI covers it)"
    go test -count=1 -timeout 10m ./...
fi

step "migrations are reversible"
if command -v psql >/dev/null 2>&1; then
    base="${TEST_DATABASE_URL%%\?*}"
    migrate_url="${base%/*}/worldzero_migrate?sslmode=disable"
    psql "$TEST_DATABASE_URL" -c 'CREATE DATABASE worldzero_migrate' >/dev/null 2>&1 || true
    MIGRATE_TEST_DATABASE_URL="$migrate_url" go test -count=1 -run TestMigrationsAreReversible ./internal/kernel/db/
else
    echo "skipped: psql not on PATH (CI covers it)"
fi

printf '\n\033[32mall checks passed\033[0m\n'
