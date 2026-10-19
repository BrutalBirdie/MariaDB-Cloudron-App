// Package rpc lets the panel use the worker's store.Catalog. The worker runs
// as the MariaDB superuser, the panel only gets the operations listed here
// and the worker checks every name again. The socket is only open to the
// panel's OS user, checked with SO_PEERCRED on every connection.
package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/go-sql-driver/mysql"

	"mdbpanel/internal/access"
	"mdbpanel/internal/names"
	"mdbpanel/internal/store"
)

type createRole struct {
	Name         string
	ConnLimit    int
	Owners       []access.Principal
	MaxDatabases int
}

type createDatabase struct {
	Name, Owner string
	Limit       int
}

type login struct {
	Role, Username, Hash, Login string
}

type byName struct {
	Name  string
	Names []string
}

type handler func(ctx context.Context, c store.Catalog, body []byte) (any, error)

func call[A any](fn func(ctx context.Context, c store.Catalog, a A) (any, error)) handler {
	return func(ctx context.Context, c store.Catalog, body []byte) (any, error) {
		var a A
		if err := json.Unmarshal(body, &a); err != nil {
			return nil, err
		}
		return fn(ctx, c, a)
	}
}

var methods = map[string]handler{
	"ListRoles": call(func(ctx context.Context, c store.Catalog, _ struct{}) (any, error) { return c.ListRoles(ctx) }),
	"ListDatabases": call(func(ctx context.Context, c store.Catalog, _ struct{}) (any, error) {
		return c.ListDatabases(ctx)
	}),
	"CreateRole": call(func(ctx context.Context, c store.Catalog, a createRole) (any, error) {
		return nil, c.CreateRole(ctx, a.Name, a.ConnLimit, a.Owners, a.MaxDatabases)
	}),
	"UpdateRole": call(func(ctx context.Context, c store.Catalog, a createRole) (any, error) {
		return nil, c.UpdateRole(ctx, a.Name, a.Owners, a.ConnLimit, a.MaxDatabases)
	}),
	"DropRole": call(func(ctx context.Context, c store.Catalog, a byName) (any, error) { return nil, c.DropRole(ctx, a.Name) }),
	"CreateDatabase": call(func(ctx context.Context, c store.Catalog, a createDatabase) (any, error) {
		return nil, c.CreateDatabase(ctx, a.Name, a.Owner, a.Limit)
	}),
	"DropDatabase": call(func(ctx context.Context, c store.Catalog, a byName) (any, error) { return nil, c.DropDatabase(ctx, a.Name) }),
	"SetLoginPassword": call(func(ctx context.Context, c store.Catalog, a login) (any, error) {
		return c.SetLoginPassword(ctx, a.Role, a.Username, a.Hash)
	}),
	"UseAppPassword": call(func(ctx context.Context, c store.Catalog, a login) (any, error) {
		return c.UseAppPassword(ctx, a.Role, a.Username)
	}),
	"RevokeLogin": call(func(ctx context.Context, c store.Catalog, a login) (any, error) { return nil, c.RevokeLogin(ctx, a.Login) }),
	"DatabaseStats": call(func(ctx context.Context, c store.Catalog, a byName) (any, error) {
		return c.DatabaseStats(ctx, a.Name)
	}),
	"Connections": call(func(ctx context.Context, c store.Catalog, _ struct{}) (any, error) { return c.Connections(ctx) }),
	"ServerSettings": call(func(ctx context.Context, c store.Catalog, a byName) (any, error) {
		return c.ServerSettings(ctx, a.Names)
	}),
}

type reply struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Kind   string          `json:"kind,omitempty"`
	Code   uint16          `json:"code,omitempty"`
}

var kinds = map[string]error{
	"not_found":     store.ErrNotFound,
	"has_databases": store.ErrHasDatabases,
	"limit":         store.ErrLimitReached,
	"job_active":    store.ErrJobActive,
	"invalid_name":  names.ErrInvalid,
	"reserved_name": names.ErrReserved,
}

func encodeError(err error) reply {
	r := reply{Error: err.Error(), Kind: "internal"}
	var me *mysql.MySQLError
	var ue store.UserError
	switch {
	case errors.As(err, &me):
		r.Kind, r.Code, r.Error = "mysql", me.Number, me.Message
	case errors.As(err, &ue):
		r.Kind = "user"
	default:
		for k, base := range kinds {
			if errors.Is(err, base) {
				r.Kind = k
			}
		}
	}
	return r
}

// remoteError keeps the worker's message and the sentinel it wraps.
type remoteError struct {
	msg  string
	base error
}

func (e *remoteError) Error() string { return e.msg }
func (e *remoteError) Unwrap() error { return e.base }

func decodeError(r reply) error {
	switch r.Kind {
	case "mysql":
		return &mysql.MySQLError{Number: r.Code, Message: r.Error}
	case "user":
		return store.UserError(r.Error)
	}
	if base, ok := kinds[r.Kind]; ok {
		return &remoteError{msg: r.Error, base: base}
	}
	return fmt.Errorf("worker: %s", r.Error)
}

type Server struct {
	Catalog  store.Catalog
	AllowUID int // the panel's OS user
	Log      *slog.Logger
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fn, ok := methods[r.URL.Path[1:]]
	if r.Method != http.MethodPost || !ok {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var rep reply
	if err == nil {
		var res any
		if res, err = fn(r.Context(), s.Catalog, body); err == nil {
			rep.Result, err = json.Marshal(res)
		}
	}
	if err != nil {
		rep = encodeError(err)
		if rep.Kind == "internal" {
			s.Log.Error("rpc call failed", "method", r.URL.Path[1:], "err", err)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rep)
}

// Serve listens on a unix socket in a directory the panel's group can enter.
func (s *Server) Serve(ctx context.Context, path string) error {
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	err = srv.Serve(&peerListener{Listener: ln, uid: s.AllowUID, log: s.Log})
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type peerListener struct {
	net.Listener
	uid int
	log *slog.Logger
}

func (l *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uid, err := peerUID(c)
		if err == nil && uid == l.uid {
			return c, nil
		}
		l.log.Warn("rpc connection refused", "uid", uid, "err", err)
		c.Close()
	}
}

func peerUID(c net.Conn) (int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return -1, errors.New("not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return -1, err
	}
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return -1, err
	}
	if credErr != nil {
		return -1, credErr
	}
	return int(cred.Uid), nil
}

// Client is the panel's store.Catalog.
type Client struct {
	http *http.Client
}

func NewClient(path string) *Client {
	return &Client{http: &http.Client{
		Timeout: 5 * time.Minute,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return new(net.Dialer).DialContext(ctx, "unix", path)
		}},
	}}
}

func (c *Client) do(ctx context.Context, method string, args, result any) error {
	if args == nil {
		args = struct{}{}
	}
	body, err := json.Marshal(args)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://worker/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("the worker is not reachable: %w", err)
	}
	defer res.Body.Close()
	var rep reply
	if err := json.NewDecoder(res.Body).Decode(&rep); err != nil {
		return fmt.Errorf("worker reply: HTTP %d: %w", res.StatusCode, err)
	}
	if rep.Error != "" {
		return decodeError(rep)
	}
	if result != nil {
		return json.Unmarshal(rep.Result, result)
	}
	return nil
}

func (c *Client) ListRoles(ctx context.Context) ([]store.Role, error) {
	var out []store.Role
	return out, c.do(ctx, "ListRoles", nil, &out)
}

func (c *Client) ListDatabases(ctx context.Context) ([]store.Database, error) {
	var out []store.Database
	return out, c.do(ctx, "ListDatabases", nil, &out)
}

func (c *Client) CreateRole(ctx context.Context, name string, connLimit int, owners []access.Principal, maxDatabases int) error {
	return c.do(ctx, "CreateRole", createRole{name, connLimit, owners, maxDatabases}, nil)
}

func (c *Client) UpdateRole(ctx context.Context, name string, owners []access.Principal, connLimit, maxDatabases int) error {
	return c.do(ctx, "UpdateRole", createRole{name, connLimit, owners, maxDatabases}, nil)
}

func (c *Client) DropRole(ctx context.Context, name string) error {
	return c.do(ctx, "DropRole", byName{Name: name}, nil)
}

func (c *Client) CreateDatabase(ctx context.Context, name, owner string, limit int) error {
	return c.do(ctx, "CreateDatabase", createDatabase{name, owner, limit}, nil)
}

func (c *Client) DropDatabase(ctx context.Context, name string) error {
	return c.do(ctx, "DropDatabase", byName{Name: name}, nil)
}

func (c *Client) SetLoginPassword(ctx context.Context, role, username, hash string) (string, error) {
	var out string
	return out, c.do(ctx, "SetLoginPassword", login{Role: role, Username: username, Hash: hash}, &out)
}

func (c *Client) UseAppPassword(ctx context.Context, role, username string) (string, error) {
	var out string
	return out, c.do(ctx, "UseAppPassword", login{Role: role, Username: username}, &out)
}

func (c *Client) RevokeLogin(ctx context.Context, l string) error {
	return c.do(ctx, "RevokeLogin", login{Login: l}, nil)
}

func (c *Client) DatabaseStats(ctx context.Context, name string) (map[string]store.DatabaseStats, error) {
	out := map[string]store.DatabaseStats{}
	return out, c.do(ctx, "DatabaseStats", byName{Name: name}, &out)
}

func (c *Client) Connections(ctx context.Context) ([]store.Connection, error) {
	var out []store.Connection
	return out, c.do(ctx, "Connections", nil, &out)
}

func (c *Client) ServerSettings(ctx context.Context, vars []string) (map[string]string, error) {
	out := map[string]string{}
	return out, c.do(ctx, "ServerSettings", byName{Names: vars}, &out)
}
