// Package worker runs snapshot and restore jobs and serves the panel's
// catalog calls (package rpc). It runs as the mysql OS user, which is the
// MariaDB superuser on the socket. Dumps and loads run as a temporary
// account with only the tenant role, so tenant controlled content is never
// processed with superuser rights.
package worker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mdbpanel/internal/access"
	"mdbpanel/internal/directory"
	"mdbpanel/internal/names"
	"mdbpanel/internal/notify"
	"mdbpanel/internal/schedule"
	"mdbpanel/internal/store"
	"mdbpanel/internal/uploads"
)

type Config struct {
	Directory    *directory.Client
	Admins       access.Admins
	SnapshotDir  string
	UploadDir    string
	Socket       string // MariaDB unix socket
	MaxSnapshots int
	Mailer       *notify.Mailer // nil without the sendmail addon
	Origin       string         // https://fqdn of the app, for links in mails
	TuningFile   string         // include file of the server configuration with the admin's variables
}

type Worker struct {
	cfg     Config
	store   *store.Store
	tenants store.Tenants
	log     *slog.Logger
}

func New(cfg Config, st *store.Store, log *slog.Logger) *Worker {
	return &Worker{cfg: cfg, store: st, tenants: store.Tenants{Catalog: st}, log: log}
}

// Snapshot files are SQL dumps compressed with zstd.
const snapshotExt = ".sql.zst"

// Resolve maps a stored relative snapshot path to an absolute one inside dir.
func Resolve(dir, rel string) (string, error) {
	abs := filepath.Join(dir, rel)
	r, err := filepath.Rel(dir, abs)
	if err != nil || r == ".." || strings.HasPrefix(r, "../") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("snapshot path %q escapes the snapshot directory", rel)
	}
	return abs, nil
}

func RemoveFiles(dir string, files []string, log *slog.Logger) {
	for _, f := range files {
		p, err := Resolve(dir, f)
		if err == nil {
			err = os.Remove(p)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Error("removing snapshot file", "file", f, "err", err)
		}
	}
}

func (w *Worker) Run(ctx context.Context) error {
	if err := w.store.FailInterruptedJobs(ctx); err != nil {
		return err
	}
	w.removePartials()
	w.reconcileSnapshots(ctx)
	if err := w.store.DropAllInternal(ctx); err != nil {
		w.log.Error("dropping leftover staging databases and job accounts", "err", err)
	}
	w.log.Info("worker started", "max_snapshots", w.cfg.MaxSnapshots)
	for {
		w.enqueueDue(ctx)
		job, err := w.store.ClaimJob(ctx)
		if err != nil && ctx.Err() == nil {
			w.log.Error("claiming job", "err", err)
		}
		if job == nil {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
			}
			continue
		}
		w.log.Info("job started", "id", job.ID, "kind", job.Kind, "database", job.Database)
		lastStep := ""
		jobCtx := withReporter(ctx, func(pct int, step string) {
			if step != lastStep {
				w.log.Info("job step", "id", job.ID, "step", step)
				lastStep = step
			}
			if err := w.store.SetJobProgress(ctx, job.ID, pct, step); err != nil && ctx.Err() == nil {
				w.log.Error("storing job progress", "id", job.ID, "err", err)
			}
		})
		jobErr := w.run(jobCtx, job)
		switch {
		case errors.As(jobErr, new(store.Warning)):
			w.log.Warn("job done with warnings", "id", job.ID, "warning", jobErr)
		case jobErr != nil:
			w.log.Error("job failed", "id", job.ID, "err", jobErr)
		default:
			w.log.Info("job done", "id", job.ID)
		}
		if err := w.store.FinishJob(context.WithoutCancel(ctx), job.ID, jobErr); err != nil {
			w.log.Error("finishing job", "id", job.ID, "err", err)
		}
	}
}

func (w *Worker) run(ctx context.Context, job *store.Job) error {
	db, err := w.tenants.GetDatabase(ctx, job.Database)
	if err != nil {
		return fmt.Errorf("database %s: %w", job.Database, err)
	}
	if err := names.Validate(db.Name); err != nil {
		return err
	}
	if err := names.Validate(db.Owner); err != nil {
		return err
	}
	acct, err := w.jobAccount(ctx, job.ID, db.Owner)
	if err != nil {
		return fmt.Errorf("creating the job account: %w", err)
	}
	defer acct.close(context.WithoutCancel(ctx))
	switch job.Kind {
	case "snapshot":
		kind := store.KindManual
		if job.Scheduled {
			kind = store.KindScheduled
		}
		id, err := w.snapshot(ctx, acct, db, job.RequestedBy, kind, "")
		if err != nil {
			return err
		}
		w.prune(ctx, db.Name, kind, id)
		return nil
	case "restore":
		if job.SnapshotID == nil {
			return errors.New("restore job without snapshot")
		}
		return w.restore(ctx, acct, db, *job.SnapshotID, job.Safety, job.RequestedBy)
	case "import":
		if job.UploadID == nil {
			return errors.New("import job without upload")
		}
		return w.importUpload(ctx, acct, db, *job.UploadID, job.Strict, job.Safety, job.RequestedBy)
	}
	return fmt.Errorf("unknown job kind %q", job.Kind)
}

func (w *Worker) snapshot(ctx context.Context, acct *jobAccount, db *store.Database, by, kind, note string) (int64, error) {
	dir := filepath.Join(w.cfg.SnapshotDir, db.Name)
	if err := ensureSharedDir(dir); err != nil {
		return 0, err
	}
	suffix := make([]byte, 4)
	rand.Read(suffix)
	rel := filepath.Join(db.Name, time.Now().UTC().Format("20060102T150405Z")+"-"+hex.EncodeToString(suffix)+snapshotExt)
	final := filepath.Join(w.cfg.SnapshotDir, rel)
	tmp := final + ".partial"
	defer os.Remove(tmp)

	step := "Creating snapshot"
	if kind == store.KindPreRestore {
		step = "Taking a safety snapshot of the current data"
	}
	if err := w.dumpDatabase(ctx, acct, db.Name, step, tmp); err != nil {
		return 0, err
	}
	if err := os.Chmod(tmp, 0o640); err != nil {
		return 0, err
	}
	info, err := os.Stat(tmp)
	if err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, final); err != nil {
		return 0, err
	}
	id, err := w.store.AddSnapshot(ctx, store.Snapshot{Database: db.Name, OwnerRole: db.Owner, Kind: kind, File: rel, SizeBytes: info.Size(), Note: note, CreatedBy: by})
	if err != nil {
		os.Remove(final)
		return 0, err
	}
	return id, nil
}

// ensureSharedDir creates a snapshot directory the panel (group cloudron) can
// read and delete in. The group comes from the setgid parent, which new
// directories inherit. chmod is not used: the kernel clears setgid when the
// caller (mysql) is not in the directory's group. The worker runs with umask 007.
func ensureSharedDir(dir string) error {
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSetgid == 0 || info.Mode().Perm()&0o070 != 0o070 {
		return fmt.Errorf("%s has mode %v, it needs setgid and group rwx (restart the app to repair)", dir, info.Mode())
	}
	return nil
}

// reconcileSnapshots matches snapshot rows and files. Snapshots are not part of
// Cloudron backups: after a restore or clone rows can point to missing files,
// and files of snapshots taken after the backup have no row.
func (w *Worker) reconcileSnapshots(ctx context.Context) {
	known, err := w.store.AllSnapshotFiles(ctx)
	if err != nil {
		w.log.Error("listing snapshots", "err", err)
		return
	}
	for file, id := range known {
		path, err := Resolve(w.cfg.SnapshotDir, file)
		if err != nil {
			continue
		}
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			if _, err := w.store.DeleteSnapshot(ctx, id); err != nil {
				w.log.Error("deleting snapshot without file", "id", id, "err", err)
				continue
			}
			w.log.Info("snapshot file is gone, removed its entry", "id", id, "file", file)
		}
	}
	dumps, _ := filepath.Glob(filepath.Join(w.cfg.SnapshotDir, "*", "*"+snapshotExt))
	for _, path := range dumps {
		rel, err := filepath.Rel(w.cfg.SnapshotDir, path)
		if err != nil {
			continue
		}
		if _, ok := known[rel]; !ok {
			if err := os.Remove(path); err != nil {
				w.log.Error("removing snapshot file without entry", "file", rel, "err", err)
				continue
			}
			w.log.Info("removed snapshot file without entry", "file", rel)
		}
	}
}

// removePartials deletes dumps left behind by a dump that was interrupted by a restart.
func (w *Worker) removePartials() {
	partials, _ := filepath.Glob(filepath.Join(w.cfg.SnapshotDir, "*", "*.partial"))
	for _, p := range partials {
		if err := os.Remove(p); err != nil {
			w.log.Error("removing partial snapshot", "file", p, "err", err)
		}
	}
}

func (w *Worker) restore(ctx context.Context, acct *jobAccount, db *store.Database, snapshotID int64, safety bool, by string) error {
	snap, err := w.store.GetSnapshot(ctx, snapshotID)
	if err != nil {
		return err
	}
	// never restore another tenant's data, even if a database name was reused
	if snap.Database != db.Name || snap.OwnerRole != db.Owner {
		return errors.New("snapshot does not belong to this database")
	}
	file, err := Resolve(w.cfg.SnapshotDir, snap.File)
	if err != nil {
		return err
	}
	if _, err := uploads.Format(file); err != nil {
		return fmt.Errorf("snapshot file is not readable: %w", err)
	}
	before, err := w.replaceWith(ctx, acct, db, file, db.Name, true, safety, by, fmt.Sprintf("taken before restoring snapshot #%d", snap.ID))
	if before != 0 {
		w.prune(ctx, db.Name, store.KindPreRestore, snap.ID, before)
	}
	return err
}

func (w *Worker) importUpload(ctx context.Context, acct *jobAccount, db *store.Database, uploadID int64, strict, safety bool, by string) error {
	up, err := w.store.GetUpload(ctx, uploadID)
	if err != nil {
		return fmt.Errorf("upload #%d: %w", uploadID, err)
	}
	// uploads belong to a role, they can only go into that role's databases
	if up.RoleName != db.Owner {
		return errors.New("upload does not belong to the owner of this database")
	}
	file, err := uploads.Path(w.cfg.UploadDir, up.RoleName, up.FileName)
	if err != nil {
		return err
	}
	if up.SHA256 == "" {
		return errors.New("the checksum of the file has not been calculated yet, try again in a moment")
	}
	hash := sha256.New()
	if err := hashWithProgress(ctx, file, hash); err != nil {
		return err
	}
	if sum := hex.EncodeToString(hash.Sum(nil)); sum != up.SHA256 {
		return fmt.Errorf("the file changed since it was uploaded (SHA-256 %s, expected %s), upload it again", sum, up.SHA256)
	}
	if _, err := uploads.Format(file); err != nil {
		return err
	}
	before, err := w.replaceWith(ctx, acct, db, file, "", strict, safety, by, fmt.Sprintf("taken before importing %s", up.FileName))
	if before != 0 {
		w.prune(ctx, db.Name, store.KindPreRestore, before)
	}
	return err
}

// replaceWith takes a safety snapshot, loads the dump as the job account into
// a new staging database and then swaps the tables in. The current database
// stays online and unchanged until the swap. source is the database the dump
// was made from, "" when the file says. It returns the safety snapshot id
// once it exists.
func (w *Worker) replaceWith(ctx context.Context, acct *jobAccount, db *store.Database, file, source string, strict, safety bool, by, note string) (int64, error) {
	var before int64
	previous := ""
	if safety {
		var err error
		if before, err = w.snapshot(ctx, acct, db, by, store.KindPreRestore, note); err != nil {
			return 0, fmt.Errorf("safety snapshot failed, database unchanged: %w", err)
		}
		previous = fmt.Sprintf(" (snapshot #%d has the previous contents)", before)
	}
	staging := store.StagingName(acct.jobID)
	report(ctx, -1, "Creating a new database to load into, the current one stays online")
	if err := w.store.CreateStaging(ctx, staging, db.Name, db.Owner); err != nil {
		return before, fmt.Errorf("creating the staging database failed, database unchanged: %w", err)
	}
	defer w.dropStaging(ctx, staging)
	var warning error
	if err := w.loadDump(ctx, acct, file, source, staging, strict); err != nil {
		if strict || errors.As(err, new(fatalError)) {
			return before, fmt.Errorf("nothing changed, the database still has its previous contents: %w", err)
		}
		// the client exits with an error after skipping failed statements, the rest is loaded
		warning = store.Warning{Msg: fmt.Sprintf("loaded with errors: %v%s", err, previous)}
	}
	report(ctx, -1, "Switching to the new tables")
	as, err := store.OpenPassword(w.cfg.Socket, acct.user, acct.password, db.Name)
	if err != nil {
		return before, err
	}
	defer as.Close()
	var warn store.Warning
	err = w.store.SwapStaging(ctx, db.Name, acct.jobID, as)
	switch {
	case errors.As(err, &warn):
		return before, store.Warning{Msg: warn.Msg + previous}
	case err != nil:
		return before, fmt.Errorf("loaded, but switching to the new tables failed, the database still has its previous contents: %w", err)
	}
	return before, warning
}

func (w *Worker) dropStaging(ctx context.Context, staging string) {
	if err := w.store.DropInternal(context.WithoutCancel(ctx), staging); err != nil {
		w.log.Error("dropping staging database", "database", staging, "err", err)
	}
}

func (w *Worker) prune(ctx context.Context, database, kind string, protect ...int64) {
	files, err := w.store.PruneSnapshots(ctx, database, kind, w.cfg.MaxSnapshots, protect...)
	if err != nil {
		w.log.Error("pruning snapshots", "database", database, "err", err)
		return
	}
	RemoveFiles(w.cfg.SnapshotDir, files, w.log)
}

// enqueueDue turns due schedules into jobs and moves each schedule to its next run.
func (w *Worker) enqueueDue(ctx context.Context) {
	due, err := w.store.DueSchedules(ctx)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Error("reading schedules", "err", err)
		}
		return
	}
	for _, sc := range due {
		db, err := w.tenants.GetDatabase(ctx, sc.Database)
		if errors.Is(err, store.ErrNotFound) || (err == nil && db.Owner != sc.OwnerRole) {
			w.log.Info("removing schedule of a deleted or reassigned database", "database", sc.Database)
			if err := w.store.DeleteSchedule(ctx, sc.Database); err != nil {
				w.log.Error("removing schedule", "database", sc.Database, "err", err)
			}
			continue
		}
		if err != nil {
			w.log.Error("reading database", "database", sc.Database, "err", err)
			continue
		}
		parsed, err := schedule.Parse(sc.Cron)
		if err != nil {
			w.log.Error("invalid stored schedule", "database", sc.Database, "cron", sc.Cron, "err", err)
			continue
		}
		// computed from now, so runs missed while the app was down collapse into one
		if err := w.store.SetNextRun(ctx, sc.Database, parsed.Next(time.Now())); err != nil {
			w.log.Error("updating schedule", "database", sc.Database, "err", err)
			continue
		}
		err = w.store.EnqueueScheduled(ctx, sc.Database)
		if errors.Is(err, store.ErrJobActive) {
			w.log.Info("scheduled snapshot skipped, a job is still running", "database", sc.Database)
		} else if err != nil {
			w.log.Error("queueing scheduled snapshot", "database", sc.Database, "err", err)
		}
	}
}
