#!/bin/bash
# Nightly database dump to the bucket. Run by ai-env-backup.timer as root:
# pg_dump inside the database container, gzip, verify, upload with the
# console's credentials, delete the local copy. The dump goes to _backup/
# and is pruned after 30 days; it never touches the command objects.
set -euo pipefail
CONTAINER="${AIENV_PG_CONTAINER:-aienv-pg15}"
DB="${AIENV_PG_DB:-aienv}"
ENVFILE="${AIENV_CONSOLE_ENV:-/etc/ai-env-mgr/console.env}"
TMP="$(mktemp -t aienv-backup.XXXXXX.sql.gz)"
trap 'rm -f "$TMP"' EXIT
chmod 600 "$TMP"
docker exec "$CONTAINER" pg_dump -U postgres --no-owner "$DB" | gzip -6 > "$TMP"
gzip -t "$TMP"
set -a; . "$ENVFILE"; set +a
/usr/local/bin/ai-env-admin backup -file "$TMP" -keep-days "${AIENV_BACKUP_KEEP_DAYS:-30}"
