package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"mdbpanel/internal/store"
)

func TestReporterThrottles(t *testing.T) {
	var calls []string
	ctx := withReporter(context.Background(), func(pct int, step string) { calls = append(calls, step) })
	report(ctx, 1, "a")
	report(ctx, 2, "a")
	report(ctx, 3, "b")
	if len(calls) != 2 || calls[1] != "b" {
		t.Fatalf("calls = %v", calls)
	}
	report(context.Background(), 1, "no reporter")
}

func TestHashWithProgress(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	os.WriteFile(p, []byte("hello"), 0o600)
	h := sha256.New()
	if err := hashWithProgress(context.Background(), p, h); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("got %s", got)
	}
}

func TestDumpEstimate(t *testing.T) {
	e := newDumpEstimate(map[string]store.TableSize{"small": {Bytes: 100}, "big": {Bytes: 900}, "new": {}})
	if pct, table := e.progress(); pct != 0 || table != "" {
		t.Fatalf("start: %d %q", pct, table)
	}
	var out bytes.Buffer
	var n atomic.Int64
	feed := func(s string) {
		if err := e.copy(&out, strings.NewReader(s), &n); err != nil {
			t.Fatal(err)
		}
	}
	feed("-- header\n\n-- Dumping data for table `small`\n" + strings.Repeat("x", 50))
	if pct, table := e.progress(); table != "small" || pct != 5 {
		t.Fatalf("small: %d %q", pct, table)
	}
	feed("\n-- Dumping data for table `big`\n" + strings.Repeat("y", 450))
	if pct, table := e.progress(); table != "big" || pct != 55 {
		t.Fatalf("big: %d %q", pct, table)
	}
	feed(strings.Repeat("y", 5000))
	if pct, _ := e.progress(); pct != 99 {
		t.Fatalf("capped: %d", pct)
	}
	feed("\n-- Dumping data for table `new`\n")
	if pct, table := e.progress(); table != "new" || pct != 99 {
		t.Fatalf("never backwards: %d %q", pct, table)
	}
	if !strings.Contains(out.String(), "`big`") || n.Load() != int64(out.Len()) {
		t.Fatal("dump not passed through")
	}
	if pct, _ := newDumpEstimate(nil).progress(); pct != -1 {
		t.Errorf("no sizes: got %d want -1", pct)
	}
}
