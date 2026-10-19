package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"time"
)

var ErrJobActive = errors.New("another snapshot or restore of this database is still running")

// Snapshot kinds, each kind is rotated separately so frequent scheduled
// snapshots never push out a manual one.
const (
	KindManual     = "manual"
	KindScheduled  = "scheduled"
	KindPreRestore = "pre_restore"
)

type Snapshot struct {
	ID        int64
	Database  string
	OwnerRole string
	Kind      string
	File      string // relative to the snapshot directory
	SizeBytes int64
	Note      string
	CreatedBy string
	CreatedAt time.Time
}

type Job struct {
	ID          int64
	Kind        string
	Database    string
	SnapshotID  *int64
	UploadID    *int64
	Strict      bool
	Safety      bool // take a safety snapshot before a restore or import
	Progress    *int // percent, nil when unknown
	Step        string
	StartedAt   *time.Time
	RequestedBy string
	Status      string
	Error       string
	CreatedAt   time.Time
	FinishedAt  *time.Time
	Scheduled   bool
}

func (j Job) Active() bool { return j.Status == "queued" || j.Status == "running" }

func (s *Store) EnqueueSnapshot(ctx context.Context, database, by string) error {
	return s.enqueue(ctx, "snapshot", database, nil, nil, by, false, true, true)
}

func (s *Store) EnqueueRestore(ctx context.Context, database string, snapshotID int64, safetySnapshot bool, by string) error {
	return s.enqueue(ctx, "restore", database, &snapshotID, nil, by, false, true, safetySnapshot)
}

// EnqueueImport queues an import. strict imports stop at the first error and
// change nothing, otherwise failing statements are skipped.
func (s *Store) EnqueueImport(ctx context.Context, database string, uploadID int64, strict, safetySnapshot bool, by string) error {
	return s.enqueue(ctx, "import", database, nil, &uploadID, by, false, strict, safetySnapshot)
}

// EnqueueScheduled adds a scheduled snapshot job. A run is skipped, not queued
// twice, when a job for the database is still active.
func (s *Store) EnqueueScheduled(ctx context.Context, database string) error {
	return s.enqueue(ctx, "snapshot", database, nil, nil, "schedule", true, true, true)
}

func (s *Store) enqueue(ctx context.Context, kind, database string, snapshotID, uploadID *int64, by string, scheduled, strict, safetySnapshot bool) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs (kind, db_name, snapshot_id, upload_id, requested_by, scheduled, strict, safety_snapshot) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		kind, database, snapshotID, uploadID, by, scheduled, strict, safetySnapshot)
	// the one-active-job-per-database index
	if mysqlCode(err) == 1062 {
		return ErrJobActive
	}
	return err
}

const jobColumns = `id, kind, db_name, snapshot_id, upload_id, strict, safety_snapshot, progress, progress_step, started_at, requested_by, status, error_msg, created_at, finished_at, scheduled`

func scanJobs(rows *sql.Rows, err error) ([]Job, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		var progress sql.NullInt16
		var snap, up sql.NullInt64
		var started, finished sql.NullTime
		if err := rows.Scan(&j.ID, &j.Kind, &j.Database, &snap, &up, &j.Strict, &j.Safety, &progress, &j.Step, &started, &j.RequestedBy, &j.Status, &j.Error, &j.CreatedAt, &finished, &j.Scheduled); err != nil {
			return nil, err
		}
		if progress.Valid {
			p := int(progress.Int16)
			j.Progress = &p
		}
		if snap.Valid {
			j.SnapshotID = &snap.Int64
		}
		if up.Valid {
			j.UploadID = &up.Int64
		}
		if started.Valid {
			j.StartedAt = &started.Time
		}
		if finished.Valid {
			j.FinishedAt = &finished.Time
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) ListJobs(ctx context.Context, database string, limit int) ([]Job, error) {
	return scanJobs(s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE db_name = ? ORDER BY id DESC LIMIT ?`, database, limit))
}

// ClaimJob marks the oldest queued job as running, nil when there is none.
func (s *Store) ClaimJob(ctx context.Context) (*Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM jobs WHERE status = 'queued' ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'running', started_at = now(6) WHERE id = ?`, id); err != nil {
		return nil, err
	}
	jobs, err := scanJobs(tx.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id))
	if err != nil || len(jobs) == 0 {
		return nil, errors.Join(err, ErrNotFound)
	}
	return &jobs[0], tx.Commit()
}

// Warning marks a job that finished but has something to report.
type Warning struct{ Msg string }

func (w Warning) Error() string { return w.Msg }

// SetJobProgress stores the progress of a running job, pct < 0 means unknown.
func (s *Store) SetJobProgress(ctx context.Context, id int64, pct int, step string) error {
	var p *int
	if pct >= 0 {
		p = &pct
	}
	if len(step) > 1000 {
		step = step[:1000]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET progress = ?, progress_step = ? WHERE id = ? AND status = 'running'`, p, step, id)
	return err
}

func (s *Store) FinishJob(ctx context.Context, id int64, jobErr error) error {
	status, msg := "done", ""
	var warn Warning
	if errors.As(jobErr, &warn) {
		msg = warn.Msg
	} else if jobErr != nil {
		status, msg = "failed", jobErr.Error()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET status = ?, error_msg = ?, finished_at = now(6) WHERE id = ?`, status, msg, id)
	return err
}

// UnnotifiedFailures lists failed jobs nobody was told about yet. Failures
// older than a day are dropped, a mail that late helps nobody.
func (s *Store) UnnotifiedFailures(ctx context.Context) ([]Job, error) {
	return scanJobs(s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE status = 'failed' AND NOT notified AND finished_at > now(6) - INTERVAL 1 DAY ORDER BY id`))
}

func (s *Store) MarkNotified(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET notified = TRUE WHERE id = ?`, id)
	return err
}

// FailInterruptedJobs is called by the worker on start, a running job cannot survive a restart.
func (s *Store) FailInterruptedJobs(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET status = 'failed', error_msg = 'interrupted by an app restart', finished_at = now(6) WHERE status = 'running'`)
	return err
}

func (s *Store) AddSnapshot(ctx context.Context, snap Snapshot) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO snapshots (db_name, owner_role, kind, file, size_bytes, note, created_by) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		snap.Database, snap.OwnerRole, snap.Kind, snap.File, snap.SizeBytes, snap.Note, snap.CreatedBy)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const snapshotColumns = `id, db_name, owner_role, kind, file, size_bytes, note, created_by, created_at`

func scanSnapshots(rows *sql.Rows, err error) ([]Snapshot, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		var sn Snapshot
		if err := rows.Scan(&sn.ID, &sn.Database, &sn.OwnerRole, &sn.Kind, &sn.File, &sn.SizeBytes, &sn.Note, &sn.CreatedBy, &sn.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

func (s *Store) ListSnapshots(ctx context.Context, database string) ([]Snapshot, error) {
	return scanSnapshots(s.db.QueryContext(ctx, `SELECT `+snapshotColumns+` FROM snapshots WHERE db_name = ? ORDER BY id DESC`, database))
}

func (s *Store) ListAllSnapshots(ctx context.Context) ([]Snapshot, error) {
	return scanSnapshots(s.db.QueryContext(ctx, `SELECT `+snapshotColumns+` FROM snapshots ORDER BY created_at DESC, id DESC`))
}

func (s *Store) GetSnapshot(ctx context.Context, id int64) (*Snapshot, error) {
	snaps, err := scanSnapshots(s.db.QueryContext(ctx, `SELECT `+snapshotColumns+` FROM snapshots WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if len(snaps) == 0 {
		return nil, ErrNotFound
	}
	return &snaps[0], nil
}

// DeleteSnapshot removes the row and returns the file the caller has to delete.
func (s *Store) DeleteSnapshot(ctx context.Context, id int64) (string, error) {
	var file string
	err := s.db.QueryRowContext(ctx, `DELETE FROM snapshots WHERE id = ? RETURNING file`, id).Scan(&file)
	return file, notFound(err)
}

// DeleteSnapshotsOf removes all snapshot rows of a database, a new database with
// the same name must never see them.
func (s *Store) DeleteSnapshotsOf(ctx context.Context, database string) ([]string, error) {
	return s.column(ctx, `DELETE FROM snapshots WHERE db_name = ? RETURNING file`, database)
}

// PruneSnapshots keeps the newest keep snapshots of one kind of a database plus
// the protected ids and returns the files of the deleted rows.
func (s *Store) PruneSnapshots(ctx context.Context, database, kind string, keep int, protect ...int64) ([]string, error) {
	ids, err := s.column(ctx, `SELECT id FROM snapshots WHERE db_name = ? AND kind = ? ORDER BY id DESC LIMIT 1000000 OFFSET ?`, database, kind, keep)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, v := range ids {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return files, err
		}
		if slices.Contains(protect, id) {
			continue
		}
		file, err := s.DeleteSnapshot(ctx, id)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return files, err
		}
		if file != "" {
			files = append(files, file)
		}
	}
	return files, nil
}

// AllSnapshotFiles maps every stored snapshot file to its id, for reconciling
// with the files on disk.
func (s *Store) AllSnapshotFiles(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT file, id FROM snapshots`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files := map[string]int64{}
	for rows.Next() {
		var f string
		var id int64
		if err := rows.Scan(&f, &id); err != nil {
			return nil, err
		}
		files[f] = id
	}
	return files, rows.Err()
}

type Schedule struct {
	Database  string
	OwnerRole string
	Cron      string
	NextRun   time.Time
	UpdatedBy string
}

func scanSchedules(rows *sql.Rows, err error) ([]Schedule, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		var sc Schedule
		if err := rows.Scan(&sc.Database, &sc.OwnerRole, &sc.Cron, &sc.NextRun, &sc.UpdatedBy); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

func (s *Store) GetSchedule(ctx context.Context, database string) (*Schedule, error) {
	list, err := scanSchedules(s.db.QueryContext(ctx, `SELECT db_name, owner_role, cron, next_run, updated_by FROM schedules WHERE db_name = ?`, database))
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return &list[0], nil
}

func (s *Store) SetSchedule(ctx context.Context, sc Schedule) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO schedules (db_name, owner_role, cron, next_run, updated_by) VALUES (?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE owner_role = VALUES(owner_role), cron = VALUES(cron), next_run = VALUES(next_run),
    updated_by = VALUES(updated_by), updated_at = now(6)`, sc.Database, sc.OwnerRole, sc.Cron, sc.NextRun.UTC(), sc.UpdatedBy)
	return err
}

func (s *Store) DeleteSchedule(ctx context.Context, database string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM schedules WHERE db_name = ?`, database)
	return err
}

func (s *Store) DueSchedules(ctx context.Context) ([]Schedule, error) {
	return scanSchedules(s.db.QueryContext(ctx, `SELECT db_name, owner_role, cron, next_run, updated_by FROM schedules WHERE next_run <= now(6) ORDER BY next_run`))
}

func (s *Store) SetNextRun(ctx context.Context, database string, next time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE schedules SET next_run = ? WHERE db_name = ?`, next.UTC(), database)
	return err
}
