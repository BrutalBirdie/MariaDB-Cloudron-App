package worker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"

	"mdbpanel/internal/sqlfilter"
	"mdbpanel/internal/store"
	"mdbpanel/internal/uploads"
)

// jobAccount is the temporary MariaDB account of one job, with an option
// file for the command line tools, so the password is not in their arguments.
type jobAccount struct {
	w              *Worker
	jobID          int64
	user, password string
	optionFile     string
}

func (w *Worker) jobAccount(ctx context.Context, jobID int64, role string) (*jobAccount, error) {
	user, password, err := w.store.CreateJobAccount(ctx, jobID, role)
	if err != nil {
		return nil, err
	}
	a := &jobAccount{w: w, jobID: jobID, user: user, password: password}
	f, err := os.CreateTemp("", "mdbjob-*.cnf")
	if err != nil {
		a.close(ctx)
		return nil, err
	}
	a.optionFile = f.Name()
	_, err = fmt.Fprintf(f, "[client]\nuser=%s\npassword=%s\nsocket=%s\n", user, password, w.cfg.Socket)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		a.close(ctx)
		return nil, err
	}
	return a, nil
}

func (a *jobAccount) close(ctx context.Context) {
	if a.optionFile != "" {
		os.Remove(a.optionFile)
	}
	if err := a.w.store.DropJobAccount(ctx, a.jobID); err != nil {
		a.w.log.Error("dropping job account", "job", a.jobID, "err", err)
	}
}

// tool runs a MariaDB client program as the job account. The option file
// has to be the first argument.
func (a *jobAccount) tool(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, append([]string{"--defaults-extra-file=" + a.optionFile}, args...)...)
}

// stderrTail keeps the first lines a tool writes to stderr, for the error.
type stderrTail struct {
	mu    sync.Mutex
	lines []string
	buf   bytes.Buffer
}

func (t *stderrTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	for {
		line, err := t.buf.ReadString('\n')
		if err != nil {
			t.buf.WriteString(line)
			break
		}
		if len(t.lines) < 20 {
			t.lines = append(t.lines, strings.TrimSpace(line))
		}
	}
	if t.buf.Len() > 64<<10 {
		t.buf.Reset()
	}
	return len(p), nil
}

func (t *stderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	msg := strings.TrimSpace(strings.Join(append(t.lines, t.buf.String()), "\n"))
	if len(msg) > 800 {
		msg = msg[:800] + "..."
	}
	return msg
}

func toolError(tool string, err error, stderr *stderrTail) error {
	if msg := stderr.String(); msg != "" {
		return fmt.Errorf("%s: %v: %s", tool, err, msg)
	}
	return fmt.Errorf("%s: %v", tool, err)
}

// dumpDatabase writes a zstd compressed mariadb-dump of database to out and
// reports an estimated percentage: the data size of the tables already dumped
// plus the SQL written for the current table, over the data size of all tables.
func (w *Worker) dumpDatabase(ctx context.Context, acct *jobAccount, database, step, out string) error {
	report(ctx, -1, step)
	sizes, err := w.store.TableSizes(ctx, database)
	if err != nil {
		w.log.Warn("no size estimate for the dump", "database", database, "err", err)
	}
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	zw, err := zstd.NewWriter(f, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return err
	}
	est := newDumpEstimate(sizes)
	cmd := acct.tool(ctx, "mariadb-dump", "--single-transaction", "--quick", "--routines", "--events", "--triggers", "--no-tablespaces",
		"--hex-blob", "--skip-dump-date", "--max-statement-time=0", "--", database)
	var stderr stderrTail
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var written atomic.Int64
	done := make(chan error, 1)
	go func() {
		done <- est.copy(zw, stdout, &written)
	}()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var copyErr error
loop:
	for {
		select {
		case copyErr = <-done:
			break loop
		case <-t.C:
			pct, table := est.progress()
			msg := step
			if table != "" {
				msg += ": table " + table
			}
			if info, err := f.Stat(); err == nil {
				msg += ", " + humanBytes(info.Size()) + " written"
			}
			report(ctx, pct, msg)
		}
	}
	waitErr := cmd.Wait()
	if waitErr != nil {
		return toolError("mariadb-dump", waitErr, &stderr)
	}
	if copyErr != nil {
		return copyErr
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return f.Sync()
}

type dumpEstimate struct {
	mu      sync.Mutex
	sizes   map[string]store.TableSize
	total   int64
	done    int64
	current string
	inTable int64 // SQL bytes written for the current table
	last    int
}

func newDumpEstimate(sizes map[string]store.TableSize) *dumpEstimate {
	e := &dumpEstimate{sizes: sizes, last: -1}
	for _, t := range sizes {
		e.total += t.Bytes
	}
	return e
}

var dataMarker = []byte("\n-- Dumping data for table `")

// copy passes the dump through and watches for the comment mariadb-dump
// writes before the data of each table.
func (e *dumpEstimate) copy(dst io.Writer, src io.Reader, written *atomic.Int64) error {
	r := bufio.NewReaderSize(src, 1<<20)
	tail := []byte{}
	buf := make([]byte, 1<<20)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, werr := dst.Write(chunk); werr != nil {
				return werr
			}
			written.Add(int64(n))
			e.scan(append(tail, chunk...), int64(n))
			if len(chunk) > 256 {
				tail = append(tail[:0], chunk[len(chunk)-256:]...)
			} else {
				tail = append(tail, chunk...)
				if len(tail) > 256 {
					tail = tail[len(tail)-256:]
				}
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// scan looks for table markers in b, which ends with the n new bytes.
func (e *dumpEstimate) scan(b []byte, n int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inTable += n
	for {
		i := bytes.Index(b, dataMarker)
		if i < 0 {
			return
		}
		rest := b[i+len(dataMarker):]
		end := bytes.IndexByte(rest, '`')
		if end < 0 {
			return
		}
		if name := string(rest[:end]); name != e.current {
			if t, ok := e.sizes[e.current]; ok {
				e.done += t.Bytes
			}
			e.current, e.inTable = name, int64(len(rest)-end)
		}
		b = rest[end:]
	}
}

// progress returns the percent (-1 when there is no estimate, never above 99
// while the dump runs, never going backwards) and the table being dumped.
func (e *dumpEstimate) progress() (int, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.total <= 0 {
		return -1, e.current
	}
	current := int64(0)
	if t, ok := e.sizes[e.current]; ok {
		current = min(e.inTable, t.Bytes)
	}
	pct := min(percent(e.done+current, e.total), 99)
	pct = max(pct, e.last, 0)
	e.last = pct
	return pct, e.current
}

// countingReader counts the bytes read from the file, for the progress of a load.
type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// loadDump feeds a dump file (plain, gzip or zstd) through sqlfilter into
// the mariadb client, logged in as the job account. --binary-mode and
// --sandbox turn off client commands such as \! (shell) and source, so
// only SQL reaches the server, where the job account can only touch the
// tenant's databases.
func (w *Worker) loadDump(ctx context.Context, acct *jobAccount, file, source, staging string, strict bool) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	counted := &countingReader{r: f}
	plain, err := uploads.Decompress(counted)
	if err != nil {
		return err
	}
	defer plain.Close()

	var mu sync.Mutex
	stepNow := "Starting the mariadb client"
	filter := &sqlfilter.Filter{Source: source, Staging: staging, OnTable: func(name string) {
		mu.Lock()
		stepNow = "Loading table " + name
		mu.Unlock()
	}}
	args := []string{"--binary-mode", "--sandbox", "--local-infile=0", "--batch", "--init-command=SET SESSION max_statement_time = 0"}
	if !strict {
		args = append(args, "--force")
	}
	cmd := acct.tool(ctx, "mariadb", append(args, "--database="+staging)...)
	var stderr stderrTail
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		err := filter.Copy(stdin, plain)
		stdin.Close()
		done <- err
	}()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	update := func() {
		mu.Lock()
		s := stepNow
		mu.Unlock()
		report(ctx, percent(counted.n.Load(), info.Size()), s)
	}
	update()
	var filterErr error
loop:
	for {
		select {
		case filterErr = <-done:
			break loop
		case <-t.C:
			update()
		}
	}
	waitErr := cmd.Wait()
	// a client that stopped at an error breaks the pipe, then its message is the useful one
	if filterErr != nil && !errors.Is(filterErr, syscall.EPIPE) {
		return fatalError{filterErr}
	}
	if waitErr != nil {
		return toolError("mariadb", waitErr, &stderr)
	}
	// with --force the client exits 0 after skipping failed statements
	if msg := stderr.String(); msg != "" {
		return fmt.Errorf("mariadb: %s", msg)
	}
	if filterErr != nil {
		return fatalError{filterErr}
	}
	return nil
}

// fatalError fails a load even when errors are skipped.
type fatalError struct{ error }

func (e fatalError) Unwrap() error { return e.error }
