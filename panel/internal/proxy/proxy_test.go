package proxy

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHeader(t *testing.T) {
	cases := map[string][2]string{
		"PROXY TCP4 203.0.113.7 172.18.0.5 50000 3306\r\n":        {"203.0.113.7:50000", "172.18.0.5:3306"},
		"PROXY TCP4 203.0.113.7 172.18.0.5 50000 3306\r\n ":       {"[::ffff:203.0.113.7]:50000", "[::ffff:172.18.0.5]:3306"},
		"PROXY TCP6 2001:db8::1 2001:db8::2 50000 3306\r\n":       {"[2001:db8::1]:50000", "[2001:db8::2]:3306"},
		"PROXY TCP6 2001:db8::1 ::ffff:172.18.0.5 50000 3306\r\n": {"[2001:db8::1]:50000", "172.18.0.5:3306"},
	}
	for want, in := range cases {
		got := Header(netip.MustParseAddrPort(in[0]), netip.MustParseAddrPort(in[1]))
		if got != strings.TrimSuffix(want, " ") {
			t.Errorf("%v: got %q want %q", in, got, want)
		}
	}
}

func TestHostNotAllowed(t *testing.T) {
	p := HostNotAllowed("192.0.2.1")
	n := int(p[0]) | int(p[1])<<8 | int(p[2])<<16
	if n != len(p)-4 || p[3] != 0 || p[4] != 0xff || p[5] != 0x6a || p[6] != 0x04 {
		t.Fatalf("bad packet % x", p)
	}
	if !strings.HasSuffix(string(p), "Host '192.0.2.1' is not allowed to connect to this MariaDB server") {
		t.Fatalf("bad message %q", p[7:])
	}
}

func TestProxy(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	headers := make(chan string, 4)
	go func() {
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				h, _ := r.ReadString('\n')
				headers <- h
				io.Copy(c, r)
			}()
		}
	}()

	dir := t.TempDir()
	list := filepath.Join(dir, "allowed.txt")
	os.WriteFile(list, []byte("# none\n"), 0o644)
	p := &Proxy{Listen: "127.0.0.1:0", Backend: backend.Addr().String(), Allowlist: list, Log: slog.New(slog.DiscardHandler)}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.Listen = ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	time.Sleep(100 * time.Millisecond)

	c, err := net.Dial("tcp", p.Listen)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(c)
	c.Close()
	if !strings.Contains(string(got), "Host '127.0.0.1' is not allowed") {
		t.Fatalf("rejected client got %q", got)
	}

	// the file is reloaded on change, without a restart
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(list, []byte("127.0.0.0/8\n"), 0o644)
	c, err = net.Dial("tcp", p.Listen)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("ping"))
	buf := make([]byte, 4)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo failed: %q %v", buf, err)
	}
	if h := <-headers; !strings.HasPrefix(h, "PROXY TCP4 127.0.0.1 127.0.0.1 ") {
		t.Fatalf("header %q", h)
	}
}
