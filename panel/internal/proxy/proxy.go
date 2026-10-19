// Package proxy is the public MariaDB port. It accepts connections from the
// allowed networks only and forwards them to MariaDB on localhost with a
// PROXY protocol header, so MariaDB sees and logs the real client address.
// TLS is negotiated inside the MariaDB protocol, end to end with the server.
// https://mariadb.com/docs/server/clients-and-utilities/server-client-software/client-libraries/proxy-protocol-support
package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"mdbpanel/internal/allowlist"
)

type Proxy struct {
	Listen    string // e.g. :3306
	Backend   string // e.g. 127.0.0.1:3307, must be in proxy_protocol_networks
	Allowlist string // file, re-read when it changes
	Log       *slog.Logger

	mu       sync.Mutex
	stamp    string
	allowed  []netip.Prefix
	lastWarn map[netip.Addr]time.Time
}

// Allowed reports whether addr may connect, reloading the allowlist file
// when its size or modification time changed.
func (p *Proxy) Allowed(addr netip.Addr) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	stamp := ""
	if info, err := os.Stat(p.Allowlist); err == nil {
		stamp = fmt.Sprintf("%d %d", info.Size(), info.ModTime().UnixNano())
	}
	if stamp != p.stamp {
		list, errs := allowlist.ReadFile(p.Allowlist)
		for _, err := range errs {
			p.Log.Warn("ignoring invalid allowlist entry", "err", err)
		}
		p.allowed, p.stamp = list, stamp
		p.Log.Info("allowlist loaded", "networks", len(list))
	}
	addr = addr.Unmap()
	for _, n := range p.allowed {
		if n.Contains(addr) {
			return true
		}
	}
	return false
}

func (p *Proxy) Run(ctx context.Context) error {
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", p.Listen)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	p.Log.Info("proxy listening", "addr", p.Listen, "backend", p.Backend)
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return err
		}
		go p.handle(ctx, c.(*net.TCPConn))
	}
}

func (p *Proxy) handle(ctx context.Context, c *net.TCPConn) {
	defer c.Close()
	remote := c.RemoteAddr().(*net.TCPAddr).AddrPort()
	if !p.Allowed(remote.Addr()) {
		p.warnRejected(remote.Addr())
		c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		c.Write(HostNotAllowed(remote.Addr().Unmap().String()))
		return
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	b, err := d.DialContext(ctx, "tcp", p.Backend)
	if err != nil {
		p.Log.Error("connecting to mariadb", "err", err)
		return
	}
	backend := b.(*net.TCPConn)
	defer backend.Close()
	c.SetKeepAlive(true)
	c.SetKeepAlivePeriod(60 * time.Second)
	local := c.LocalAddr().(*net.TCPAddr).AddrPort()
	if _, err := backend.Write([]byte(Header(remote, local))); err != nil {
		p.Log.Error("writing proxy header", "err", err)
		return
	}
	done := make(chan struct{}, 2)
	pipe := func(dst, src *net.TCPConn) {
		io.Copy(dst, src)
		dst.CloseWrite()
		done <- struct{}{}
	}
	go pipe(backend, c)
	go pipe(c, backend)
	<-done
	<-done
}

// warnRejected logs a rejected address at most once a minute.
func (p *Proxy) warnRejected(addr netip.Addr) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastWarn == nil {
		p.lastWarn = map[netip.Addr]time.Time{}
	}
	if time.Since(p.lastWarn[addr]) < time.Minute {
		return
	}
	if len(p.lastWarn) > 10000 {
		clear(p.lastWarn)
	}
	p.lastWarn[addr] = time.Now()
	p.Log.Warn("connection rejected, address not in the allowlist", "addr", addr.Unmap().String())
}

// Header is a PROXY protocol v1 header.
// https://www.haproxy.org/download/2.9/doc/proxy-protocol.txt
func Header(src, dst netip.AddrPort) string {
	s, d := src.Addr().Unmap(), dst.Addr().Unmap()
	family := "TCP6"
	if s.Is4() && d.Is4() {
		family = "TCP4"
	} else {
		s, d = netip.AddrFrom16(s.As16()), netip.AddrFrom16(d.As16())
	}
	return fmt.Sprintf("PROXY %s %s %s %d %d\r\n", family, s, d, src.Port(), dst.Port())
}

// HostNotAllowed is the error packet MariaDB itself sends instead of its
// greeting to hosts without an account (ER_HOST_NOT_PRIVILEGED), so clients
// show a readable message.
// https://mariadb.com/docs/server/reference/clientserver-protocol/4-server-response-packets/err_packet
func HostNotAllowed(host string) []byte {
	msg := fmt.Sprintf("Host '%s' is not allowed to connect to this MariaDB server", host)
	payload := make([]byte, 3, 3+len(msg))
	payload[0] = 0xff
	binary.LittleEndian.PutUint16(payload[1:], 1130)
	payload = append(payload, msg...)
	packet := []byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), 0}
	return append(packet, payload...)
}
