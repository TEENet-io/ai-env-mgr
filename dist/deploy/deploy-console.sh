#!/bin/bash
# Deploy the console from this checkout: build, migrate, swap, check, and
# put the previous build back if the new one does not come up healthy.
#
#   sudo dist/deploy/deploy-console.sh            # deploy what is checked out
#   sudo dist/deploy/deploy-console.sh --dirty    # allow uncommitted changes
#
# Order matters. Migrations run first, as the tables' owner: the console's
# own role (aienv_app) cannot alter tables, and a console that finds a
# migration pending refuses to start -- so swapping the binary before
# migrating is an outage (it happened on 2026-09-24). Nothing is replaced
# until the migrations have succeeded. When there is something to migrate,
# the database is dumped locally first.
#
# A failed health check restores the previous binaries. It does not undo
# migrations: they are written to be compatible with the build before them,
# and reversing one is a decision for a person (ai-env-migrate down).
set -euo pipefail

SERVICE="${AIENV_SERVICE:-ai-env-mgr-console}"
BIN_DIR="${AIENV_BIN_DIR:-/usr/local/bin}"
CONSOLE_ENV="${AIENV_CONSOLE_ENV:-/etc/ai-env-mgr/console.env}"
# AIENVMGR_DB_DSN for the owner of the tables (postgres), mode 600.
MIGRATE_ENV="${AIENV_MIGRATE_ENV:-/etc/ai-env-mgr/migrate.env}"
HEALTH_URL="${AIENV_HEALTH_URL:-http://127.0.0.1:8090/healthz}"
PG_CONTAINER="${AIENV_PG_CONTAINER:-aienv-pg15}"
DUMP_DIR="${AIENV_DUMP_DIR:-/var/backups/ai-env-mgr}"

say() { printf '==> %s\n' "$*"; }
die() { printf 'deploy: %s\n' "$*" >&2; exit 1; }

allow_dirty=0
[ "${1:-}" = "--dirty" ] && allow_dirty=1

[ "$(id -u)" -eq 0 ] || die "run as root (it replaces binaries and restarts $SERVICE)"
[ -r "$CONSOLE_ENV" ] || die "no $CONSOLE_ENV"
[ -r "$MIGRATE_ENV" ] || die "no $MIGRATE_ENV: create it (mode 600) with AIENVMGR_DB_DSN for the table owner"
[ "$(stat -c %a "$MIGRATE_ENV")" = "600" ] || die "$MIGRATE_ENV must be mode 600"

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT/go"
if [ -n "$(git status --porcelain)" ] && [ "$allow_dirty" -ne 1 ]; then
  die "uncommitted changes in $ROOT; commit them or pass --dirty"
fi
VERSION="$(git describe --tags --always --dirty)"

WORK="$(mktemp -d -t aienv-deploy.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

say "building $VERSION"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$WORK/ai-env-admin" ./cmd/admin
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$WORK/ai-env-migrate" ./cmd/migrate

# The new migrate binary knows the new migrations; run it as the owner.
migrate() { ( set -a; . "$MIGRATE_ENV"; set +a; "$WORK/ai-env-migrate" "$@" ); }
pending="$(migrate status | grep -c ' pending' || true)"
if [ "$pending" -gt 0 ]; then
  say "$pending migration(s) pending; dumping the database first"
  install -d -m 700 "$DUMP_DIR"
  dump="$DUMP_DIR/aienv-predeploy-$(date -u +%Y%m%dT%H%M%SZ).sql.gz"
  ( umask 077; docker exec "$PG_CONTAINER" pg_dump -U postgres --no-owner aienv | gzip -6 > "$dump" )
  gzip -t "$dump"
  say "dump: $dump ($(stat -c %s "$dump") bytes)"
  say "migrating"
  migrate up || die "migration failed; nothing was replaced, $SERVICE still runs the old build"
else
  say "schema up to date"
fi

say "installing (previous build kept as .prev)"
for b in ai-env-admin ai-env-migrate; do
  [ -e "$BIN_DIR/$b" ] && cp -p "$BIN_DIR/$b" "$BIN_DIR/$b.prev"
  install -m 755 "$WORK/$b" "$BIN_DIR/$b"
done
systemctl restart "$SERVICE"

healthy() {
  for _ in $(seq 1 30); do
    if systemctl is-active --quiet "$SERVICE" && [ "$(curl -s -o /dev/null -m 3 -w '%{http_code}' "$HEALTH_URL")" = "200" ]; then
      return 0
    fi
    sleep 1
  done
  return 1
}

if healthy; then
  say "deployed $VERSION; $SERVICE healthy"
  exit 0
fi

say "$SERVICE did not come up healthy; restoring the previous build"
journalctl -u "$SERVICE" --since "-2min" --no-pager | tail -20 >&2 || true
for b in ai-env-admin ai-env-migrate; do
  [ -e "$BIN_DIR/$b.prev" ] && install -m 755 "$BIN_DIR/$b.prev" "$BIN_DIR/$b"
done
systemctl restart "$SERVICE"
if healthy; then
  die "rolled back to the previous build (healthy). Migrations, if any, were NOT reversed."
fi
die "the previous build is not healthy either; look at: journalctl -u $SERVICE"
