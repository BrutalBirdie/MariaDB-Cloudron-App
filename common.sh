#!/bin/bash

readonly MDB_ROOT="/var/lib/mysql"
DATADIR="${MDB_ROOT}/data"
# socket lives in the persistent dir so backupCommand's container can reach it
readonly MDB_SOCKET_DIR="${MDB_ROOT}/run"
readonly MDB_SOCKET="${MDB_SOCKET_DIR}/mysqld.sock"
readonly MDB_RUN_DIR="/run/mdb"
readonly DUMP_DIR="/app/data/dumps"

mdb_prepare_dirs() {
    mkdir -p "${MDB_ROOT}" "${MDB_SOCKET_DIR}"
    chown mysql:mysql "${MDB_ROOT}" "${MDB_SOCKET_DIR}"
    chmod 0755 "${MDB_ROOT}" "${MDB_SOCKET_DIR}"
    [[ -d "${DATADIR}" ]] && chown -R mysql:mysql "${DATADIR}" && chmod 0700 "${DATADIR}"
    return 0
}

# root@localhost and mysql@localhost authenticate with unix_socket only
mdb_install_db() {
    echo "==> Initializing new data directory"
    mkdir -p "${DATADIR}"
    chown mysql:mysql "${DATADIR}"
    mariadb-install-db --no-defaults --user=mysql --datadir="${DATADIR}" --auth-root-authentication-method=socket --skip-test-db > /tmp/mariadb-install.log 2>&1 \
        || { cat /tmp/mariadb-install.log; return 1; }
}

# socket-only instance for maintenance (setup, restore). Nothing listens on TCP.
mdb_start_local() {
    rm -f "${MDB_SOCKET}"
    gosu mysql mariadbd --no-defaults --datadir="${DATADIR}" --socket="${MDB_SOCKET}" --skip-networking --pid-file=/tmp/mariadb-maintenance.pid \
        --log-error=/tmp/mariadb-maintenance.log --plugin-load-add=auth_pam_v1 &
    for _ in $(seq 1 120); do
        mariadb-admin --no-defaults --socket="${MDB_SOCKET}" --silent ping >/dev/null 2>&1 && return 0
        kill -0 $! 2>/dev/null || break
        sleep 1
    done
    cat /tmp/mariadb-maintenance.log
    return 1
}

mdb_stop_local() {
    mariadb-admin --no-defaults --socket="${MDB_SOCKET}" shutdown
    for _ in $(seq 1 600); do
        [[ -e /tmp/mariadb-maintenance.pid ]] || return 0
        sleep 0.5
    done
    echo "==> MariaDB did not stop"
    return 1
}

mariadb_admin() {
    mariadb --no-defaults --socket="${MDB_SOCKET}" --user=root "$@"
}
