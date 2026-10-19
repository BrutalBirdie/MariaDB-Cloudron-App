package store

// One statement per entry: MariaDB commits every CREATE TABLE on its own.
var schema = []string{`
CREATE TABLE IF NOT EXISTS sessions (
    id_hash BINARY(32) NOT NULL PRIMARY KEY,
    username VARCHAR(255) NOT NULL,
    display_name VARCHAR(255) NOT NULL,
    is_admin BOOLEAN NOT NULL,
    csrf_token VARCHAR(255) NOT NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    last_seen_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
)`, `
CREATE TABLE IF NOT EXISTS oidc_logins (
    state_hash BINARY(32) NOT NULL PRIMARY KEY,
    verifier VARCHAR(255) NOT NULL,
    nonce VARCHAR(255) NOT NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
)`, `
CREATE TABLE IF NOT EXISTS audit_log (
    id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    actor VARCHAR(255) NOT NULL,
    action VARCHAR(64) NOT NULL,
    target VARCHAR(255) NOT NULL,
    detail TEXT NOT NULL DEFAULT ''
)`, `
CREATE TABLE IF NOT EXISTS role_settings (
    role_name VARCHAR(64) NOT NULL PRIMARY KEY,
    max_databases INT NOT NULL DEFAULT 0,
    conn_limit INT NOT NULL DEFAULT 20
)`, `
CREATE TABLE IF NOT EXISTS role_access (
    role_name VARCHAR(64) NOT NULL,
    kind VARCHAR(8) NOT NULL CHECK (kind IN ('user', 'group')),
    principal_id VARCHAR(255) NOT NULL,
    display VARCHAR(255) NOT NULL DEFAULT '',
    PRIMARY KEY (role_name, kind, principal_id)
)`, `
CREATE TABLE IF NOT EXISTS role_logins (
    login_role VARCHAR(128) NOT NULL PRIMARY KEY,
    role_name VARCHAR(64) NOT NULL,
    username VARCHAR(255) NOT NULL,
    auth VARCHAR(16) NOT NULL DEFAULT 'password' CHECK (auth IN ('password', 'apppw')),
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    rotated_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY (role_name, username)
)`, `
CREATE TABLE IF NOT EXISTS snapshots (
    id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    db_name VARCHAR(64) NOT NULL,
    owner_role VARCHAR(64) NOT NULL,
    kind VARCHAR(16) NOT NULL DEFAULT 'manual',
    file VARCHAR(255) NOT NULL,
    size_bytes BIGINT NOT NULL,
    note VARCHAR(1024) NOT NULL DEFAULT '',
    created_by VARCHAR(255) NOT NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    KEY (db_name)
)`, `
CREATE TABLE IF NOT EXISTS jobs (
    id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    kind VARCHAR(16) NOT NULL CHECK (kind IN ('snapshot', 'restore', 'import')),
    db_name VARCHAR(64) NOT NULL,
    snapshot_id BIGINT NULL,
    upload_id BIGINT NULL,
    scheduled BOOLEAN NOT NULL DEFAULT FALSE,
    strict BOOLEAN NOT NULL DEFAULT TRUE,
    safety_snapshot BOOLEAN NOT NULL DEFAULT TRUE,
    requested_by VARCHAR(255) NOT NULL,
    status VARCHAR(16) NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'done', 'failed')),
    progress SMALLINT NULL,
    progress_step VARCHAR(1024) NOT NULL DEFAULT '',
    error_msg TEXT NOT NULL DEFAULT '',
    notified BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    started_at TIMESTAMP(6) NULL,
    finished_at TIMESTAMP(6) NULL,
    -- one queued or running job per database, MariaDB has no partial indexes
    active_db VARCHAR(64) AS (IF(status IN ('queued', 'running'), db_name, NULL)) PERSISTENT,
    UNIQUE KEY jobs_one_active (active_db),
    KEY (db_name)
)`, `
CREATE TABLE IF NOT EXISTS schedules (
    db_name VARCHAR(64) NOT NULL PRIMARY KEY,
    owner_role VARCHAR(64) NOT NULL,
    cron VARCHAR(255) NOT NULL,
    next_run TIMESTAMP(6) NOT NULL,
    updated_by VARCHAR(255) NOT NULL,
    updated_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
)`, `
CREATE TABLE IF NOT EXISTS uploads (
    id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    role_name VARCHAR(64) NOT NULL,
    file_name VARCHAR(255) NOT NULL,
    size_bytes BIGINT NOT NULL,
    mtime TIMESTAMP(6) NULL,
    sha256 CHAR(64) NULL,
    verified_by VARCHAR(16) NOT NULL DEFAULT '',
    uploaded_by VARCHAR(255) NOT NULL,
    keep BOOLEAN NOT NULL DEFAULT FALSE,
    first_seen TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY (role_name, file_name)
)`, `
CREATE TABLE IF NOT EXISTS settings (
    name VARCHAR(64) NOT NULL PRIMARY KEY,
    value TEXT NOT NULL
)`, `
CREATE TABLE IF NOT EXISTS user_prefs (
    username VARCHAR(255) NOT NULL PRIMARY KEY,
    notify_failures BOOLEAN NOT NULL DEFAULT FALSE,
    show_labels BOOLEAN NOT NULL DEFAULT FALSE
)`}
