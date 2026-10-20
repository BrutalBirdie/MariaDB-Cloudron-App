package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrHasDatabases = errors.New("role still owns databases")
	ErrLimitReached = errors.New("database limit reached")
)

// UserError is a message meant for the person who asked for the change.
type UserError string

func (e UserError) Error() string { return string(e) }

// Database is the panel's own database, created by start.sh.
const PanelDatabase = "mdbpanel"

type Store struct {
	db *sql.DB
}

// Open connects over the unix socket with unix_socket authentication as the
// MariaDB account named like the OS user: cloudron for the panel, mysql
// (superuser) for the worker.
func Open(socket, user string) (*Store, error) {
	return OpenPassword(socket, user, "", PanelDatabase)
}

// OpenPassword connects with a password, for the worker's job accounts.
func OpenPassword(socket, user, password, database string) (*Store, error) {
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd = user, password
	cfg.Net, cfg.Addr, cfg.DBName = "unix", socket, database
	cfg.ParseTime, cfg.Loc = true, time.UTC
	// tenant timeouts set by admins must not cut off the panel, the worker or job accounts
	cfg.Params = map[string]string{"time_zone": "'+00:00'", "max_statement_time": "0", "idle_transaction_timeout": "0", "wait_timeout": "28800"}
	conn, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(conn)
	db.SetMaxOpenConns(8)
	db.SetConnMaxIdleTime(30 * time.Second)
	return &Store{db: db}, nil
}

func (s *Store) Close() { s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// CreateSchema creates the panel's tables, start.sh runs it before the panel
// and the worker start.
func (s *Store) CreateSchema(ctx context.Context) error {
	for _, stmt := range schema {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func ident(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }

func literal(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, "'", "''", "\x00", `\0`).Replace(s) + "'"
}

// account is a user account in SQL, 'user'@'host'.
func account(user, host string) string { return literal(user) + "@" + literal(host) }

// dbGrant is a database name for GRANT and REVOKE, where _ and % are wildcards.
func dbGrant(name string) string {
	return ident(strings.NewReplacer(`\`, `\\`, "_", `\_`, "%", `\%`).Replace(name))
}

// grantDB undoes dbGrant for the names stored in mysql.db.
func grantDB(stored string) string {
	return strings.NewReplacer(`\_`, "_", `\%`, "%", `\\`, `\`).Replace(stored)
}

func hash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// mysqlCode returns the server error number, 0 for other errors.
func mysqlCode(err error) uint16 {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number
	}
	return 0
}

func (s *Store) exec(ctx context.Context, stmts ...string) error {
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) column(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

type Session struct {
	Username    string
	DisplayName string
	IsAdmin     bool // stored value is the admin group membership at login
	CSRFToken   string
}

// CreateSession stores only a hash of the token, a database leak does not expose live sessions.
func (s *Store) CreateSession(ctx context.Context, token string, sess Session) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id_hash, username, display_name, is_admin, csrf_token) VALUES (?, ?, ?, ?, ?)`,
		hash(token), sess.Username, sess.DisplayName, sess.IsAdmin, sess.CSRFToken)
	return err
}

const sessionAlive = `id_hash = ? AND last_seen_at > now(6) - INTERVAL ? SECOND AND created_at > now(6) - INTERVAL ? SECOND`

func (s *Store) GetSession(ctx context.Context, token string, idle, absolute time.Duration) (*Session, error) {
	args := []any{hash(token), int(idle.Seconds()), int(absolute.Seconds())}
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = now(6) WHERE `+sessionAlive, args...)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	var sess Session
	err = s.db.QueryRowContext(ctx, `SELECT username, display_name, is_admin, csrf_token FROM sessions WHERE id_hash = ?`, hash(token)).
		Scan(&sess.Username, &sess.DisplayName, &sess.IsAdmin, &sess.CSRFToken)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &sess, err
}

// SessionValid checks a session without counting it as activity, for long
// running requests.
func (s *Store) SessionValid(ctx context.Context, token string, idle, absolute time.Duration) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE `+sessionAlive+`)`,
		hash(token), int(idle.Seconds()), int(absolute.Seconds())).Scan(&ok)
	return ok, err
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, hash(token))
	return err
}

func (s *Store) PurgeExpired(ctx context.Context, idle, absolute time.Duration) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE last_seen_at < now(6) - INTERVAL ? SECOND OR created_at < now(6) - INTERVAL ? SECOND`,
		int(idle.Seconds()), int(absolute.Seconds()))
	if err != nil {
		return err
	}
	if _, err = s.db.ExecContext(ctx, `DELETE FROM oidc_logins WHERE created_at < now(6) - INTERVAL 10 MINUTE`); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM jobs WHERE status IN ('done', 'failed') AND finished_at < now(6) - INTERVAL 30 DAY`)
	return err
}

func (s *Store) SaveOIDCLogin(ctx context.Context, state, verifier, nonce string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO oidc_logins (state_hash, verifier, nonce) VALUES (?, ?, ?)`, hash(state), verifier, nonce)
	return err
}

// TakeOIDCLogin returns and deletes a pending login, so each state can be used once.
func (s *Store) TakeOIDCLogin(ctx context.Context, state string) (verifier, nonce string, err error) {
	err = s.db.QueryRowContext(ctx, `DELETE FROM oidc_logins WHERE state_hash = ? AND created_at > now(6) - INTERVAL 10 MINUTE RETURNING verifier, nonce`,
		hash(state)).Scan(&verifier, &nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return verifier, nonce, err
}

type AuditEntry struct {
	At     time.Time
	Actor  string
	Action string
	Target string
	Detail string
}

func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_log (actor, action, target, detail) VALUES (?, ?, ?, ?)`, e.Actor, e.Action, e.Target, e.Detail)
	return err
}

// AuditQuery filters the audit log. Category is the part of the action
// before the dot, Search matches any column as a substring.
type AuditQuery struct {
	Category, Search string
	Limit, Offset    int
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func (s *Store) ListAudit(ctx context.Context, q AuditQuery) ([]AuditEntry, error) {
	search := likeEscaper.Replace(q.Search)
	rows, err := s.db.QueryContext(ctx, `SELECT created_at, actor, action, target, detail FROM audit_log
		WHERE (? = '' OR SUBSTRING_INDEX(action, '.', 1) = ?)
		  AND (? = '' OR CONCAT_WS(' ', actor, action, target, detail) LIKE CONCAT('%', ?, '%'))
		ORDER BY id DESC LIMIT ? OFFSET ?`, q.Category, q.Category, search, search, q.Limit, q.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.At, &e.Actor, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) AuditCategories(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT SUBSTRING_INDEX(action, '.', 1) AS c FROM audit_log ORDER BY c`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Setting returns a stored setting, "" when it is not set.
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE name = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetSetting stores a setting, an empty value removes it.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	if value == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE name = ?`, key)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (name, value) VALUES (?, ?) ON DUPLICATE KEY UPDATE value = VALUES(value)`, key, value)
	return err
}

// TakeSetting reports whether a flag setting was set and clears it.
func (s *Store) TakeSetting(ctx context.Context, key string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE name = ?`, key)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

const settingAccessSync = "access_sync_requested"

// RequestAccessSync asks the worker to check all logins now, for example
// after owners changed.
func (s *Store) RequestAccessSync(ctx context.Context) error {
	return s.SetSetting(ctx, settingAccessSync, time.Now().UTC().Format(time.RFC3339))
}

// TakeAccessSyncRequest reports and clears a pending request.
func (s *Store) TakeAccessSyncRequest(ctx context.Context) (bool, error) {
	return s.TakeSetting(ctx, settingAccessSync)
}
