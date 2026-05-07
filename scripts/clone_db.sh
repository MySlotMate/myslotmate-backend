#!/usr/bin/env bash
# Clone the entire `public` schema (DDL + data) from the source DB
# (read from .env -> DATABASE_URL) into the new Supabase project.
#
# Usage:
#   TARGET_DB_PASSWORD='your-new-supabase-db-password' ./scripts/clone_db.sh
#
# Optional:
#   DUMP_FILE=/tmp/msm.dump   # where to write the intermediate dump
#   SKIP_DUMP=1               # reuse an existing $DUMP_FILE
#   SKIP_DROP=1               # do NOT drop public on target before restore
#   JOBS=4                    # parallel restore workers

set -euo pipefail

cd "$(dirname "$0")/.."

if [[ -z "${TARGET_DB_PASSWORD:-}" ]]; then
  echo "ERROR: set TARGET_DB_PASSWORD env var (the new Supabase project's DB password)." >&2
  exit 1
fi

# Load .env to get SOURCE DATABASE_URL
if [[ ! -f .env ]]; then
  echo "ERROR: .env not found in $(pwd)" >&2
  exit 1
fi
# shellcheck disable=SC1091
set -a; source .env; set +a

SOURCE_DATABASE_URL="${DATABASE_URL:-}"
if [[ -z "$SOURCE_DATABASE_URL" ]]; then
  echo "ERROR: DATABASE_URL not set in .env" >&2
  exit 1
fi

TARGET_DATABASE_URL="postgresql://postgres:${TARGET_DB_PASSWORD}@db.ycuyduhsqsrvrtulqhzc.supabase.co:5432/postgres"

DUMP_FILE="${DUMP_FILE:-/tmp/msm_full.dump}"
JOBS="${JOBS:-4}"

# Sanity-check tools
for bin in pg_dump pg_restore psql; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "ERROR: '$bin' not found. Install with: brew install libpq && brew link --force libpq" >&2
    exit 1
  fi
done

echo "==> Source: $(echo "$SOURCE_DATABASE_URL" | sed -E 's#:[^:@/]+@#:****@#')"
echo "==> Target: $(echo "$TARGET_DATABASE_URL" | sed -E 's#:[^:@/]+@#:****@#')"
echo "==> Dump file: $DUMP_FILE"
echo

read -r -p "Proceed? This will OVERWRITE the public schema on the target. [y/N] " ans
[[ "$ans" =~ ^[Yy]$ ]] || { echo "Aborted."; exit 1; }

# 1. Verify connectivity to both
echo "==> Checking source connectivity..."
psql "$SOURCE_DATABASE_URL" -c "SELECT current_database(), now();" >/dev/null

echo "==> Checking target connectivity..."
psql "$TARGET_DATABASE_URL" -c "SELECT current_database(), now();" >/dev/null

# 2. Dump source
if [[ "${SKIP_DUMP:-0}" == "1" && -f "$DUMP_FILE" ]]; then
  echo "==> Skipping dump (SKIP_DUMP=1), reusing $DUMP_FILE"
else
  echo "==> Dumping public schema from source..."
  pg_dump "$SOURCE_DATABASE_URL" \
    --schema=public \
    --no-owner \
    --no-privileges \
    -Fc \
    -f "$DUMP_FILE"
  echo "    wrote $(du -h "$DUMP_FILE" | cut -f1) to $DUMP_FILE"
fi

# 3. Reset target public schema
if [[ "${SKIP_DROP:-0}" == "1" ]]; then
  echo "==> Skipping schema drop (SKIP_DROP=1)"
else
  echo "==> Dropping & recreating public schema on target..."
  psql "$TARGET_DATABASE_URL" -v ON_ERROR_STOP=1 <<'SQL'
DROP SCHEMA IF EXISTS public CASCADE;
CREATE SCHEMA public;
GRANT ALL ON SCHEMA public TO postgres, anon, authenticated, service_role;
SQL
fi

# 4. Restore
echo "==> Restoring into target (jobs=$JOBS)..."
pg_restore \
  --no-owner \
  --no-privileges \
  --dbname="$TARGET_DATABASE_URL" \
  --jobs="$JOBS" \
  "$DUMP_FILE"

# 5. Quick verification
echo "==> Verifying..."
psql "$TARGET_DATABASE_URL" -c "\dt public.*" || true
psql "$TARGET_DATABASE_URL" -c "SELECT version FROM public.schema_migrations ORDER BY version;" || true

echo
echo "Done. Source DB was not modified."
