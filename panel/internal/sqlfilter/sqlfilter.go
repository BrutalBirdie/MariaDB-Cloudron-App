// Package sqlfilter adjusts a SQL dump on its way into a tenant database. It
// works line by line on the statement lines of mariadb-dump, mysqldump and
// phpMyAdmin files and leaves data lines alone:
//
//   - DEFINER clauses become DEFINER=CURRENT_ROLE, so views, triggers,
//     routines and events belong to the tenant role and not to an account of
//     the source server (which would need the SET USER privilege).
//   - CREATE DATABASE and USE of one source database are dropped, the dump is
//     loaded into the database chosen in the panel. A second database is an error.
//   - View definitions qualify columns with the source database name, they
//     are pointed at the staging database the dump is loaded into.
//   - MySQL GTID and binary log statements, which need admin privileges, are dropped.
package sqlfilter

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"regexp"
	"strings"
)

var ErrManyDatabases = errors.New("the file contains more than one database, dump a single database without --databases or --all-databases")

type Filter struct {
	Source  string            // database the dump was made from, "" to detect it from the file
	Staging string            // database the dump is loaded into
	OnTable func(name string) // called when the dump starts on the next table
}

var (
	identifier = "(?:`(?:[^`]|``)*`|'(?:[^'\\\\]|\\\\.|'')*'|\"(?:[^\"\\\\]|\\\\.)*\"|[A-Za-z0-9_.$%-]+)"
	definerRe  = regexp.MustCompile("(?i)DEFINER\\s*=\\s*(?:CURRENT_USER(?:\\s*\\(\\s*\\))?|CURRENT_ROLE(?:\\s*\\(\\s*\\))?|" + identifier + "(?:\\s*@\\s*" + identifier + ")?)")
	createDBRe = regexp.MustCompile("(?i)^CREATE\\s+(?:DATABASE|SCHEMA)\\s+(?:/\\*!\\d+\\s*IF\\s+NOT\\s+EXISTS\\s*\\*/\\s*|IF\\s+NOT\\s+EXISTS\\s+)?(" + identifier + ")")
	useRe      = regexp.MustCompile("(?i)^USE\\s+(" + identifier + ")\\s*;")
	headerDBRe = regexp.MustCompile("^-- Host: .*\\sDatabase: (\\S+)\\s*$")
	tableRe    = regexp.MustCompile("^-- (?:Dumping data for table|Table structure for table) `((?:[^`]|``)+)`")
	insertRe   = regexp.MustCompile("(?i)^INSERT\\s+(?:IGNORE\\s+)?INTO\\s+`((?:[^`]|``)+)`")
	gtidRe     = regexp.MustCompile("(?i)^(?:/\\*!\\d+\\s+)?SET\\s+@@(?:SESSION\\.|GLOBAL\\.)?(?:SQL_LOG_BIN|GTID_PURGED)\\s*=")
)

func unquote(id string) string {
	if len(id) >= 2 {
		switch q := id[0]; q {
		case '`':
			return strings.ReplaceAll(id[1:len(id)-1], "``", "`")
		case '\'', '"':
			return id[1 : len(id)-1]
		}
	}
	return id
}

func quote(id string) string { return "`" + strings.ReplaceAll(id, "`", "``") + "`" }

// statementLine reports whether a line starts a statement the filter adjusts.
// Data lines (INSERT, VALUES continuation) never do.
func statementLine(line []byte) bool {
	t := bytes.TrimLeft(line, " \t")
	return bytes.HasPrefix(t, []byte("/*!")) || hasPrefixFold(t, "CREATE") || hasPrefixFold(t, "ALTER")
}

func hasPrefixFold(b []byte, prefix string) bool {
	return len(b) >= len(prefix) && strings.EqualFold(string(b[:len(prefix)]), prefix)
}

func (f *Filter) useDatabase(name string) error {
	name = unquote(name)
	if f.Source == "" {
		f.Source = name
		return nil
	}
	if name != f.Source {
		return ErrManyDatabases
	}
	return nil
}

// Copy writes src to dst with the adjustments. Lines longer than the buffer
// are passed through unchanged, they are table data.
func (f *Filter) Copy(dst io.Writer, src io.Reader) error {
	r := bufio.NewReaderSize(src, 1<<20)
	w := bufio.NewWriterSize(dst, 1<<20)
	skipping, inLong := false, false
	for {
		line, err := r.ReadSlice('\n')
		full := errors.Is(err, bufio.ErrBufferFull)
		switch {
		case inLong || full:
			if !skipping {
				if _, werr := w.Write(line); werr != nil {
					return werr
				}
			} else if !full {
				skipping = !bytes.HasSuffix(bytes.TrimSpace(line), []byte(";"))
			}
			inLong = full
		case len(line) > 0:
			out, skip, ferr := f.line(line, skipping)
			if ferr != nil {
				return ferr
			}
			skipping = skip
			if _, werr := w.Write(out); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return w.Flush()
		}
		if err != nil && !full {
			return err
		}
	}
}

// line returns the line to write and whether the following lines belong to a
// dropped statement.
func (f *Filter) line(line []byte, skipping bool) ([]byte, bool, error) {
	trimmed := bytes.TrimSpace(line)
	if skipping {
		return nil, !bytes.HasSuffix(trimmed, []byte(";")), nil
	}
	if m := headerDBRe.FindSubmatch(trimmed); m != nil && f.Source == "" {
		f.Source = string(m[1])
		return line, false, nil
	}
	if m := tableRe.FindSubmatch(trimmed); m != nil {
		f.table(string(m[1]))
		return line, false, nil
	}
	if m := insertRe.FindSubmatch(trimmed); m != nil {
		f.table(string(m[1]))
		return line, false, nil
	}
	if m := createDBRe.FindSubmatch(trimmed); m != nil {
		if err := f.useDatabase(string(m[1])); err != nil {
			return nil, false, err
		}
		return []byte("-- dropped by the panel: CREATE DATABASE\n"), !bytes.HasSuffix(trimmed, []byte(";")), nil
	}
	if m := useRe.FindSubmatch(trimmed); m != nil {
		if err := f.useDatabase(string(m[1])); err != nil {
			return nil, false, err
		}
		return []byte("-- dropped by the panel: USE\n"), false, nil
	}
	if gtidRe.Match(trimmed) {
		return []byte("-- dropped by the panel: needs admin privileges\n"), !bytes.HasSuffix(trimmed, []byte(";")), nil
	}
	if !statementLine(trimmed) {
		return line, false, nil
	}
	out := definerRe.ReplaceAll(line, []byte("DEFINER=CURRENT_ROLE"))
	if f.Source != "" && f.Staging != "" && f.Source != f.Staging && bytes.Contains(bytes.ToUpper(out), []byte(" VIEW ")) {
		out = bytes.ReplaceAll(out, []byte(quote(f.Source)+"."), []byte(quote(f.Staging)+"."))
	}
	return out, false, nil
}

func (f *Filter) table(quoted string) {
	if f.OnTable != nil {
		f.OnTable(strings.ReplaceAll(quoted, "``", "`"))
	}
}

// RewriteDefiner replaces the DEFINER clause of a statement printed by SHOW
// CREATE with DEFINER=CURRENT_ROLE. It reports false when there is none.
func RewriteDefiner(ddl string) (string, bool) {
	loc := definerRe.FindStringIndex(ddl)
	if loc == nil {
		return ddl, false
	}
	return ddl[:loc[0]] + "DEFINER=CURRENT_ROLE" + ddl[loc[1]:], true
}
