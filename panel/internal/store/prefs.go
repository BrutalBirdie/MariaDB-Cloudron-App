package store

import (
	"context"
	"database/sql"
	"errors"
)

// Prefs are per Cloudron user and apply to every role the user owns.
type Prefs struct {
	NotifyFailures bool // mail when a job of a database of an owned role fails
	ShowLabels     bool // text next to every icon button
}

// Prefs returns the user's preferences, the zero value when none are stored.
func (s *Store) Prefs(ctx context.Context, username string) (Prefs, error) {
	var p Prefs
	err := s.db.QueryRowContext(ctx, `SELECT notify_failures, show_labels FROM user_prefs WHERE username = ?`, username).Scan(&p.NotifyFailures, &p.ShowLabels)
	if errors.Is(err, sql.ErrNoRows) {
		return Prefs{}, nil
	}
	return p, err
}

func (s *Store) SetPrefs(ctx context.Context, username string, p Prefs) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO user_prefs (username, notify_failures, show_labels) VALUES (?, ?, ?)
ON DUPLICATE KEY UPDATE notify_failures = VALUES(notify_failures), show_labels = VALUES(show_labels)`, username, p.NotifyFailures, p.ShowLabels)
	return err
}

// NotifyOptIns returns the users who want failure mails.
func (s *Store) NotifyOptIns(ctx context.Context) (map[string]bool, error) {
	names, err := s.column(ctx, `SELECT username FROM user_prefs WHERE notify_failures`)
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out, err
}
