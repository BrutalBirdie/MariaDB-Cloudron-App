// Package uploads handles dump files in /app/data/uploads/<role>/. Files get
// there through the panel or the Cloudron File Manager. The directories are
// owned by cloudron (panel) with setgid group mysql, so the worker can read.
package uploads

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)

// ValidName rejects hidden files (in-progress uploads use a dot prefix) and
// anything that could be read as a path.
func ValidName(name string) bool {
	return namePattern.MatchString(name) && !strings.Contains(name, "..")
}

// SanitizeName turns a browser supplied file name into a valid one.
func SanitizeName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	var b strings.Builder
	for _, r := range name {
		if r < 128 && (r == '.' || r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	s := strings.TrimLeft(strings.ReplaceAll(b.String(), "..", "_"), "._-")
	if len(s) > 200 {
		s = s[len(s)-200:]
	}
	if s == "" {
		s = "upload.sql"
	}
	return s
}

type File struct {
	Name    string
	Size    int64
	ModTime time.Time
}

// Scan lists the valid regular files of a role's upload directory.
func Scan(dir string) ([]File, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []File
	for _, e := range entries {
		if !e.Type().IsRegular() || !ValidName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, File{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime()})
	}
	return files, nil
}

// Path returns the file path inside root/role, refusing anything else.
func Path(root, role, name string) (string, error) {
	if !ValidName(name) || !ValidName(role) {
		return "", fmt.Errorf("invalid upload name %q", name)
	}
	return filepath.Join(root, role, name), nil
}

// Format detects the compression of a SQL dump by its magic bytes and checks
// that the content looks like SQL.
func Format(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := make([]byte, 4)
	n, _ := io.ReadFull(f, head)
	format := "sql"
	switch {
	case n == 4 && bytes.Equal(head, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		format = "sql.zst"
	case n >= 2 && bytes.Equal(head[:2], []byte{0x1f, 0x8b}):
		format = "sql.gz"
	}
	r, err := Open(path)
	if err != nil {
		return "", err
	}
	defer r.Close()
	text := make([]byte, 4096)
	n, err = io.ReadFull(r, text)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("the file cannot be decompressed: %w", err)
	}
	if !looksLikeSQL(text[:n]) {
		return "", errors.New("not a SQL dump, use mariadb-dump or mysqldump, optionally compressed with gzip or zstd")
	}
	return format, nil
}

// Open returns the decompressed content of a dump file.
func Open(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r, err := Decompress(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{r, closers{r, f}}, nil
}

type closers []io.Closer

func (c closers) Close() error {
	var errs []error
	for _, x := range c {
		errs = append(errs, x.Close())
	}
	return errors.Join(errs...)
}

// Decompress detects gzip and zstd and returns a reader of the plain content.
func Decompress(r io.Reader) (io.ReadCloser, error) {
	br := bufio.NewReader(r)
	head, _ := br.Peek(4)
	switch {
	case bytes.Equal(head, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		d, err := zstd.NewReader(br, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, err
		}
		return d.IOReadCloser(), nil
	case len(head) >= 2 && bytes.Equal(head[:2], []byte{0x1f, 0x8b}):
		return gzip.NewReader(br)
	}
	return io.NopCloser(br), nil
}

func looksLikeSQL(head []byte) bool {
	if bytes.IndexByte(head, 0) >= 0 {
		return false
	}
	s := strings.TrimSpace(string(head))
	for _, p := range []string{"--", "/*", "SET ", "CREATE ", "DROP ", "INSERT ", "LOCK ", "USE ", "START TRANSACTION", "BEGIN"} {
		if len(s) >= len(p) && strings.EqualFold(s[:len(p)], p) {
			return true
		}
	}
	return false
}

// HashFile returns the hex SHA-256 of a file.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// NormalizeSHA256 accepts the output of sha256sum ("<hash>  <file>") and
// PowerShell's Get-FileHash (upper case) and returns the lower case hash.
func NormalizeSHA256(s string) (string, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return "", nil
	}
	h := strings.ToLower(fields[0])
	if !sha256Pattern.MatchString(h) {
		return "", errors.New("the SHA-256 checksum must be 64 hexadecimal characters")
	}
	return h, nil
}
