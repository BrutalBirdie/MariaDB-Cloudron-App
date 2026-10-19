#!/bin/bash

set -eu

source /app/pkg/common.sh

if [[ ! -f "${DUMP_DIR}/users.sql" ]]; then
    echo "==> No dumps in ${DUMP_DIR}, keeping existing data directory"
    exit 0
fi

readonly live_dir="${DATADIR}"
readonly new_dir="${DATADIR}.restore"

echo "==> Restoring dumps into a new data directory"
rm -rf "${new_dir}"
mdb_prepare_dirs
DATADIR="${new_dir}" mdb_install_db
DATADIR="${new_dir}" mdb_start_local

fail() {
    DATADIR="${new_dir}" mdb_stop_local || true
    rm -rf "${new_dir}"
    echo "==> Restore failed, existing data directory left untouched"
    exit 1
}

# users.sql creates root, mysql and mariadb.sys again, which exist in the new directory, and
# every role twice: MariaDB also runs the /*!80001 CREATE ROLE */ meant for MySQL
mariadb_admin --force < "${DUMP_DIR}/users.sql" 2>&1 \
    | grep -v -e "^ERROR 1396 .*Operation CREATE USER failed for '\(root\|mysql\|mariadb.sys\)'@'localhost'" -e "^ERROR 1396 .*Operation CREATE ROLE failed" -e "^--------------$" -e "^CREATE \(USER\|ROLE\) " -e "^$" || true

for dump in "${DUMP_DIR}"/*.sql.zst; do
    [[ -f "${dump}" ]] || continue
    echo "==> Restoring ${dump}"
    zstd -dc "${dump}" | mariadb_admin --binary-mode || fail
done

DATADIR="${new_dir}" mdb_stop_local

echo "==> Swapping in restored data directory"
rm -rf "${live_dir}"
mv "${new_dir}" "${live_dir}"

echo "==> Restore complete"
