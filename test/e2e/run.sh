#!/bin/bash
# End-to-end test: fresh install, real browser, mock OIDC, MariaDB over TLS,
# backup and restore. Usage: test/e2e/run.sh   (needs Docker)
# SCREENSHOTS=1 also writes the store screenshots to screenshots/
# ARTIFACTS=<dir> keeps step screenshots, store screenshots, failed pages and the app log

set -eu

cd "$(dirname "$0")"
readonly root=$(cd ../.. && pwd)
readonly img=mdbapp:e2e
readonly net=mdbe2e
readonly app=mdbe2e-app
readonly mock=mdbe2e-mock
readonly scim=mdbe2e-scim
readonly smtp=mdbe2e-smtp
# fixed, so the test certificate can name it and clients verify it
readonly subnet=172.31.250.0/24
readonly app_ip=172.31.250.10
readonly work=$(mktemp -d)
failures=0

cleanup() {
    if [[ -n "${ARTIFACTS:-}" ]]; then
        mkdir -p "${ARTIFACTS}"
        docker logs "${app}" > "${ARTIFACTS}/app.log" 2>&1 || true
        cp -R "${work}/steps" "${work}/shots" "${work}/"fail-*.html "${ARTIFACTS}/" 2>/dev/null || true
    fi
    if [[ -n "${KEEP:-}" ]]; then
        echo "==> Kept containers and ${work}"
    else
        cleanup_containers
        rm -rf "${work}"
    fi
}
cleanup_containers() {
    docker rm -f "${app}" "${mock}" "${scim}" "${smtp}" >/dev/null 2>&1 || true
    docker volume rm mdbe2e-data mdbe2e-db >/dev/null 2>&1 || true
    docker network rm "${net}" >/dev/null 2>&1 || true
}
cleanup_containers
trap cleanup EXIT

pass() { echo "PASS [shell] $1"; }
fail() { echo "FAIL [shell] $1 :: $2"; failures=$((failures + 1)); }

expect() { # expect <name> <regex> <output>
    if [[ "$3" =~ $2 ]]; then pass "$1"; else fail "$1" "${3:0:300}"; fi
}

browser() {
    docker run --rm --network "container:${app}" -u "$(id -u):$(id -g)" -e HOME=/tmp -e APP_IP="${app_ip}" -v "${PWD}:/e2e" -v "${work}:/work" -w /e2e \
        mcr.microsoft.com/playwright:v1.64.1-noble node e2e.mjs "$1" || failures=$((failures + 1))
}

# runs a client tool in the app's network namespace, so connections come from app_ip
client() {
    docker run --rm -i -u "$(id -u):$(id -g)" --network "container:${app}" -v "${work}/certs:/certs:ro" -v "${work}/files:/files" "${img}" "$@"
}

db() { # db <password> <user> <database> <sql> [extra client options]
    client mariadb --no-defaults --host="${app_ip}" --port=3306 --connect-timeout=5 --ssl-ca=/certs/tls_cert.pem --ssl-verify-server-cert \
        --user="$2" --password="$1" ${5:-} --batch --skip-column-names --database="$3" -e "$4" 2>&1 || true
}

superuser() {
    docker exec "${app}" mariadb --no-defaults --socket=/var/lib/mysql/run/mysqld.sock --user=root --batch --raw --skip-column-names -e "$1" ${2:-} 2>&1
}

wait_healthy() {
    for _ in $(seq 1 90); do
        docker exec "${app}" curl -sf http://localhost:8000/healthz >/dev/null 2>&1 && return 0
        sleep 1
    done
    docker logs "${app}" | tail -30
    return 1
}

app_run() {
    docker run -d --name "${app}" --network "${net}" --ip "${app_ip}" --read-only --tmpfs /run --tmpfs /tmp \
        -v mdbe2e-data:/app/data -v mdbe2e-db:/var/lib/mysql -v "${work}/certs:/etc/certs:ro" \
        -e CLOUDRON_APP_DOMAIN=db.test -e CLOUDRON_APP_ORIGIN=http://localhost:8000 -e MARIADB_PORT=3306 \
        -e CLOUDRON_OIDC_ISSUER="http://${mock}:8080/default" -e CLOUDRON_OIDC_CLIENT_ID=mdbpanel -e CLOUDRON_OIDC_CLIENT_SECRET=secret \
        -e CLOUDRON_SCIM_ORIGIN="http://${scim}:8080" -e CLOUDRON_SCIM_TOKEN=scimtoken -e PANEL_SCIM_CACHE=1s -e PANEL_ACCESS_SYNC=3s \
        -e CLOUDRON_MAIL_SMTP_SERVER=mail.test -e CLOUDRON_MAIL_STARTTLS_PORT=2587 -e CLOUDRON_MAIL_SMTP_USERNAME=app -e CLOUDRON_MAIL_SMTP_PASSWORD=mailpass \
        -e CLOUDRON_APP_BRIDGE_ORIGIN="http://${scim}:3006" -e CLOUDRON_WEBADMIN_ORIGIN=https://my.db.test \
        -e CLOUDRON_MAIL_FROM=db.app@db.test -e PANEL_NOTIFY_INTERVAL=2s -e SSL_CERT_FILE=/etc/certs/mail_cert.pem \
        "${img}" >/dev/null
}

echo "==> Building"
docker build -q -t "${img}" "${root}" >/dev/null
docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -v "${PWD}:/e2e" -w /e2e mcr.microsoft.com/playwright:v1.64.1-noble npm install --no-audit --no-fund --silent

mkdir -p "${work}/certs" "${work}/files"
openssl req -x509 -newkey rsa:2048 -nodes -keyout "${work}/certs/tls_key.pem" -out "${work}/certs/tls_cert.pem" -days 2 \
    -subj "/CN=db.test" -addext "subjectAltName=DNS:db.test,IP:${app_ip}" 2>/dev/null
openssl req -x509 -newkey rsa:2048 -nodes -keyout "${work}/certs/mail_key.pem" -out "${work}/certs/mail_cert.pem" -days 2 \
    -subj "/CN=mail.test" -addext "subjectAltName=DNS:mail.test" 2>/dev/null
chmod 0644 "${work}/certs/"*
chmod 0777 "${work}" "${work}/files"

docker network create --subnet "${subnet}" "${net}" >/dev/null
docker run -d --name "${mock}" --network "${net}" -e JSON_CONFIG='{"interactiveLogin":true}' ghcr.io/navikt/mock-oauth2-server:6.0.3 >/dev/null
scim_users() { # scim_users <carol's groups as JSON array> [eve's app passwords as JSON array]
    cat > "${work}/scim.json" <<EOF
{
  "appPasswords": {"eve": ${2:-[]}},
  "groups": [{"id": "g-admins", "displayName": "mariadb-admins"}, {"id": "g-team", "displayName": "dbteam"}, {"id": "g-ops", "displayName": "ops"}],
  "users": [
    {"userName": "ann", "displayName": "Ann Admin", "groups": ["g-admins"]},
    {"userName": "bob", "displayName": "Bob Builder", "groups": []},
    {"userName": "carol", "displayName": "Carol Team", "groups": $1},
    {"userName": "eve", "displayName": "Eve", "groups": ["g-ops"]}
  ]
}
EOF
}
scim_users '["g-team"]'
docker run -d --name "${scim}" --network "${net}" -e SCIM_TOKEN=scimtoken -v "${PWD}:/e2e:ro" -v "${work}:/work:ro" -w /e2e \
    mcr.microsoft.com/playwright:v1.64.1-noble node scim-mock.mjs >/dev/null
docker run -d --name "${smtp}" --network "${net}" --network-alias mail.test -u "$(id -u):$(id -g)" -v "${PWD}:/e2e:ro" -v "${work}:/work" -w /e2e \
    mcr.microsoft.com/playwright:v1.64.1-noble node smtp-mock.mjs >/dev/null

mails() { # mails <recipient> <subject regex>: number of matching messages
    grep -lE "^Subject: $2" "${work}"/mail/*-"$1".eml 2>/dev/null | wc -l
}
mail_text() { # mail_text <recipient>: all messages with quoted-printable soft line breaks removed
    cat "${work}"/mail/*-"$1".eml 2>/dev/null | sed -z 's/=\n//g'
}
wait_mail() { # wait_mail <recipient> <regex>
    for _ in $(seq 1 30); do
        grep -qE "$2" <<<"$(mail_text "$1")" && return 0
        sleep 1
    done
    return 1
}

echo "==> Fresh install"
app_run
wait_healthy
expect "env.sh created on first start" "MARIADB_MAX_SNAPSHOTS=7" "$(docker exec "${app}" cat /app/data/env.sh)"
expect "no remote access before allowlist" "Host '${app_ip}' is not allowed to connect" "$(db x e2e_bob.bob e2e_bob 'select 1')"
expect "MariaDB itself only listens on localhost" "127.0.0.1:3307" "$(docker exec "${app}" ss -ltn | grep 3307)"
expect "no admin group by default" '^export MARIADB_ADMIN_GROUP=""$' "$(docker exec "${app}" grep MARIADB_ADMIN_GROUP= /app/data/env.sh)"
browser noAdminGroup

echo "==> Several admin groups in MARIADB_ADMIN_GROUP"
docker exec "${app}" sed -i 's/^export MARIADB_ADMIN_GROUP=.*/export MARIADB_ADMIN_GROUP="dba, mariadb-admins"/' /app/data/env.sh
docker restart "${app}" >/dev/null
wait_healthy
browser admin
expect "test mail delivered" "^1$" "$(mails dba@example.test 'MariaDB: test mail')"

echo "==> Server parameters"
browser adminServer
for _ in $(seq 1 20); do
    [[ "$(superuser 'select @@global.tmp_table_size')" == "33554432" ]] && break
    sleep 1
done
expect "variable applied by the worker" "^33554432$" "$(superuser 'select @@global.tmp_table_size')"
expect "follower variable set with it" "^33554432$" "$(superuser 'select @@global.max_heap_table_size')"
expect "max_connections applied at once" "^150$" "$(superuser 'select @@global.max_connections')"
expect "variable file only writable by mysql" "^mysql 644$" "$(docker exec "${app}" stat -c '%U %a' /run/mdb/conf/tuning.cnf)"
expect "variable file content" "tmp_table_size = 33554432" "$(docker exec "${app}" cat /run/mdb/conf/tuning.cnf)"
expect "injected option never written" "^0$" "$(docker exec "${app}" grep -c -e skip-grant -e init_file /run/mdb/conf/tuning.cnf || true)"
browser adminServer2
browser bobLogin
browser eveLogin
bob_pw=$(yq -p json -oy '.bobPassword' "${work}/state.json")
eve_pw=$(yq -p json -oy '.evePassword' "${work}/state.json")

echo "==> MariaDB access"
expect "bob connects" "^1$" "$(db "${bob_pw}" e2e_bob.bob e2e_bob 'select 1')"
expect "bob's connection uses TLS 1.3" "TLSv1.3" "$(db "${bob_pw}" e2e_bob.bob e2e_bob "show status like 'Ssl_version'")"
expect "MariaDB sees the client address" "^${app_ip}:" "$(db "${bob_pw}" e2e_bob.bob e2e_bob 'select host from information_schema.processlist where id = connection_id()')"
expect "bob creates data and objects" "^3$" "$(db "${bob_pw}" e2e_bob.bob e2e_bob "create table t(x int); create table t_log(x int);
    create trigger t_ins after insert on t for each row insert into t_log values (new.x);
    create view v_t as select x from t; create function f_one() returns int return 1; create procedure p_cnt() select count(*) from t;
    create event e_noop on schedule every 1 day do select 1; insert into t values (1),(2),(3); select count(*) from t_log")"
expect "plain connection rejected" "insecure transport are prohibited" "$(db "${bob_pw}" e2e_bob.bob e2e_bob 'select 1' '--skip-ssl')"
expect "TLS 1.2 rejected" "TLS|SSL|protocol version" "$(db "${bob_pw}" e2e_bob.bob e2e_bob 'select 1' '--tls-version=TLSv1.2')"
expect "bob cannot open eve database" "Access denied" "$(db "${bob_pw}" e2e_bob.bob e2e_eve 'select 1')"
expect "bob cannot read mysql" "Access denied|command denied" "$(db "${bob_pw}" e2e_bob.bob e2e_bob 'select user from mysql.user')"
expect "bob cannot open mdbpanel" "Access denied" "$(db "${bob_pw}" e2e_bob.bob mdbpanel 'select 1')"
expect "bob only sees his databases" "^e2e_bob information_schema$" "$(db "${bob_pw}" e2e_bob.bob e2e_bob 'show databases' | paste -sd ' ')"
expect "bob cannot create databases with SQL" "Access denied" "$(db "${bob_pw}" e2e_bob.bob e2e_bob 'create database sneaky')"
expect "bob cannot create a look-alike database" "Access denied" "$(db "${bob_pw}" e2e_bob.bob e2e_bob 'create database e2eXbob')"
expect "bob cannot write files" "Access denied" "$(db "${bob_pw}" e2e_bob.bob e2e_bob "select 1 into outfile '/tmp/x'")"
expect "bob cannot read files" "Access denied" "$(db "${bob_pw}" e2e_bob.bob e2e_bob "load data infile '/etc/passwd' into table t")"
expect "bob cannot install plugins" "Access denied|denied" "$(db "${bob_pw}" e2e_bob.bob e2e_bob "install soname 'ha_federatedx'")"
expect "no FEDERATED engine" "Unknown storage engine|not supported|denied" "$(db "${bob_pw}" e2e_bob.bob e2e_bob "create table f (i int) engine=federated connection='mysql://root@127.0.0.1:3307/mysql/user'")"
expect "bob cannot change server variables" "Access denied" "$(db "${bob_pw}" e2e_bob.bob e2e_bob 'set global max_connections = 1')"
expect "bob cannot list other sessions" "^1$" "$(db "${bob_pw}" e2e_bob.bob e2e_bob 'select count(*) from information_schema.processlist')"
expect "root rejected remotely" "Access denied" "$(db x root mysql 'select 1')"
expect "mysql rejected remotely" "Access denied" "$(db x mysql mysql 'select 1')"
expect "cloudron rejected remotely" "Access denied" "$(db x cloudron mdbpanel 'select 1')"
expect "the role itself cannot log in" "Access denied" "$(db x e2e_bob e2e_bob 'select 1')"
expect "eve connects to her database" "^1$" "$(db "${eve_pw}" e2e_eve.eve e2e_eve 'select 1')"
expect "panel account has only its own database" "GRANT ALL PRIVILEGES ON \`mdbpanel\`.\\* TO \`cloudron\`@\`localhost\`" "$(superuser "show grants for cloudron@localhost")"
expect "panel account has no global privileges" "GRANT USAGE ON \\*\\.\\* TO \`cloudron\`@\`localhost\` IDENTIFIED VIA unix_socket" "$(superuser "show grants for cloudron@localhost")"

echo "==> Group owners and per-user logins"
browser carol
carol_pw=$(yq -p json -oy '.carolPassword' "${work}/state.json")
expect "carol connects with her own login" "^2$" "$(db "${carol_pw}" e2e_bob.carol e2e_bob 'create table carol_t(i int); create view carol_v as select count(*) as n from t; select count(*) from information_schema.tables where table_name like "carol%"')"
expect "bob uses carol's table and view" "^3$" "$(db "${bob_pw}" e2e_bob.bob e2e_bob 'select count(*) from carol_t; select n from carol_v' | tail -1)"
scim_users '[]'
for _ in $(seq 1 30); do
    [[ "$(superuser "select count(*) from mdbpanel.role_logins where login_role = 'e2e_bob.carol'")" == "0" ]] && break
    sleep 1
done
expect "carol's login locked after leaving the group" "Access denied|locked" "$(db "${carol_pw}" e2e_bob.carol e2e_bob 'select 1')"
expect "bob's login still works" "^1$" "$(db "${bob_pw}" e2e_bob.bob e2e_bob 'select 1')"
expect "carol's view still works without her login" "^3$" "$(db "${bob_pw}" e2e_bob.bob e2e_bob 'select n from carol_v')"
expect "removal audited" "login.remove" "$(superuser "select action from mdbpanel.audit_log where target = 'e2e_bob.carol'")"
db "${bob_pw}" e2e_bob.bob e2e_bob 'drop view carol_v; drop table carol_t' >/dev/null
browser carolGone
scim_users '["g-team"]'

# a directory outage must not revoke anything, otherwise every client would be locked out
outage_before=$(docker logs "${app}" 2>&1 | grep -c 'directory is not reachable' || true)
docker stop "${scim}" >/dev/null
outage_seen=0
for _ in $(seq 1 40); do
    outage_seen=$(( $(docker logs "${app}" 2>&1 | grep -c 'directory is not reachable' || true) - outage_before ))
    [[ ${outage_seen} -ge 2 ]] && break
    sleep 1
done
expect "outage noticed by the access sync" "^[2-9]|^[1-9][0-9]" "${outage_seen}"
expect "no login removed while SCIM is down" "^1$" "$(superuser "select count(*) from mdbpanel.role_logins where login_role = 'e2e_bob.bob'")"
expect "bob still connects while SCIM is down" "^1$" "$(db "${bob_pw}" e2e_bob.bob e2e_bob 'select 1')"
docker start "${scim}" >/dev/null

browser bob1
new_pw=$(yq -p json -oy '.bobPassword' "${work}/state.json")
expect "old password stops working" "Access denied" "$(db "${bob_pw}" e2e_bob.bob e2e_bob 'select 1')"
expect "new password works" "^1,2,3$" "$(db "${new_pw}" e2e_bob.bob e2e_bob "select group_concat(x order by x) from t")"
expect "self-service database grant has no wildcard" 'GRANT ALL PRIVILEGES ON `e2e\\_bob\\_two`\.\* TO `e2e_bob`' "$(superuser 'show grants for e2e_bob')"
expect "password not in logs" "^0$" "$(docker logs "${app}" 2>&1 | grep -c -- "${new_pw}" || true)"

db "${new_pw}" e2e_bob.bob e2e_bob 'delete from t; insert into t values (99)' >/dev/null
browser bob2
expect "restore brings data back" "^1,2,3$" "$(db "${new_pw}" e2e_bob.bob e2e_bob "select group_concat(x order by x) from t")"
expect "restored trigger fires" "^7$" "$(db "${new_pw}" e2e_bob.bob e2e_bob "delete from t_log; insert into t values (7); select group_concat(x) from t_log; delete from t where x = 7")"
expect "restored view and function" "^3 1$" "$(db "${new_pw}" e2e_bob.bob e2e_bob "select (select count(*) from v_t), f_one()" | tr '\t' ' ')"
expect "restored procedure runs" "^3$" "$(db "${new_pw}" e2e_bob.bob e2e_bob 'call p_cnt()')"
expect "restored objects belong to the role" "^e2e_bob@? e2e_bob@? e2e_bob@? e2e_bob@? e2e_bob@?$" "$(superuser "select (select definer from information_schema.views where table_schema = 'e2e_bob' and table_name = 'v_t'),
    (select definer from information_schema.triggers where trigger_schema = 'e2e_bob'), (select definer from information_schema.routines where routine_schema = 'e2e_bob' and routine_name = 'f_one'),
    (select definer from information_schema.routines where routine_schema = 'e2e_bob' and routine_name = 'p_cnt'), (select definer from information_schema.events where event_schema = 'e2e_bob')" | tr '\t' ' ')"
expect "notification opt-in stored" "^1$" "$(superuser "select notify_failures from mdbpanel.user_prefs where username = 'bob'")"

echo "==> Connections and snapshots overview"
db "${new_pw}" e2e_bob.bob e2e_bob 'select sleep(20)' >/dev/null &
sleeper=$!
sleep 2
browser adminConnections
wait "${sleeper}" 2>/dev/null || true

echo "==> Snapshot storage"
snap_file=$(docker exec "${app}" find /var/lib/mysql/snapshots -name '*.sql.zst' | head -1)
expect "snapshots stored outside /app/data" "^/var/lib/mysql/snapshots/e2e_bob/" "${snap_file}"
expect "no snapshot directory in /app/data" "^no$" "$(docker exec "${app}" bash -c 'test -e /app/data/snapshots && echo yes || echo no')"
expect "snapshots are zstd SQL dumps" "CREATE TABLE \`t\`" "$(docker exec "${app}" zstd -dc "${snap_file}" | grep 'CREATE TABLE `t`')"
expect "snapshots carry no database name" "^0$" "$(docker exec "${app}" zstd -dc "${snap_file}" | grep -c '^USE ' || true)"

if [[ "${SCREENSHOTS:-}" == "1" || -n "${ARTIFACTS:-}" ]]; then
    browser screenshots
fi
if [[ "${SCREENSHOTS:-}" == "1" ]]; then
    mkdir -p "${root}/screenshots"
    cp "${work}/shots/"*.png "${root}/screenshots/"
fi

echo "==> Uploads and import"
dump_tls=(--ssl-ca=/certs/tls_cert.pem --ssl-verify-server-cert --host="${app_ip}" --port=3306 --user=e2e_bob.bob --password="${new_pw}")
db "${new_pw}" e2e_bob.bob e2e_bob "create table imported_marker(v text); insert into imported_marker values ('from dump')" >/dev/null
client sh -c "mariadb-dump --no-defaults ${dump_tls[*]} --single-transaction --routines --events --triggers --no-tablespaces e2e_bob > '/files/import me.sql'"
db "${new_pw}" e2e_bob.bob e2e_bob 'drop table imported_marker' >/dev/null
printf -- '-- dump with client commands\ncreate table evil_ok (i int);\n\\! touch /tmp/pwned\nsystem touch /tmp/pwned2\nselect 1;\n' > "${work}/files/evil.sql"
printf -- '-- needs admin rights\nCREATE TABLE admin_try (i int);\nSET GLOBAL max_connections = 1;\n' > "${work}/files/admin.sql"
head -c 3145728 /dev/urandom > "${work}/files/big.bin"
chmod -R a+rwX "${work}/files"
browser uploads
# the File Manager copies with the owner of the parent directory
docker exec "${app}" install -o cloudron -g mysql -m 0644 /app/data/env.sh /app/data/uploads/e2e_bob/manual.sql
manual_sha=$(docker exec "${app}" sha256sum /app/data/uploads/e2e_bob/manual.sql | cut -d' ' -f1)
yq -i -p json -o json ".manualSha = \"${manual_sha}\"" "${work}/state.json"
browser uploads2
# change one byte but keep size and mtime, so only the checksum can notice
docker exec "${app}" bash -c 'f=/app/data/uploads/e2e_bob/import_me-1.sql; m=$(stat -c %y "$f"); printf X | dd of="$f" bs=1 seek=200 conv=notrunc status=none; touch -d "$m" "$f"'
browser tampered

db "${new_pw}" e2e_bob.bob e2e_bob "create table big (id int primary key, payload varchar(64), key (payload)) as select seq as id, concat(md5(seq), md5(seq * 7)) as payload from seq_1_to_3000000" >/dev/null
client sh -c "mariadb-dump --no-defaults ${dump_tls[*]} --single-transaction --routines --events --no-tablespaces e2e_bob | zstd -q > /files/big.sql.zst"
db "${new_pw}" e2e_bob.bob e2e_bob 'drop table big' >/dev/null
# a client keeps querying while the import runs, it must see the old data until the swap
( for _ in $(seq 1 200); do db "${new_pw}" e2e_bob.bob e2e_bob 'select v from imported_marker' | head -1; done ) > "${work}/during.log" 2>&1 &
reader=$!
browser bigImport
kill "${reader}" 2>/dev/null || true
wait "${reader}" 2>/dev/null || true
expect "client saw the old data during the import" "^[3-9]|^[1-9][0-9]" "$(grep -c '^from dump$' "${work}/during.log")"
expect "client interrupted only at the swap" "^[0-2]$" "$(grep -vc '^from dump$' "${work}/during.log" || true)"
expect "no staging databases left" "^0$" "$(superuser "select count(*) from information_schema.schemata where schema_name like '\\_mdb\\_%'")"
expect "no job accounts left" "^0$" "$(superuser "select count(*) from mysql.global_priv where user like '\\_mdb\\_job%'")"
import_job=$(superuser "select max(id) from mdbpanel.jobs where kind = 'import'")
steps=$(docker logs "${app}" 2>&1 | grep '"msg":"job step","id":'"${import_job}"',' | while read -r line; do yq -p json -oy '.step' <<<"${line}"; done \
    | sed -E 's/, [0-9.]+ [KMG]?i?B written$//' | uniq | paste -sd '>' -)
steps_pattern='^Checking the SHA-256 of the file>Taking a safety snapshot of the current data(>Taking a safety snapshot of the current data: table [a-z_]+)*>Creating a new database to load into, the current one stays online>Starting the mariadb client>(Loading table [a-z_]+>)*Loading table big(>Loading table [a-z_]+)*>Switching to the new tables$'
expect "import steps logged in order" "${steps_pattern}" "${steps}"
expect "big import restored all rows" "^3000000$" "$(db "${new_pw}" e2e_bob.bob e2e_bob 'select count(*) from big')"
expect "big import kept the index" "payload" "$(db "${new_pw}" e2e_bob.bob e2e_bob 'show index from big')"
browser bigSnapshot
echo "INFO 3 million rows snapshot: $(superuser "select size_bytes div 1024 from mdbpanel.snapshots order by id desc limit 1") KiB"
expect "progress recorded for the finished job" "Switching to the new tables" "$(superuser "select progress_step from mdbpanel.jobs where kind = 'import' order by id desc limit 1")"
db "${new_pw}" e2e_bob.bob e2e_bob 'drop table big' >/dev/null

browser failedImport
failed_subject='MariaDB: import of e2e_bob failed'
wait_mail bob@example.test 'Access denied' && pass "owner mailed the error of the failed import" || fail "owner mailed the error of the failed import" "$(mail_text bob@example.test | tail -20)"
wait_mail dba@example.test 'Access denied' && pass "admin address mailed the error of the failed import" || fail "admin address mailed the error of the failed import" "$(ls "${work}/mail")"
expect "failure mail subject" "^[1-9]" "$(mails bob@example.test "${failed_subject}")"
expect "group owner without opt-in not mailed" "^0$" "$(mails carol@example.test "${failed_subject}")"
expect "non-owner not mailed" "^0$" "$(mails eve@example.test 'MariaDB: import')"
for _ in $(seq 1 10); do
    [[ "$(superuser "select count(*) from mdbpanel.jobs where status = 'failed' and not notified")" == "0" ]] && break
    sleep 1
done
expect "failed jobs marked notified" "^0$" "$(superuser "select count(*) from mdbpanel.jobs where status = 'failed' and not notified")"
failure_mails=$(ls "${work}/mail" | wc -l)
expect "failed import left the data unchanged" "^from dump$" "$(db "${new_pw}" e2e_bob.bob e2e_bob 'select v from imported_marker')"
expect "failed import left no staging database" "^0$" "$(superuser "select count(*) from information_schema.schemata where schema_name like '\\_mdb\\_%'")"
expect "import loaded the dump" "^from dump$" "$(db "${new_pw}" e2e_bob.bob e2e_bob 'select v from imported_marker')"
expect "imported view belongs to the role" "^e2e_bob@?$" "$(superuser "select definer from information_schema.views where table_schema = 'e2e_bob' and table_name = 'v_t'")"
browser evilImport
expect "client commands in a dump never executed" "^no$" "$(docker exec "${app}" bash -c 'test -e /tmp/pwned -o -e /tmp/pwned2 && echo yes || echo no')"
expect "SQL around them loaded" "^evil_ok$" "$(db "${new_pw}" e2e_bob.bob e2e_bob_two 'show tables')"
upload_perms=$(docker exec "${app}" find /app/data/uploads -mindepth 1 -printf '%y %m %u:%g %p\n')
expect "upload directories setgid, cloudron:mysql" "^$" "$(grep '^d' <<<"${upload_perms}" | grep -v '^d 2750 cloudron:mysql ' || true)"
expect "panel uploads 0640 cloudron:mysql" "f 640 cloudron:mysql /app/data/uploads/e2e_bob/import_me.sql" "${upload_perms}"

echo "==> Cloudron app passwords"
readonly eve_app_pw='eve-app-password-1'
scim_users '["g-team"]' "[\"${eve_app_pw}\"]"
browser eveAppPassword
expect "eve connects with her app password" "^1$" "$(db "${eve_app_pw}" e2e_eve.eve e2e_eve 'select 1')"
expect "app password login uses TLS 1.3" "TLSv1.3" "$(db "${eve_app_pw}" e2e_eve.eve e2e_eve "show status like 'Ssl_version'")"
expect "generated password stops working" "Access denied" "$(db "${eve_pw}" e2e_eve.eve e2e_eve 'select 1')"
expect "wrong app password rejected" "Access denied" "$(db wrong-password e2e_eve.eve e2e_eve 'select 1')"
# MariaDB's PAM plugin answers an empty password with a dialog prompt the client cannot handle - plugin/auth_pam/auth_pam_v1.c roundtrip()
expect "empty app password never logs in" "Can't connect|Access denied" "$(db '' e2e_eve.eve e2e_eve 'select 1')"
expect "bob's password does not open eve's login" "Access denied" "$(db "${new_pw}" e2e_eve.eve e2e_eve 'select 1')"
expect "bob still uses his generated password" "^1$" "$(db "${new_pw}" e2e_bob.bob e2e_bob 'select 1')"
expect "app password needs TLS" "insecure transport are prohibited" "$(db "${eve_app_pw}" e2e_eve.eve e2e_eve 'select 1' '--skip-ssl')"
expect "app password not in logs" "^0$" "$(docker logs "${app}" 2>&1 | grep -c -- "${eve_app_pw}" || true)"
expect "backups keep the PAM login" "\`e2e_eve.eve\`@\`%\` IDENTIFIED VIA pam USING 'mdbpanel'" "$(docker exec "${app}" mariadb-dump --no-defaults --socket=/var/lib/mysql/run/mysqld.sock --user=root --system=users)"

superuser "update mdbpanel.uploads set first_seen = now() - interval 9 day" >/dev/null
docker restart "${app}" >/dev/null
wait_healthy
sleep 3
remaining=$(docker exec "${app}" ls /app/data/uploads/e2e_bob)
expect "expired uploads deleted" "^import_me.sql$" "${remaining}"
expect "variables kept after a restart" "^150 33554432$" "$(superuser 'select @@global.max_connections, @@global.tmp_table_size' | tr '\t' ' ')"
expect "app password works after a restart" "^1$" "$(db "${eve_app_pw}" e2e_eve.eve e2e_eve 'select 1')"
scim_users '["g-team"]' '[]'
expect "deleted app password stops working" "Access denied" "$(db "${eve_app_pw}" e2e_eve.eve e2e_eve 'select 1')"
browser eveGeneratedAgain
eve_pw=$(yq -p json -oy '.evePassword' "${work}/state.json")
expect "eve connects with the generated password again" "^1$" "$(db "${eve_pw}" e2e_eve.eve e2e_eve 'select 1')"
expect "eve no longer uses PAM" "^0$" "$(superuser "select count(*) from mysql.global_priv where user = 'e2e_eve.eve' and priv like '%pam%'")"

superuser "update mdbpanel.schedules set next_run = now() - interval 1 minute" >/dev/null
for _ in $(seq 1 60); do
    [[ "$(superuser "select count(*) from mdbpanel.jobs where scheduled and status = 'done'")" -ge 1 ]] && break
    sleep 1
done
browser bob3
expect "dropped database has no grants left" "^0$" "$(superuser 'show grants for e2e_bob' | grep -c _two || true)"
browser eve
superuser "insert into audit_log (actor, action, target) select 'e2e', 'pager.test', concat('row ', seq) from seq_1_to_60" mdbpanel >/dev/null
browser admin2
expect "deleted role and its logins gone" "^0 0$" "$(superuser "select (select count(*) from mysql.global_priv where user like 'e2e\\_eve%'), (select count(*) from mysql.db where user = 'e2e_eve')" | tr '\t' ' ')"

echo "==> Snapshot file permissions"
perms=$(docker exec "${app}" find /var/lib/mysql/snapshots -mindepth 1 -printf '%y %m %g %p\n')
expect "snapshot directories setgid, group cloudron" "^$" "$(grep '^d' <<<"${perms}" | grep -v '^d 2770 cloudron ' || true)"
expect "snapshot files 0640, group cloudron" "^$" "$(grep '^f' <<<"${perms}" | grep -v '^f 640 cloudron ' || true)"
expect "snapshot files exist" "f 640 cloudron" "${perms}"

echo "==> MARIADB_ADMINS without new login"
docker exec "${app}" sed -i 's/^export MARIADB_ADMINS=.*/export MARIADB_ADMINS="bob"/' /app/data/env.sh
docker restart "${app}" >/dev/null
wait_healthy
browser bobAdminByEnv

echo "==> Tenant timeouts do not stop snapshots"
db "${new_pw}" e2e_bob.bob e2e_bob "create table slow as select seq from seq_1_to_300000" >/dev/null
superuser "set global max_statement_time = 0.001; set session max_statement_time = 0; insert into mdbpanel.jobs (kind, db_name, requested_by) values ('snapshot', 'e2e_bob', 'test')" >/dev/null
for _ in $(seq 1 60); do
    [[ "$(superuser "set session max_statement_time = 0; select status from mdbpanel.jobs where requested_by = 'test'")" =~ done|failed ]] && break
    sleep 1
done
expect "snapshot ignores max_statement_time" "^done$" "$(superuser "set session max_statement_time = 0; select status from mdbpanel.jobs where requested_by = 'test'")"
superuser "set global max_statement_time = 0" >/dev/null
db "${new_pw}" e2e_bob.bob e2e_bob 'drop table slow' >/dev/null

echo "==> Backup and restore"
docker run --rm --network "container:${app}" --read-only --tmpfs /tmp --tmpfs /run -v mdbe2e-data:/app/data -v mdbe2e-db:/var/lib/mysql \
    "${img}" /app/pkg/backup.sh >"${work}/backup.log" 2>&1 && pass "backup.sh" || fail "backup.sh" "$(tail -5 "${work}/backup.log")"
db "${new_pw}" e2e_bob.bob e2e_bob 'insert into t values (1000)' >/dev/null
docker stop "${app}" >/dev/null
docker run --rm --read-only --tmpfs /tmp --tmpfs /run -v mdbe2e-data:/app/data -v mdbe2e-db:/var/lib/mysql \
    "${img}" /app/pkg/restore.sh >"${work}/restore.log" 2>&1 && pass "restore.sh" || fail "restore.sh" "$(tail -5 "${work}/restore.log")"
expect "restore reports no errors" "^$" "$(grep -E '^ERROR' "${work}/restore.log" || true)"
docker start "${app}" >/dev/null
wait_healthy
expect "data from backup" "^1,2,3$" "$(db "${new_pw}" e2e_bob.bob e2e_bob "select group_concat(x order by x) from t")"
expect "objects from backup" "^3 1$" "$(db "${new_pw}" e2e_bob.bob e2e_bob "select (select count(*) from v_t), f_one()" | tr '\t' ' ')"
expect "allowlist survives restore" "${app_ip}/32" "$(docker exec "${app}" cat /app/data/allowed_networks.txt)"

echo "==> Snapshot rows and files are reconciled"
gone_file=$(superuser "select file from mdbpanel.snapshots where kind = 'pre_restore' order by id limit 1")
docker exec "${app}" rm "/var/lib/mysql/snapshots/${gone_file}"
docker exec "${app}" bash -c 'cp "$(ls /var/lib/mysql/snapshots/e2e_bob/*.sql.zst | head -1)" /var/lib/mysql/snapshots/e2e_bob/orphan.sql.zst'
docker restart "${app}" >/dev/null
wait_healthy
sleep 3
expect "row of a missing snapshot file removed" "^0$" "$(superuser "select count(*) from mdbpanel.snapshots where file = '${gone_file}'")"
expect "snapshot file without row removed" "^no$" "$(docker exec "${app}" bash -c 'test -e /var/lib/mysql/snapshots/e2e_bob/orphan.sql.zst && echo yes || echo no')"
browser afterRestore

expect "no failure mail repeated after restarts" "^${failure_mails}$" "$(ls "${work}/mail" | wc -l)"

echo "==> Logs"
expect "no panel or worker errors" "^$" "$(docker logs "${app}" 2>&1 | grep '"level":"ERROR"' | grep -v 'the file changed since it was uploaded\|Access denied; you need' || true)"
expect "no MariaDB errors" "^$" "$(docker logs "${app}" 2>&1 | grep -E '\[ERROR\]' || true)"

echo
if [[ ${failures} -eq 0 ]]; then
    echo "==> All checks passed"
else
    echo "==> ${failures} failed"
    exit 1
fi
