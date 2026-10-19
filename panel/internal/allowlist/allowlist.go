// Package allowlist reads and writes the networks allowed to reach the
// public MariaDB port. The proxy enforces it, see package proxy.
package allowlist

import (
	"bufio"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
)

// ParseEntry accepts an address or a CIDR and returns the masked prefix.
func ParseEntry(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("%q is not a valid network", s)
		}
		if p.Addr().Is4In6() {
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" {
		return netip.Prefix{}, fmt.Errorf("%q is not a valid IP address", s)
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// Parse reads the allowlist file format: one entry per line, # starts a comment.
// Invalid lines are returned as errors and skipped.
func Parse(r io.Reader) ([]netip.Prefix, []error) {
	var list []netip.Prefix
	var errs []error
	seen := map[netip.Prefix]bool{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), "#")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		p, err := ParseEntry(line)
		if err != nil {
			errs = append(errs, fmt.Errorf("line %d: %w", n, err))
			continue
		}
		if !seen[p] {
			seen[p] = true
			list = append(list, p)
		}
	}
	if err := sc.Err(); err != nil {
		errs = append(errs, err)
	}
	return list, errs
}

func ReadFile(path string) ([]netip.Prefix, []error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{err}
	}
	defer f.Close()
	return Parse(f)
}

const allowlistHeader = `# Networks allowed to connect to MariaDB from outside, one per line.
# Managed by the admin panel. Manual edits apply to new connections at once.
`

func FormatAllowlist(list []netip.Prefix) string {
	var b strings.Builder
	b.WriteString(allowlistHeader)
	for _, p := range list {
		b.WriteString(p.String())
		b.WriteByte('\n')
	}
	return b.String()
}

// WriteFileAtomic replaces path via rename so readers never see a partial file.
func WriteFileAtomic(path, content string, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
