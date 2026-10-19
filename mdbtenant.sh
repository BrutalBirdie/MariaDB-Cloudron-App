#!/bin/bash

set -eu

source /app/pkg/common.sh

usage() {
    echo "Usage: $0 create <name>            create role <name> and database <name> for it"
    echo "       $0 login <role> <username>  create or rotate the login of a Cloudron user for a role"
    echo "       $0 list                     list roles, their databases and logins"
    echo "Owners are managed in the admin panel."
    exit 1
}

validate_name() {
    if [[ ! "$1" =~ ^[a-z][a-z0-9_]{2,62}$ || " mysql mariadb information_schema performance_schema sys root cloudron mdbpanel tenants public test " == *" $1 "* ]]; then
        echo "Invalid name '$1': use 3-63 chars of a-z, 0-9, _ starting with a letter; built-in names are reserved" >&2
        exit 1
    fi
}

random_password() {
    openssl rand -base64 64 | tr -dc 'A-Za-z0-9' | head -c 32
}

# mysql_native_password hash, computed here so the plaintext never reaches the server
native_hash() {
    printf '*%s' "$(printf '%s' "$1" | openssl dgst -sha1 -binary | openssl dgst -sha1 -hex | sed 's/.* //' | tr 'a-f' 'A-F')"
}

print_connection() {
    local login="$1" password="$2" db="$3"
    local host="${CLOUDRON_APP_DOMAIN:-<app domain>}" port="${MARIADB_PORT:-3306}"
    echo
    echo "Login:    ${login}"
    echo "Password: ${password}"
    echo "Command:  mariadb --host=${host} --port=${port} --user=${login} --password --ssl-verify-server-cert ${db}"
    echo
    echo "The password is not stored anywhere. Save it now."
}

[[ $# -ge 1 ]] || usage

case "$1" in
create)
    [[ $# -eq 2 ]] || usage
    validate_name "$2"
    db_grant="${2//_/\\_}"
    mariadb_admin <<EOF
CREATE ROLE \`$2\`;
GRANT tenants TO \`$2\`;
GRANT \`$2\` TO mysql@localhost WITH ADMIN OPTION;
CREATE DATABASE \`$2\`;
GRANT ALL PRIVILEGES ON \`${db_grant}\`.* TO \`$2\`;
INSERT IGNORE INTO mdbpanel.role_settings (role_name) VALUES ('$2');
EOF
    echo "Created role and database $2. Add owners in the admin panel, they create their own logins there."
    ;;
login)
    [[ $# -eq 3 ]] || usage
    validate_name "$2"
    if [[ ! "$3" =~ ^[a-z0-9][a-z0-9._-]*$ ]] || (( ${#2} + ${#3} > 127 )); then
        echo "Invalid username '$3', or role and username are too long together" >&2
        exit 1
    fi
    login="$2.$3"
    password=$(random_password)
    hash=$(native_hash "${password}")
    limit=$(mariadb_admin -N -B -e "SELECT COALESCE(MAX(conn_limit), 20) FROM mdbpanel.role_settings WHERE role_name = '$2'")
    mariadb_admin <<EOF
CREATE USER IF NOT EXISTS '${login}'@'%' IDENTIFIED BY PASSWORD '${hash}' REQUIRE SSL WITH MAX_USER_CONNECTIONS ${limit} ACCOUNT LOCK;
ALTER USER '${login}'@'%' IDENTIFIED BY PASSWORD '${hash}' REQUIRE SSL WITH MAX_USER_CONNECTIONS ${limit};
GRANT \`$2\` TO '${login}'@'%';
SET DEFAULT ROLE \`$2\` FOR '${login}'@'%';
ALTER USER '${login}'@'%' ACCOUNT UNLOCK;
INSERT INTO mdbpanel.role_logins (login_role, role_name, username) VALUES ('${login}', '$2', '$3')
    ON DUPLICATE KEY UPDATE auth = 'password', rotated_at = now(6);
EOF
    # the login is definer of what its user creates, MariaDB ignores roles there
    mariadb_admin -N -B --raw -e "SELECT CONCAT('GRANT ALL PRIVILEGES ON \\\`', Db, '\\\`.* TO \\'${login}\\'@\\'%\\';') FROM mysql.db WHERE User = '$2' AND Host = ''" | mariadb_admin
    print_connection "${login}" "${password}" "$2"
    echo "The login is locked automatically when $3 is not an owner of $2."
    ;;
list)
    mariadb_admin -t -e "SELECT r.User AS role, GROUP_CONCAT(REPLACE(d.Db, '\\\\_', '_') ORDER BY d.Db SEPARATOR ', ') AS dbs
        FROM mysql.roles_mapping r LEFT JOIN mysql.db d ON d.User = r.User AND d.Host = ''
        WHERE r.Role = 'tenants' AND r.Host = '' GROUP BY r.User ORDER BY r.User"
    mariadb_admin -t -e "SELECT login_role AS login, role_name AS role, username, auth, rotated_at FROM mdbpanel.role_logins ORDER BY role_name, username"
    ;;
*)
    usage
    ;;
esac
