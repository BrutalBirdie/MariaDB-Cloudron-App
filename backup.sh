#!/bin/bash

set -eu

source /app/pkg/common.sh

readonly tmp_dir="${DUMP_DIR}.new"

rm -rf "${tmp_dir}"
mkdir -p "${tmp_dir}"

echo "==> Dumping accounts and grants"
mariadb-dump --no-defaults --socket="${MDB_SOCKET}" --user=root --system=users > "${tmp_dir}/users.sql"

# dump files are numbered, the database name is inside the dump (--databases)
i=0
while IFS= read -r db; do
    i=$((i + 1))
    echo "==> Dumping database ${db}"
    mariadb-dump --no-defaults --socket="${MDB_SOCKET}" --user=root --single-transaction --quick --routines --events --triggers --no-tablespaces \
        --hex-blob --skip-dump-date --max-statement-time=0 --databases "${db}" | zstd -q -T0 > "${tmp_dir}/$(printf '%04d' "${i}").sql.zst"
done < <(mariadb_admin -N -B -e "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys') AND SCHEMA_NAME NOT LIKE '\\_mdb\\_%' ORDER BY SCHEMA_NAME")

rm -rf "${DUMP_DIR}"
mv "${tmp_dir}" "${DUMP_DIR}"
chown -R cloudron:cloudron "${DUMP_DIR}"
chmod 0700 "${DUMP_DIR}"

echo "==> Backup dump complete (${i} databases)"
