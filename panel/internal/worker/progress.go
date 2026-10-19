package worker

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// reporter stores the progress of the running job. pct is -1 when the total is
// unknown. Writes are throttled, a step change is written at once.
type reporter struct {
	mu       sync.Mutex
	save     func(pct int, step string)
	lastStep string
	lastAt   time.Time
}

type reporterKey struct{}

func withReporter(ctx context.Context, save func(pct int, step string)) context.Context {
	return context.WithValue(ctx, reporterKey{}, &reporter{save: save})
}

func report(ctx context.Context, pct int, step string) {
	r, ok := ctx.Value(reporterKey{}).(*reporter)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if step == r.lastStep && time.Since(r.lastAt) < 2*time.Second {
		return
	}
	r.lastStep, r.lastAt = step, time.Now()
	r.save(pct, step)
}

func percent(done, total int64) int {
	if total <= 0 {
		return -1
	}
	p := int(done * 100 / total)
	return min(max(p, 0), 100)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// hashWithProgress is uploads.HashFile with progress reports.
func hashWithProgress(ctx context.Context, path string, hash io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	buf := make([]byte, 8<<20)
	var done int64
	for {
		n, err := f.Read(buf)
		if n > 0 {
			hash.Write(buf[:n])
			done += int64(n)
			report(ctx, percent(done, info.Size()), "Checking the SHA-256 of the file")
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}
