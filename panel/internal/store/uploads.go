package store

import (
	"context"
	"database/sql"
	"time"
)

type Upload struct {
	ID         int64
	RoleName   string
	FileName   string
	SizeBytes  int64
	UploadedBy string
	Keep       bool
	FirstSeen  time.Time
	SHA256     string // empty until calculated
	VerifiedBy string // "browser" or "user" when a checksum computed elsewhere matched
}

const uploadColumns = `id, role_name, file_name, size_bytes, uploaded_by, keep, first_seen, COALESCE(sha256, ''), verified_by`

func scanUploads(rows *sql.Rows, err error) ([]Upload, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Upload
	for rows.Next() {
		var u Upload
		if err := rows.Scan(&u.ID, &u.RoleName, &u.FileName, &u.SizeBytes, &u.UploadedBy, &u.Keep, &u.FirstSeen, &u.SHA256, &u.VerifiedBy); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// FileState is what the scanner sees on disk. A change of size or mtime
// invalidates the stored checksum.
type FileState struct {
	Size  int64
	MTime time.Time
}

// mtime is the modification time as stored, TIMESTAMP(6) keeps microseconds.
func (f FileState) mtime() time.Time { return f.MTime.UTC().Truncate(time.Microsecond) }

// SyncUploads makes the uploads table match the files found in a role's
// directory. by is recorded for files that were not known yet.
func (s *Store) SyncUploads(ctx context.Context, role string, files map[string]FileState, by string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for name, st := range files {
		// assignments run left to right, the checksum is compared before size and mtime change
		if _, err := tx.ExecContext(ctx, `
INSERT INTO uploads (role_name, file_name, size_bytes, mtime, uploaded_by) VALUES (?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
    sha256 = IF(size_bytes = VALUES(size_bytes) AND mtime <=> VALUES(mtime), sha256, NULL),
    verified_by = IF(size_bytes = VALUES(size_bytes) AND mtime <=> VALUES(mtime), verified_by, ''),
    size_bytes = VALUES(size_bytes),
    mtime = VALUES(mtime)`, role, name, st.Size, st.mtime(), by); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, file_name FROM uploads WHERE role_name = ?`, role)
	if err != nil {
		return err
	}
	var gone []int64
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		if _, ok := files[name]; !ok {
			gone = append(gone, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range gone {
		if _, err := tx.ExecContext(ctx, `DELETE FROM uploads WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) AddUpload(ctx context.Context, role, file string, st FileState, by, sha256, verifiedBy string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO uploads (role_name, file_name, size_bytes, mtime, uploaded_by, sha256, verified_by) VALUES (?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE size_bytes = VALUES(size_bytes), mtime = VALUES(mtime), uploaded_by = VALUES(uploaded_by),
    sha256 = VALUES(sha256), verified_by = VALUES(verified_by), first_seen = now(6)`, role, file, st.Size, st.mtime(), by, sha256, verifiedBy)
	return err
}

// PendingHashes lists uploads whose checksum still has to be calculated.
func (s *Store) PendingHashes(ctx context.Context) ([]Upload, error) {
	return scanUploads(s.db.QueryContext(ctx, `SELECT `+uploadColumns+` FROM uploads WHERE sha256 IS NULL ORDER BY id`))
}

// SetUploadHash stores a checksum unless the file changed while it was hashed.
func (s *Store) SetUploadHash(ctx context.Context, id int64, st FileState, sha256 string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE uploads SET sha256 = ? WHERE id = ? AND size_bytes = ? AND mtime = ?`, sha256, id, st.Size, st.mtime())
	return err
}

func (s *Store) SetUploadVerified(ctx context.Context, id int64, by string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE uploads SET verified_by = ? WHERE id = ?`, by, id)
	return err
}

func (s *Store) ListUploads(ctx context.Context, role string) ([]Upload, error) {
	return scanUploads(s.db.QueryContext(ctx, `SELECT `+uploadColumns+` FROM uploads WHERE role_name = ? ORDER BY first_seen DESC, id DESC`, role))
}

func (s *Store) GetUpload(ctx context.Context, id int64) (*Upload, error) {
	list, err := scanUploads(s.db.QueryContext(ctx, `SELECT `+uploadColumns+` FROM uploads WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return &list[0], nil
}

func (s *Store) SetUploadKeep(ctx context.Context, id int64, keep bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE uploads SET keep = ? WHERE id = ?`, keep, id)
	return err
}

func (s *Store) DeleteUpload(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM uploads WHERE id = ?`, id)
	return err
}

func (s *Store) DeleteUploadsOf(ctx context.Context, role string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM uploads WHERE role_name = ?`, role)
	return err
}

// ExpiredUploads lists uploads older than retention that are not kept.
func (s *Store) ExpiredUploads(ctx context.Context, retention time.Duration) ([]Upload, error) {
	return scanUploads(s.db.QueryContext(ctx, `SELECT `+uploadColumns+` FROM uploads WHERE NOT keep AND first_seen < now(6) - INTERVAL ? SECOND`, int64(retention.Seconds())))
}

// UploadInUse reports whether a queued or running import uses the upload.
func (s *Store) UploadInUse(ctx context.Context, id int64) (bool, error) {
	var used bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE upload_id = ? AND status IN ('queued', 'running'))`, id).Scan(&used)
	return used, err
}
