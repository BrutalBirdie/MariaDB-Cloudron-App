package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"regexp"
	"time"
)

type Login struct {
	LoginRole string // MariaDB user name, the host is always %
	RoleName  string
	Username  string
	Auth      string // AuthPassword or AuthAppPassword
	CreatedAt time.Time
	RotatedAt time.Time
}

var plainUsername = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func validUsername(u string) bool { return u != "" && len(u) <= 255 }

// LoginName is the MariaDB user of a Cloudron user for a role, e.g.
// "shop.james". Role names never contain a dot, so login names cannot
// collide with them. MariaDB user names have up to 128 characters.
func LoginName(role, username string) string {
	name := role + "." + username
	if plainUsername.MatchString(username) && len(name) <= 128 {
		return name
	}
	sum := sha256.Sum256([]byte(username))
	return role + "." + hex.EncodeToString(sum[:4])
}

const loginColumns = `login_role, role_name, username, auth, created_at, rotated_at`

func scanLogins(rows *sql.Rows, err error) ([]Login, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Login
	for rows.Next() {
		var l Login
		if err := rows.Scan(&l.LoginRole, &l.RoleName, &l.Username, &l.Auth, &l.CreatedAt, &l.RotatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) LoginOf(ctx context.Context, role, username string) (*Login, error) {
	logins, err := scanLogins(s.db.QueryContext(ctx, `SELECT `+loginColumns+` FROM role_logins WHERE role_name = ? AND username = ?`, role, username))
	if err != nil {
		return nil, err
	}
	if len(logins) == 0 {
		return nil, ErrNotFound
	}
	return &logins[0], nil
}

// ListLogins returns the logins of one role, or of all roles when role is "".
func (s *Store) ListLogins(ctx context.Context, role string) ([]Login, error) {
	return scanLogins(s.db.QueryContext(ctx, `SELECT `+loginColumns+` FROM role_logins WHERE ? = '' OR role_name = ? ORDER BY role_name, username`, role, role))
}

// AppPasswordLogins maps every login that uses app passwords to its Cloudron username.
func (s *Store) AppPasswordLogins(ctx context.Context) (map[string]string, error) {
	logins, err := scanLogins(s.db.QueryContext(ctx, `SELECT `+loginColumns+` FROM role_logins WHERE auth = 'apppw'`))
	out := map[string]string{}
	for _, l := range logins {
		out[l.LoginRole] = l.Username
	}
	return out, err
}

func (s *Store) DeleteLoginRow(ctx context.Context, login string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM role_logins WHERE login_role = ?`, login)
	return err
}

var errNoRows = sql.ErrNoRows

func notFound(err error) error {
	if errors.Is(err, errNoRows) {
		return ErrNotFound
	}
	return err
}
