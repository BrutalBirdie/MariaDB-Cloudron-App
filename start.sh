#!/bin/bash

set -eu

source /app/pkg/common.sh

readonly ALLOWLIST_FILE="/app/data/allowed_networks.txt"

mkdir -p /run/nginx "${MDB_RUN_DIR}/tls" "${MDB_RUN_DIR}/conf" "${MDB_RUN_DIR}/panel" "${MDB_RUN_DIR}/rpc"
chown mysql:mysql "${MDB_RUN_DIR}/tls" "${MDB_RUN_DIR}/conf"
chmod 0750 "${MDB_RUN_DIR}/tls"
chown cloudron:cloudron "${MDB_RUN_DIR}/panel"
# the worker (mysql) creates the socket, the setgid group lets the panel (cloudron) connect
chown mysql:cloudron "${MDB_RUN_DIR}/rpc"
chmod 2750 "${MDB_RUN_DIR}/rpc"
mdb_prepare_dirs

if [[ ! -d "${DATADIR}/mysql" ]]; then
    mdb_install_db
fi

if [[ ! -f /app/data/env.sh ]]; then
    cat > /app/data/env.sh <<'EOF'
# Restart the app after changing these.
# Cloudron groups whose members are admins in the panel, separated by commas. Example: "dba, ops"
export MARIADB_ADMIN_GROUP=""
# Cloudron usernames that are always admins, separated by commas.
export MARIADB_ADMINS=""
# Snapshots kept per database and type (manual, scheduled, before restore), oldest are deleted first.
export MARIADB_MAX_SNAPSHOTS=7
# Uploaded dump files are deleted after this many days unless marked to keep.
export MARIADB_UPLOAD_RETENTION_DAYS=8
EOF
fi
source /app/data/env.sh

if [[ ! -f "${ALLOWLIST_FILE}" ]]; then
    printf '# Networks allowed to connect to MariaDB from outside, one per line.\n# Managed by the admin panel. Manual edits apply to new connections at once.\n' > "${ALLOWLIST_FILE}"
fi

chown -R cloudron:cloudron /app/data
readonly SNAPSHOT_DIR="${MDB_ROOT}/snapshots"
mkdir -p "${SNAPSHOT_DIR}"
# written by the worker (mysql), read and deleted by the panel (cloudron)
chown -R mysql:cloudron "${SNAPSHOT_DIR}"
find "${SNAPSHOT_DIR}" -type d -exec chmod 2770 {} +
find "${SNAPSHOT_DIR}" -type f -exec chmod 0640 {} +
mkdir -p /app/data/uploads
# written and deleted by the panel (cloudron), read by the worker (mysql)
chown -R cloudron:mysql /app/data/uploads
find /app/data/uploads -type d -exec chmod 2750 {} +
find /app/data/uploads -type f -exec chmod 0640 {} +

echo "==> Copying TLS certificate"
if [[ ! -f /etc/certs/tls_cert.pem || ! -f /etc/certs/tls_key.pem ]]; then
    echo "==> /etc/certs/tls_cert.pem missing, refusing to start without TLS"
    exit 1
fi
install -o mysql -g mysql -m 0644 /etc/certs/tls_cert.pem "${MDB_RUN_DIR}/tls/server.crt"
install -o mysql -g mysql -m 0600 /etc/certs/tls_key.pem "${MDB_RUN_DIR}/tls/server.key"

if [[ -f /sys/fs/cgroup/cgroup.controllers ]]; then
    memory_limit=$(cat /sys/fs/cgroup/memory.max)
    [[ "${memory_limit}" == "max" ]] && memory_limit=$((2 * 1024 * 1024 * 1024))
else
    memory_limit=$(cat /sys/fs/cgroup/memory/memory.limit_in_bytes)
fi
memory_mb=$((memory_limit / 1024 / 1024))

echo "==> Generating my.cnf (memory limit ${memory_mb}MB)"
cat > "${MDB_RUN_DIR}/my.cnf" <<EOF
[mariadbd]
datadir = ${DATADIR}
socket = ${MDB_SOCKET}
pid-file = ${MDB_RUN_DIR}/conf/mariadbd.pid
# clients come in through the allowlist proxy on 3306 - see panel/internal/proxy
bind-address = 127.0.0.1
port = 3307
proxy-protocol-networks = 127.0.0.1
skip-name-resolve = ON
max_connections = 100

ssl-cert = ${MDB_RUN_DIR}/tls/server.crt
ssl-key = ${MDB_RUN_DIR}/tls/server.key
tls-version = TLSv1.3
require-secure-transport = ON

# Cloudron app passwords, v1 runs PAM in the server process without the setuid helper
# https://mariadb.com/docs/server/reference/plugins/authentication-plugins/authentication-with-pluggable-authentication-modules-pam/authentication-plugin-pam
plugin-load-add = auth_pam_v1
pam-use-cleartext-plugin = ON

local-infile = OFF
event-scheduler = ON
innodb_buffer_pool_size = $((memory_mb / 4))M
# never log statements into the app log, they can contain passwords or tenant data
general-log = OFF
slow-query-log = OFF

# variables admins change in the panel, written by the worker - see panel/internal/tuning
!include ${MDB_RUN_DIR}/conf/tuning.cnf
EOF
chown mysql:mysql "${MDB_RUN_DIR}/my.cnf"

echo "==> Applying baseline accounts"
mdb_start_local
mariadb_admin <<'EOF'
CREATE ROLE IF NOT EXISTS tenants;
-- the worker connects as mysql and grants tenant roles, that needs their admin option
GRANT tenants TO mysql@localhost WITH ADMIN OPTION;
-- the panel signs in over the socket as OS user cloudron and only gets its own database
CREATE USER IF NOT EXISTS cloudron@localhost IDENTIFIED VIA unix_socket;
ALTER USER cloudron@localhost IDENTIFIED VIA unix_socket;
CREATE DATABASE IF NOT EXISTS mdbpanel CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
GRANT ALL PRIVILEGES ON mdbpanel.* TO cloudron@localhost;
EOF
# roles restored from a backup are administered by root only
mariadb_admin -N -B -e "SELECT CONCAT('GRANT \`', User, '\` TO mysql@localhost WITH ADMIN OPTION;') FROM mysql.roles_mapping t WHERE t.Role = 'tenants' AND t.Host = ''
    AND NOT EXISTS (SELECT 1 FROM mysql.roles_mapping m WHERE m.Role = t.User AND m.User = 'mysql' AND m.Host = 'localhost' AND m.Admin_option = 'Y')" | mariadb_admin
gosu cloudron:cloudron /app/code/panel migrate
gosu mysql:mysql /app/code/panel render-tuning --out "${MDB_RUN_DIR}/conf/tuning.cnf"
mdb_stop_local

echo "==> Starting supervisor"
exec /usr/bin/supervisord --configuration /etc/supervisor/supervisord.conf --nodaemon -i MariaDB
