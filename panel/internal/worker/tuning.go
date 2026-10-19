package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"mdbpanel/internal/allowlist"
	"mdbpanel/internal/store"
	"mdbpanel/internal/tuning"
)

// Settings of the server variables admins change in the panel.
const (
	SettingTuning          = "server_params"           // JSON object, name to value
	SettingTuningRequested = "server_params_requested" // set by the panel after a change
	SettingTuningError     = "server_params_error"     // last apply error, empty when it worked
)

// ApplyTuning applies the admin's variables when the panel asks for it. The
// panel cannot do it itself: SET GLOBAL needs admin privileges, and the
// include file is read by MariaDB.
func (w *Worker) ApplyTuning(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		requested, err := w.store.TakeSetting(ctx, SettingTuningRequested)
		if err != nil && ctx.Err() == nil {
			w.log.Error("reading the parameter change request", "err", err)
		}
		if requested {
			msg := ""
			if err := w.applyTuning(ctx); err != nil {
				w.log.Error("applying server parameters", "err", err)
				msg = err.Error()
			} else {
				w.log.Info("server parameters applied")
			}
			if err := w.store.SetSetting(ctx, SettingTuningError, msg); err != nil {
				w.log.Error("storing the parameter apply result", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// applyTuning writes the include file for the next start and sets every
// variable in the running server, also the ones reset to their default.
func (w *Worker) applyTuning(ctx context.Context) error {
	values, invalid, err := StoredTuning(ctx, w.store)
	if err != nil {
		return err
	}
	if len(invalid) > 0 {
		return errors.Join(invalid...)
	}
	mem := tuning.MemoryMB()
	content, err := tuning.Render(values, mem)
	if err != nil {
		return err
	}
	assignments, err := tuning.Assignments(values, mem)
	if err != nil {
		return err
	}
	if err := allowlist.WriteFileAtomic(w.cfg.TuningFile, content, 0o644); err != nil {
		return err
	}
	var failed []error
	for _, a := range assignments {
		if err := w.store.SetGlobal(ctx, a.Name, a.Value); err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", a.Name, err))
		}
	}
	return errors.Join(failed...)
}

// StoredTuning returns the stored variables after validating them again.
// Invalid ones, for example after the memory limit was lowered, are left out
// and returned.
func StoredTuning(ctx context.Context, st *store.Store) (map[string]string, []error, error) {
	raw, err := st.Setting(ctx, SettingTuning)
	if err != nil {
		return nil, nil, err
	}
	values, err := tuning.Decode(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("stored parameters: %w", err)
	}
	clean, invalid := tuning.Validate(values, tuning.MemoryMB())
	return clean, invalid, nil
}
