package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"mdbpanel/internal/mysqlpw"
	"mdbpanel/internal/names"
	"mdbpanel/internal/sqlfilter"
)

// Databases and accounts with this prefix are internal: staging databases a
// restore loads into, replaced databases waiting to be dropped and the
// accounts jobs run as. Tenant names start with a letter, so they never match.
const InternalPrefix = "_mdb_"

func StagingName(jobID int64) string  { return fmt.Sprintf("%sstaging_%d", InternalPrefix, jobID) }
func replacedName(jobID int64) string { return fmt.Sprintf("%sreplaced_%d", InternalPrefix, jobID) }
func JobUser(jobID int64) string      { return fmt.Sprintf("%sjob_%d", InternalPrefix, jobID) }

// CreateJobAccount creates the local account a job runs mariadb-dump and the
// mariadb client as. It has the tenant role as default role and nothing
// else, so dump content never runs with the worker's superuser rights.
func (s *Store) CreateJobAccount(ctx context.Context, jobID int64, role string) (user, password string, err error) {
	if err := names.Validate(role); err != nil {
		return "", "", err
	}
	b := make([]byte, 24)
	rand.Read(b)
	user, password = JobUser(jobID), hex.EncodeToString(b)
	acct := account(user, "localhost")
	err = s.exec(ctx,
		`DROP USER IF EXISTS `+acct,
		`CREATE USER `+acct+` IDENTIFIED BY PASSWORD `+literal(mysqlpw.NativeHash(password)),
		`GRANT `+ident(role)+` TO `+acct,
		`SET DEFAULT ROLE `+ident(role)+` FOR `+acct,
	)
	return user, password, err
}

func (s *Store) DropJobAccount(ctx context.Context, jobID int64) error {
	if err := s.killSessions(ctx, `USER = ?`, JobUser(jobID)); err != nil {
		return err
	}
	return s.exec(ctx, `DROP USER IF EXISTS `+account(JobUser(jobID), "localhost"))
}

// CreateStaging creates an empty internal database with the character set of
// database like, and gives the tenant role access to it.
func (s *Store) CreateStaging(ctx context.Context, name, like, owner string) error {
	if !strings.HasPrefix(name, InternalPrefix) {
		return fmt.Errorf("%q is not an internal database", name)
	}
	if err := names.Validate(owner); err != nil {
		return err
	}
	var charset, collation string
	err := s.db.QueryRowContext(ctx, `SELECT DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?`, like).Scan(&charset, &collation)
	if err != nil {
		return notFound(err)
	}
	if err := s.DropInternal(ctx, name); err != nil {
		return err
	}
	return s.exec(ctx,
		`CREATE DATABASE `+ident(name)+` CHARACTER SET `+literal(charset)+` COLLATE `+literal(collation),
		`GRANT ALL PRIVILEGES ON `+dbGrant(name)+`.* TO `+ident(owner))
}

func (s *Store) DropInternal(ctx context.Context, name string) error {
	if !strings.HasPrefix(name, InternalPrefix) {
		return fmt.Errorf("%q is not an internal database", name)
	}
	if err := s.revokeDatabase(ctx, name); err != nil {
		return err
	}
	if err := s.killSessions(ctx, `DB = ?`, name); err != nil {
		return err
	}
	return s.exec(ctx, `DROP DATABASE IF EXISTS `+ident(name))
}

// DropAllInternal removes staging databases, replaced databases and job
// accounts left by an interrupted worker.
func (s *Store) DropAllInternal(ctx context.Context) error {
	dbs, err := s.column(ctx, `SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE '\_mdb\_%'`)
	if err != nil {
		return err
	}
	stored, err := s.column(ctx, `SELECT Db FROM mysql.db`)
	if err != nil {
		return err
	}
	for _, d := range stored {
		if name := grantDB(d); strings.HasPrefix(name, InternalPrefix) {
			dbs = append(dbs, name)
		}
	}
	for _, d := range dbs {
		if err := s.DropInternal(ctx, d); err != nil {
			return err
		}
	}
	users, err := s.column(ctx, `SELECT User FROM mysql.global_priv WHERE Host = 'localhost' AND User LIKE '\_mdb\_job\_%'`)
	if err != nil {
		return err
	}
	for _, u := range users {
		if err := s.exec(ctx, `DROP USER IF EXISTS `+account(u, "localhost")); err != nil {
			return err
		}
	}
	return nil
}

type TableSize struct {
	Bytes   int64
	EstRows int64
}

// TableSizes returns the data size of every table in a database.
func (s *Store) TableSizes(ctx context.Context, database string) (map[string]TableSize, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT TABLE_NAME, COALESCE(DATA_LENGTH, 0), COALESCE(TABLE_ROWS, 0) FROM information_schema.TABLES
WHERE TABLE_SCHEMA = ? AND TABLE_TYPE IN ('BASE TABLE', 'SYSTEM VERSIONED')`, database)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]TableSize{}
	for rows.Next() {
		var n string
		var t TableSize
		if err := rows.Scan(&n, &t.Bytes, &t.EstRows); err != nil {
			return nil, err
		}
		out[n] = t
	}
	return out, rows.Err()
}

// object is a view, trigger, routine or event with the statement that creates it.
type object struct {
	kind, name, ddl, sqlMode, timeZone string
}

func (o object) drop(db string) string {
	return "DROP " + o.kind + " IF EXISTS " + ident(db) + "." + ident(o.name)
}

// showCreate runs a SHOW CREATE statement and returns its columns by name.
func (s *Store) showCreate(ctx context.Context, query string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		return nil, errors.Join(rows.Err(), ErrNotFound)
	}
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i, c := range cols {
		out[c] = vals[i].String
	}
	return out, nil
}

// objects reads everything in a database that is not a table, in an order
// that can be created again: views, then triggers, routines and events.
func (s *Store) objects(ctx context.Context, db string) ([]object, error) {
	var out []object
	q := ident(db) + "."
	add := func(kind, list, show, ddlCol string) error {
		found, err := s.column(ctx, list, db)
		if err != nil {
			return err
		}
		for _, name := range found {
			sc, err := s.showCreate(ctx, show+" "+q+ident(name))
			if err != nil {
				return fmt.Errorf("%s %s: %w", strings.ToLower(kind), name, err)
			}
			out = append(out, object{kind: kind, name: name, ddl: sc[ddlCol], sqlMode: sc["sql_mode"], timeZone: sc["time_zone"]})
		}
		return nil
	}
	steps := []struct{ kind, list, show, col string }{
		{"VIEW", `SELECT TABLE_NAME FROM information_schema.VIEWS WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME`, "SHOW CREATE VIEW", "Create View"},
		{"TRIGGER", `SELECT TRIGGER_NAME FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ? ORDER BY EVENT_OBJECT_TABLE, EVENT_MANIPULATION, ACTION_TIMING, ACTION_ORDER`, "SHOW CREATE TRIGGER", "SQL Original Statement"},
		{"PROCEDURE", `SELECT ROUTINE_NAME FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = ? AND ROUTINE_TYPE = 'PROCEDURE' ORDER BY ROUTINE_NAME`, "SHOW CREATE PROCEDURE", "Create Procedure"},
		{"FUNCTION", `SELECT ROUTINE_NAME FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = ? AND ROUTINE_TYPE = 'FUNCTION' ORDER BY ROUTINE_NAME`, "SHOW CREATE FUNCTION", "Create Function"},
		{"EVENT", `SELECT EVENT_NAME FROM information_schema.EVENTS WHERE EVENT_SCHEMA = ? ORDER BY EVENT_NAME`, "SHOW CREATE EVENT", "Create Event"},
	}
	for _, st := range steps {
		if err := add(st.kind, st.list, st.show, st.col); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) baseTables(ctx context.Context, db string) ([]string, error) {
	return s.column(ctx, `SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_TYPE IN ('BASE TABLE', 'SEQUENCE', 'SYSTEM VERSIONED') ORDER BY TABLE_NAME`, db)
}

// SwapStaging replaces the contents of database live with the loaded staging
// database of a job. The tables move with one RENAME TABLE statement, so
// clients see either the old or the new tables. Views, triggers, routines and
// events are created again in live afterwards, as the job account (as) with
// the tenant role as definer. The superuser only reads definitions here.
func (s *Store) SwapStaging(ctx context.Context, live string, jobID int64, as *Store) error {
	staging, replaced := StagingName(jobID), replacedName(jobID)
	db, err := (Tenants{s}).GetDatabase(ctx, live)
	if err != nil {
		return err
	}
	newObjs, err := s.objects(ctx, staging)
	if err != nil {
		return err
	}
	oldObjs, err := s.objects(ctx, live)
	if err != nil {
		return err
	}
	newTables, err := s.baseTables(ctx, staging)
	if err != nil {
		return err
	}
	oldTables, err := s.baseTables(ctx, live)
	if err != nil {
		return err
	}
	if err := s.CreateStaging(ctx, replaced, live, db.Owner); err != nil {
		return err
	}
	defer s.DropInternal(context.WithoutCancel(ctx), replaced)

	conn, err := as.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	run := func(stmts ...string) error {
		for _, st := range stmts {
			if _, err := conn.ExecContext(ctx, st); err != nil {
				return fmt.Errorf("%s: %w", st, err)
			}
		}
		return nil
	}
	if err := run("SET SESSION lock_wait_timeout = 60", "SET SESSION foreign_key_checks = 0"); err != nil {
		return err
	}
	if err := s.killSessions(ctx, `DB = ? AND USER <> ?`, live, JobUser(jobID)); err != nil {
		return err
	}
	// triggers keep a table from moving to another database, views cannot move at all
	var drops []string
	for _, o := range oldObjs {
		drops = append(drops, o.drop(live))
	}
	for _, o := range newObjs {
		drops = append(drops, o.drop(staging))
	}
	if err := run(drops...); err != nil {
		return errors.Join(err, createObjects(ctx, conn, live, oldObjs, "", ""))
	}
	var moves []string
	for _, t := range oldTables {
		moves = append(moves, ident(live)+"."+ident(t)+" TO "+ident(replaced)+"."+ident(t))
	}
	for _, t := range newTables {
		moves = append(moves, ident(staging)+"."+ident(t)+" TO "+ident(live)+"."+ident(t))
	}
	if len(moves) > 0 {
		if err := run("RENAME TABLE " + strings.Join(moves, ", ")); err != nil {
			return errors.Join(err, createObjects(ctx, conn, live, oldObjs, "", ""))
		}
	}
	if err := createObjects(ctx, conn, live, newObjs, staging, live); err != nil {
		return Warning{Msg: "the tables were replaced, but " + err.Error()}
	}
	return nil
}

// createObjects creates objects in db. View definitions that name database
// from are pointed to database to. Views can depend on each other, they are
// retried until no more can be created.
func createObjects(ctx context.Context, conn *sql.Conn, db string, objs []object, from, to string) error {
	if _, err := conn.ExecContext(ctx, "USE "+ident(db)); err != nil {
		return err
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), "SET SESSION sql_mode = DEFAULT, time_zone = '+00:00'")
	create := func(o object) error {
		ddl, ok := sqlfilter.RewriteDefiner(o.ddl)
		if !ok {
			return fmt.Errorf("%s %s has no definer", strings.ToLower(o.kind), o.name)
		}
		if from != "" {
			ddl = strings.ReplaceAll(ddl, ident(from)+".", ident(to)+".")
		}
		if _, err := conn.ExecContext(ctx, "SET SESSION sql_mode = ?", o.sqlMode); err != nil {
			return err
		}
		if o.timeZone != "" {
			if _, err := conn.ExecContext(ctx, "SET SESSION time_zone = ?", o.timeZone); err != nil {
				return err
			}
		}
		// plain text protocol, CREATE TRIGGER and CREATE PROCEDURE cannot be prepared
		_, err := conn.ExecContext(ctx, ddl)
		return err
	}
	var failed []string
	var pending []object
	for _, o := range objs {
		if o.kind == "VIEW" {
			pending = append(pending, o)
		}
	}
	for len(pending) > 0 {
		var next []object
		var lastErr error
		for _, o := range pending {
			if err := create(o); err != nil {
				next, lastErr = append(next, o), err
			}
		}
		if len(next) == len(pending) {
			for _, o := range next {
				failed = append(failed, fmt.Sprintf("view %s", o.name))
			}
			failed[len(failed)-1] += fmt.Sprintf(" (%v)", lastErr)
			break
		}
		pending = next
	}
	for _, o := range objs {
		if o.kind == "VIEW" {
			continue
		}
		if err := create(o); err != nil {
			failed = append(failed, fmt.Sprintf("%s %s (%v)", strings.ToLower(o.kind), o.name, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("these could not be created: %s", strings.Join(failed, ", "))
	}
	return nil
}
