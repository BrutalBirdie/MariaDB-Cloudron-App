package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"mdbpanel/internal/access"
	"mdbpanel/internal/mysqlpw"
	"mdbpanel/internal/names"
)

// Catalog is everything that reads or changes MariaDB accounts, grants and
// databases. Only the worker implements it with SQL (as superuser), the panel
// reaches it through the worker's socket (package rpc). The panel's own
// MariaDB account can only use the mdbpanel database: CREATE USER would
// let it change any account, including root.
type Catalog interface {
	ListRoles(ctx context.Context) ([]Role, error)
	ListDatabases(ctx context.Context) ([]Database, error)
	CreateRole(ctx context.Context, name string, connLimit int, owners []access.Principal, maxDatabases int) error
	UpdateRole(ctx context.Context, name string, owners []access.Principal, connLimit, maxDatabases int) error
	DropRole(ctx context.Context, name string) error
	CreateDatabase(ctx context.Context, name, owner string, limit int) error
	DropDatabase(ctx context.Context, name string) error
	SetLoginPassword(ctx context.Context, role, username, hash string) (string, error)
	UseAppPassword(ctx context.Context, role, username string) (string, error)
	RevokeLogin(ctx context.Context, login string) error
	DatabaseStats(ctx context.Context, name string) (map[string]DatabaseStats, error)
	Connections(ctx context.Context) ([]Connection, error)
	ServerSettings(ctx context.Context, names []string) (map[string]string, error)
}

// Tenants adds lookups on top of a Catalog.
type Tenants struct{ Catalog }

func (t Tenants) GetRole(ctx context.Context, name string) (*Role, error) {
	all, err := t.ListRoles(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range all {
		if r.Name == name {
			return &r, nil
		}
	}
	return nil, ErrNotFound
}

// RolesOwnedBy lists the roles the member owns by name or through a group.
func (t Tenants) RolesOwnedBy(ctx context.Context, m access.Member) ([]Role, error) {
	all, err := t.ListRoles(ctx)
	if err != nil {
		return nil, err
	}
	var mine []Role
	for _, r := range all {
		if access.Owns(r.Owners, m) {
			mine = append(mine, r)
		}
	}
	return mine, nil
}

func (t Tenants) GetDatabase(ctx context.Context, name string) (*Database, error) {
	all, err := t.ListDatabases(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range all {
		if d.Name == name {
			return &d, nil
		}
	}
	return nil, ErrNotFound
}

type Role struct {
	Name         string
	ConnLimit    int // applied to every login of the role
	Owners       []access.Principal
	MaxDatabases int // databases the owners may have in total via self-service, 0 disables it
	Databases    []string
}

func (r Role) OwnerUsers() []access.Principal  { return r.ownersOf(access.KindUser) }
func (r Role) OwnerGroups() []access.Principal { return r.ownersOf(access.KindGroup) }

func (r Role) ownersOf(kind string) []access.Principal {
	var out []access.Principal
	for _, p := range r.Owners {
		if p.Kind == kind {
			out = append(out, p)
		}
	}
	return out
}

type Database struct {
	Name  string
	Owner string // tenant role with ALL PRIVILEGES on it
}

// tenantRoles are the MariaDB roles granted the marker role tenants. Roles
// granted to roles have an empty host in mysql.roles_mapping.
func (s *Store) tenantRoles(ctx context.Context) ([]string, error) {
	return s.column(ctx, `SELECT User FROM mysql.roles_mapping WHERE Role = 'tenants' AND Host = '' ORDER BY User`)
}

func (s *Store) isTenant(ctx context.Context, role string) (bool, error) {
	if err := names.Validate(role); err != nil {
		return false, err
	}
	roles, err := s.tenantRoles(ctx)
	return slices.Contains(roles, role), err
}

func (s *Store) roleOwners(ctx context.Context) (map[string][]access.Principal, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT role_name, kind, principal_id, display FROM role_access ORDER BY kind DESC, display, principal_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	owners := map[string][]access.Principal{}
	for rows.Next() {
		var role string
		var p access.Principal
		if err := rows.Scan(&role, &p.Kind, &p.ID, &p.Display); err != nil {
			return nil, err
		}
		owners[role] = append(owners[role], p)
	}
	return owners, rows.Err()
}

func (s *Store) ListRoles(ctx context.Context) ([]Role, error) {
	tenants, err := s.tenantRoles(ctx)
	if err != nil {
		return nil, err
	}
	owners, err := s.roleOwners(ctx)
	if err != nil {
		return nil, err
	}
	dbs, err := s.ListDatabases(ctx)
	if err != nil {
		return nil, err
	}
	settings := map[string][2]int{}
	rows, err := s.db.QueryContext(ctx, `SELECT role_name, max_databases, conn_limit FROM role_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		var v [2]int
		if err := rows.Scan(&n, &v[0], &v[1]); err != nil {
			return nil, err
		}
		settings[n] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []Role
	for _, name := range tenants {
		r := Role{Name: name, Owners: owners[name], ConnLimit: 20}
		if v, ok := settings[name]; ok {
			r.MaxDatabases, r.ConnLimit = v[0], v[1]
		}
		for _, d := range dbs {
			if d.Owner == name {
				r.Databases = append(r.Databases, d.Name)
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// ListDatabases returns the existing databases a tenant role has a database
// level grant on. Internal databases start with an underscore and are left out.
func (s *Store) ListDatabases(ctx context.Context) ([]Database, error) {
	tenants, err := s.tenantRoles(ctx)
	if err != nil {
		return nil, err
	}
	existing, err := s.column(ctx, `SELECT SCHEMA_NAME FROM information_schema.SCHEMATA`)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT Db, User FROM mysql.db WHERE Host = '' ORDER BY Db`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Database
	seen := map[string]bool{}
	for rows.Next() {
		var stored, user string
		if err := rows.Scan(&stored, &user); err != nil {
			return nil, err
		}
		name := grantDB(stored)
		if seen[name] || !slices.Contains(tenants, user) || !slices.Contains(existing, name) || names.Validate(name) != nil {
			continue
		}
		seen[name] = true
		out = append(out, Database{Name: name, Owner: user})
	}
	return out, rows.Err()
}

func (s *Store) setOwners(ctx context.Context, tx *sql.Tx, role string, owners []access.Principal, connLimit, maxDatabases int) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM role_access WHERE role_name = ?`, role); err != nil {
		return err
	}
	for _, p := range owners {
		if _, err := tx.ExecContext(ctx, `INSERT IGNORE INTO role_access (role_name, kind, principal_id, display) VALUES (?, ?, ?, ?)`,
			role, p.Kind, p.ID, p.Display); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO role_settings (role_name, max_databases, conn_limit) VALUES (?, ?, ?)
ON DUPLICATE KEY UPDATE max_databases = VALUES(max_databases), conn_limit = VALUES(conn_limit)`, role, maxDatabases, connLimit)
	return err
}

func (s *Store) saveOwners(ctx context.Context, role string, owners []access.Principal, connLimit, maxDatabases int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.setOwners(ctx, tx, role, owners, connLimit, maxDatabases); err != nil {
		return err
	}
	return tx.Commit()
}

func validLimits(connLimit, maxDatabases int) error {
	if connLimit < 1 || connLimit > 1000 || maxDatabases < 0 || maxDatabases > 100 {
		return UserError("connection limit or database limit out of range")
	}
	return nil
}

// CreateRole creates a tenant role. Nobody connects as the role itself:
// people get their own login (SetLoginPassword) with the role as default role.
func (s *Store) CreateRole(ctx context.Context, name string, connLimit int, owners []access.Principal, maxDatabases int) error {
	if err := names.Validate(name); err != nil {
		return err
	}
	if err := validLimits(connLimit, maxDatabases); err != nil {
		return err
	}
	if err := s.exec(ctx, `CREATE ROLE `+ident(name)); err != nil {
		return err
	}
	err := s.exec(ctx, `GRANT tenants TO `+ident(name))
	if err == nil {
		err = s.saveOwners(ctx, name, owners, connLimit, maxDatabases)
	}
	if err != nil {
		return errors.Join(err, s.exec(context.WithoutCancel(ctx), `DROP ROLE `+ident(name)))
	}
	return nil
}

// UpdateRole replaces owners and limits. The connection limit applies to every
// login of the role; the worker revokes logins of users who lost access.
func (s *Store) UpdateRole(ctx context.Context, name string, owners []access.Principal, connLimit, maxDatabases int) error {
	if ok, err := s.isTenant(ctx, name); err != nil || !ok {
		return errors.Join(err, ErrNotFound)
	}
	if err := validLimits(connLimit, maxDatabases); err != nil {
		return err
	}
	logins, err := s.ListLogins(ctx, name)
	if err != nil {
		return err
	}
	for _, l := range logins {
		if err := s.exec(ctx, fmt.Sprintf(`ALTER USER IF EXISTS %s WITH MAX_USER_CONNECTIONS %d`, account(l.LoginRole, "%"), connLimit)); err != nil {
			return err
		}
	}
	if err := s.saveOwners(ctx, name, owners, connLimit, maxDatabases); err != nil {
		return err
	}
	return s.RequestAccessSync(ctx)
}

// syncLoginGrants gives every login of a role, locked ones too, the role's
// database privileges directly: MariaDB does not use roles for the definer
// of views, triggers and routines, and objects created without DEFINER name
// the login.
func (s *Store) syncLoginGrants(ctx context.Context, role string) error {
	accounts, err := s.roleAccounts(ctx, role)
	if err != nil {
		return err
	}
	all, err := s.ListDatabases(ctx)
	if err != nil {
		return err
	}
	for _, d := range all {
		if d.Owner != role {
			continue
		}
		for _, a := range accounts {
			if err := s.exec(ctx, `GRANT ALL PRIVILEGES ON `+dbGrant(d.Name)+`.* TO `+account(a, "%")); err != nil {
				return err
			}
		}
	}
	return nil
}

// roleAccounts lists the login accounts the role is granted to, including
// locked logins of former owners.
func (s *Store) roleAccounts(ctx context.Context, role string) ([]string, error) {
	return s.column(ctx, `SELECT User FROM mysql.roles_mapping WHERE Role = ? AND Host = '%'`, role)
}

func (s *Store) DropRole(ctx context.Context, name string) error {
	if ok, err := s.isTenant(ctx, name); err != nil || !ok {
		return errors.Join(err, ErrNotFound)
	}
	var dbs []string
	all, err := s.ListDatabases(ctx)
	if err != nil {
		return err
	}
	for _, d := range all {
		if d.Owner == name {
			dbs = append(dbs, d.Name)
		}
	}
	if len(dbs) > 0 {
		return fmt.Errorf("%w: %s", ErrHasDatabases, strings.Join(dbs, ", "))
	}
	accounts, err := s.roleAccounts(ctx, name)
	if err != nil {
		return err
	}
	// the role owns no database, so its logins are definer of nothing
	for _, a := range accounts {
		if err := s.killSessions(ctx, `USER = ?`, a); err != nil {
			return err
		}
		if err := s.exec(ctx, `DROP USER IF EXISTS `+account(a, "%")); err != nil {
			return err
		}
	}
	if err := s.revokeAll(ctx, name); err != nil {
		return err
	}
	if err := s.exec(ctx, `DROP ROLE `+ident(name)); err != nil {
		return err
	}
	for _, q := range []string{`DELETE FROM role_settings WHERE role_name = ?`, `DELETE FROM role_access WHERE role_name = ?`, `DELETE FROM role_logins WHERE role_name = ?`} {
		if _, err := s.db.ExecContext(ctx, q, name); err != nil {
			return err
		}
	}
	return nil
}

// revokeAll removes the database level grants left for a role, also those of
// databases dropped outside the panel.
func (s *Store) revokeAll(ctx context.Context, role string) error {
	stored, err := s.column(ctx, `SELECT Db FROM mysql.db WHERE User = ? AND Host = ''`, role)
	if err != nil {
		return err
	}
	for _, db := range stored {
		if err := s.exec(ctx, `REVOKE ALL PRIVILEGES ON `+ident(db)+`.* FROM `+ident(role)); err != nil {
			return err
		}
	}
	return nil
}

// revokeDatabase removes every database level grant on a database name, so a
// new database with the name never inherits access.
func (s *Store) revokeDatabase(ctx context.Context, name string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT Db, User, Host FROM mysql.db`)
	if err != nil {
		return err
	}
	type grant struct{ db, user, host string }
	var grants []grant
	for rows.Next() {
		var g grant
		if err := rows.Scan(&g.db, &g.user, &g.host); err != nil {
			rows.Close()
			return err
		}
		if grantDB(g.db) == name {
			grants = append(grants, g)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, g := range grants {
		to := ident(g.user)
		if g.host != "" {
			to = account(g.user, g.host)
		}
		if err := s.exec(ctx, `REVOKE ALL PRIVILEGES ON `+ident(g.db)+`.* FROM `+to); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) killSessions(ctx context.Context, where string, args ...any) error {
	ids, err := s.column(ctx, `SELECT ID FROM information_schema.PROCESSLIST WHERE ID <> CONNECTION_ID() AND `+where, args...)
	if err != nil {
		return err
	}
	for _, id := range ids {
		// the session may have ended on its own in between
		if err := s.exec(ctx, `KILL CONNECTION `+id); err != nil && mysqlCode(err) != 1094 {
			return err
		}
	}
	return nil
}

// CreateDatabase creates a database for a tenant role. With limit > 0 it
// fails when the role already has limit databases.
func (s *Store) CreateDatabase(ctx context.Context, name, owner string, limit int) error {
	if err := names.Validate(name); err != nil {
		return err
	}
	if ok, err := s.isTenant(ctx, owner); err != nil || !ok {
		return fmt.Errorf("owner role: %w", errors.Join(err, ErrNotFound))
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var locked sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK('mdbpanel.create_database', 30)`).Scan(&locked); err != nil || locked.Int64 != 1 {
		return errors.Join(err, errors.New("another database is being created, try again"))
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), `SELECT RELEASE_LOCK('mdbpanel.create_database')`)
	if limit > 0 {
		all, err := s.ListDatabases(ctx)
		if err != nil {
			return err
		}
		owned := 0
		for _, d := range all {
			if d.Owner == owner {
				owned++
			}
		}
		if owned >= limit {
			return fmt.Errorf("%w: role %s already has %d of %d databases", ErrLimitReached, owner, owned, limit)
		}
	}
	if err := s.revokeDatabase(ctx, name); err != nil {
		return err
	}
	if err := s.exec(ctx, `CREATE DATABASE `+ident(name)); err != nil {
		return err
	}
	if err := s.exec(ctx, `GRANT ALL PRIVILEGES ON `+dbGrant(name)+`.* TO `+ident(owner)); err != nil {
		return errors.Join(err, s.exec(context.WithoutCancel(ctx), `DROP DATABASE `+ident(name)))
	}
	return s.syncLoginGrants(ctx, owner)
}

func (s *Store) DropDatabase(ctx context.Context, name string) error {
	if err := names.Validate(name); err != nil {
		return err
	}
	if _, err := (Tenants{s}).GetDatabase(ctx, name); err != nil {
		return err
	}
	// grants first, so nobody connects again while the sessions end
	if err := s.revokeDatabase(ctx, name); err != nil {
		return err
	}
	if err := s.killSessions(ctx, `DB = ?`, name); err != nil {
		return err
	}
	return s.exec(ctx, `DROP DATABASE `+ident(name))
}

type DatabaseStats struct {
	SizeBytes     int64
	Connections   int
	Snapshots     int
	SnapshotBytes int64
	LastSnapshot  *time.Time
}

// DatabaseStats returns sizes and snapshot totals of one database, or of all
// tenant databases when name is "".
func (s *Store) DatabaseStats(ctx context.Context, name string) (map[string]DatabaseStats, error) {
	dbs, err := s.ListDatabases(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]DatabaseStats{}
	for _, d := range dbs {
		if name == "" || d.Name == name {
			out[d.Name] = DatabaseStats{}
		}
	}
	if len(out) == 0 {
		return out, nil
	}
	filter, args := ``, []any{}
	if name != "" {
		filter, args = ` WHERE TABLE_SCHEMA = ?`, []any{name}
	}
	scan := func(query string, fn func(rows *sql.Rows) error, args ...any) error {
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := fn(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	err = scan(`SELECT TABLE_SCHEMA, COALESCE(SUM(DATA_LENGTH + INDEX_LENGTH), 0) FROM information_schema.TABLES`+filter+` GROUP BY TABLE_SCHEMA`,
		func(rows *sql.Rows) error {
			var db string
			var size int64
			if err := rows.Scan(&db, &size); err != nil {
				return err
			}
			if st, ok := out[db]; ok {
				st.SizeBytes = size
				out[db] = st
			}
			return nil
		}, args...)
	if err != nil {
		return nil, err
	}
	err = scan(`SELECT DB, COUNT(*) FROM information_schema.PROCESSLIST WHERE DB IS NOT NULL AND USER LIKE '%.%' GROUP BY DB`,
		func(rows *sql.Rows) error {
			var db string
			var n int
			if err := rows.Scan(&db, &n); err != nil {
				return err
			}
			if st, ok := out[db]; ok {
				st.Connections = n
				out[db] = st
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	err = scan(`SELECT db_name, COUNT(*), COALESCE(SUM(size_bytes), 0), MAX(created_at) FROM snapshots GROUP BY db_name`,
		func(rows *sql.Rows) error {
			var db string
			var st DatabaseStats
			var last sql.NullTime
			if err := rows.Scan(&db, &st.Snapshots, &st.SnapshotBytes, &last); err != nil {
				return err
			}
			if cur, ok := out[db]; ok {
				cur.Snapshots, cur.SnapshotBytes = st.Snapshots, st.SnapshotBytes
				if last.Valid {
					cur.LastSnapshot = &last.Time
				}
				out[db] = cur
			}
			return nil
		})
	return out, err
}

// Login authentication methods, see role_logins.auth.
const (
	AuthPassword    = "password"
	AuthAppPassword = "apppw"
)

// pamService is the PAM service app password logins authenticate with, see /etc/pam.d/mdbpanel.
const pamService = "mdbpanel"

// SetLoginPassword creates the user's login for a role or gives it a new
// password. hash is the mysql_native_password hash, the plaintext stays in
// the panel. A login that used app passwords goes back to a password.
func (s *Store) SetLoginPassword(ctx context.Context, role, username, hash string) (string, error) {
	if !mysqlpw.ValidHash(hash) {
		return "", errors.New("invalid password hash")
	}
	return s.setLoginAuth(ctx, role, username, AuthPassword, `IDENTIFIED BY PASSWORD `+literal(hash))
}

// UseAppPassword makes the user's login for a role authenticate with Cloudron
// app passwords only, through PAM. Its password is removed.
func (s *Store) UseAppPassword(ctx context.Context, role, username string) (string, error) {
	return s.setLoginAuth(ctx, role, username, AuthAppPassword, `IDENTIFIED VIA pam USING `+literal(pamService))
}

func (s *Store) setLoginAuth(ctx context.Context, role, username, auth, identified string) (string, error) {
	if !validUsername(username) {
		return "", UserError("invalid Cloudron username")
	}
	r, err := (Tenants{s}).GetRole(ctx, role)
	if err != nil {
		return "", err
	}
	login := LoginName(role, username)
	if existing, err := s.LoginOf(ctx, role, username); err == nil {
		login = existing.LoginRole
	} else if !errors.Is(err, ErrNotFound) {
		return "", err
	}
	acct := account(login, "%")
	// an existing account is the locked login of a former owner, it is reused
	if err := s.exec(ctx,
		fmt.Sprintf(`CREATE USER IF NOT EXISTS %s %s REQUIRE SSL WITH MAX_USER_CONNECTIONS %d ACCOUNT LOCK`, acct, identified, r.ConnLimit),
		fmt.Sprintf(`ALTER USER %s %s REQUIRE SSL WITH MAX_USER_CONNECTIONS %d`, acct, identified, r.ConnLimit),
		`GRANT `+ident(role)+` TO `+acct,
		`SET DEFAULT ROLE `+ident(role)+` FOR `+acct,
		`ALTER USER `+acct+` ACCOUNT UNLOCK`,
	); err != nil {
		return "", err
	}
	if err := s.syncLoginGrants(ctx, role); err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO role_logins (login_role, role_name, username, auth) VALUES (?, ?, ?, ?)
ON DUPLICATE KEY UPDATE auth = VALUES(auth), rotated_at = now(6)`, login, role, username, auth)
	return login, err
}

// RevokeLogin removes a login at once, see RemoveLogin.
func (s *Store) RevokeLogin(ctx context.Context, login string) error {
	var role string
	err := s.db.QueryRowContext(ctx, `SELECT role_name FROM role_logins WHERE login_role = ?`, login).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.RemoveLogin(ctx, login, role)
}

// RemoveLogin locks a login and ends its sessions. The account stays with
// a random password: views, triggers and routines its user created name it as
// definer and keep working. Logging in again needs a new password from the panel.
func (s *Store) RemoveLogin(ctx context.Context, login, role string) error {
	if !strings.HasPrefix(login, role+".") || names.Validate(role) != nil {
		return fmt.Errorf("%q is not a login of role %q", login, role)
	}
	random := make([]byte, 32)
	rand.Read(random)
	acct := account(login, "%")
	if err := s.exec(ctx, `ALTER USER IF EXISTS `+acct+` IDENTIFIED BY PASSWORD `+literal(mysqlpw.NativeHash(hex.EncodeToString(random)))+` ACCOUNT LOCK`); err != nil {
		return err
	}
	if err := s.killSessions(ctx, `USER = ?`, login); err != nil {
		return err
	}
	return s.DeleteLoginRow(ctx, login)
}

type Connection struct {
	ID          int64
	Database    string
	Login       string
	ClientAddr  string // empty for socket connections
	Command     string // Sleep, Query, ...
	State       string
	StateChange time.Time
}

// Connections lists the sessions of tenant logins. Query text is left out,
// it can contain passwords or tenant data.
func (s *Store) Connections(ctx context.Context) ([]Connection, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ID, COALESCE(DB, ''), USER, HOST, COMMAND, COALESCE(STATE, ''), TIME
FROM information_schema.PROCESSLIST WHERE USER LIKE '%.%' ORDER BY DB, ID`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Connection
	now := time.Now()
	for rows.Next() {
		var c Connection
		var host string
		var secs int64
		if err := rows.Scan(&c.ID, &c.Database, &c.Login, &host, &c.Command, &c.State, &secs); err != nil {
			return nil, err
		}
		if h, _, ok := strings.Cut(host, ":"); ok && !strings.HasPrefix(host, "[") {
			host = h
		} else if strings.HasPrefix(host, "[") {
			host, _, _ = strings.Cut(strings.TrimPrefix(host, "["), "]")
		}
		if host != "localhost" {
			c.ClientAddr = host
		}
		c.StateChange = now.Add(-time.Duration(secs) * time.Second)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ServerSettings returns the running values of global variables.
func (s *Store) ServerSettings(ctx context.Context, vars []string) (map[string]string, error) {
	out := map[string]string{}
	if len(vars) == 0 {
		return out, nil
	}
	args := make([]any, len(vars))
	for i, v := range vars {
		args[i] = strings.ToUpper(v)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT LOWER(VARIABLE_NAME), VARIABLE_VALUE FROM information_schema.GLOBAL_VARIABLES WHERE VARIABLE_NAME IN (?`+strings.Repeat(", ?", len(vars)-1)+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n, v string
		if err := rows.Scan(&n, &v); err != nil {
			return nil, err
		}
		out[n] = v
	}
	return out, rows.Err()
}

// SetGlobal sets a server variable for the running server. Only the worker
// calls it, value must be digits.
func (s *Store) SetGlobal(ctx context.Context, name, value string) error {
	if strings.Trim(value, "0123456789") != "" || strings.Trim(name, "abcdefghijklmnopqrstuvwxyz_") != "" {
		return fmt.Errorf("refusing to set %q to %q", name, value)
	}
	return s.exec(ctx, `SET GLOBAL `+name+` = `+value)
}
