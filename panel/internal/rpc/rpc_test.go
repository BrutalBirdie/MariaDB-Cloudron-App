package rpc

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"mdbpanel/internal/access"
	"mdbpanel/internal/names"
	"mdbpanel/internal/store"
)

type fake struct{ store.Catalog }

func (fake) ListRoles(context.Context) ([]store.Role, error) {
	return []store.Role{{Name: "shop", ConnLimit: 5, Owners: []access.Principal{{Kind: "user", ID: "ann"}}, Databases: []string{"shop"}}}, nil
}

func (fake) CreateDatabase(_ context.Context, name, owner string, limit int) error {
	switch name {
	case "full":
		return errors.Join(errors.New("x"), store.ErrLimitReached)
	case "Bad":
		return names.ErrInvalid
	case "dup":
		return &mysql.MySQLError{Number: 1007, Message: "Can't create database 'dup'; database exists"}
	case "user":
		return store.UserError("try again")
	}
	return errors.New("disk on fire")
}

func (fake) SetLoginPassword(_ context.Context, role, username, hash string) (string, error) {
	return role + "." + username + hash, nil
}

func TestRoundTrip(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "w.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &Server{Catalog: fake{}, AllowUID: os.Getuid(), Log: slog.New(slog.DiscardHandler)}
	go srv.Serve(ctx, sock)
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	c := NewClient(sock)
	roles, err := c.ListRoles(ctx)
	if err != nil || len(roles) != 1 || roles[0].Owners[0].ID != "ann" || roles[0].ConnLimit != 5 {
		t.Fatalf("roles %v %v", roles, err)
	}
	if l, err := c.SetLoginPassword(ctx, "shop", "ann", "*H"); err != nil || l != "shop.ann*H" {
		t.Fatalf("login %q %v", l, err)
	}
	var me *mysql.MySQLError
	var ue store.UserError
	checks := map[string]func(error) bool{
		"full": func(err error) bool { return errors.Is(err, store.ErrLimitReached) && strings.Contains(err.Error(), "x") },
		"Bad":  func(err error) bool { return errors.Is(err, names.ErrInvalid) },
		"dup":  func(err error) bool { return errors.As(err, &me) && me.Number == 1007 },
		"user": func(err error) bool { return errors.As(err, &ue) && ue.Error() == "try again" },
		"x":    func(err error) bool { return err != nil && strings.Contains(err.Error(), "disk on fire") },
	}
	for name, ok := range checks {
		if err := c.CreateDatabase(ctx, name, "shop", 0); !ok(err) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if err := NewClient(sock + "-missing").DropRole(ctx, "x"); err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("missing socket: %v", err)
	}
}

func TestPeerCheck(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "w.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &Server{Catalog: fake{}, AllowUID: os.Getuid() + 1, Log: slog.New(slog.DiscardHandler)}
	go srv.Serve(ctx, sock)
	time.Sleep(50 * time.Millisecond)
	if _, err := NewClient(sock).ListRoles(ctx); err == nil {
		t.Fatal("a foreign uid was served")
	}
}
