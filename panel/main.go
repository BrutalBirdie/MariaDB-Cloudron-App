package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mdbpanel/internal/access"
	"mdbpanel/internal/allowlist"
	"mdbpanel/internal/apppw"
	"mdbpanel/internal/directory"
	"mdbpanel/internal/notify"
	"mdbpanel/internal/proxy"
	"mdbpanel/internal/rpc"
	"mdbpanel/internal/store"
	"mdbpanel/internal/tuning"
	"mdbpanel/internal/web"
	"mdbpanel/internal/worker"
)

var (
	snapshotDir   = env("SNAPSHOT_DIR", "/var/lib/mysql/snapshots")
	uploadDir     = env("UPLOAD_DIR", "/app/data/uploads")
	socket        = env("MARIADB_SOCKET", "/var/lib/mysql/run/mysqld.sock")
	rpcSocket     = env("WORKER_SOCKET", "/run/mdb/rpc/worker.sock")
	allowlistFile = env("ALLOWLIST_FILE", "/app/data/allowed_networks.txt")
)

// appPasswordMap is read by pam-auth without the app's environment, so its path is fixed.
const appPasswordMap = "/run/mdb/panel/apppw.json"

const tuningFile = "/run/mdb/conf/tuning.cnf"

func uploadRetention() time.Duration {
	n, err := strconv.Atoi(os.Getenv("MARIADB_UPLOAD_RETENTION_DAYS"))
	if err != nil || n < 1 {
		n = 8
	}
	return time.Duration(n) * 24 * time.Hour
}

func scimClient() *directory.Client {
	ttl, err := time.ParseDuration(os.Getenv("PANEL_SCIM_CACHE"))
	if err != nil || ttl <= 0 {
		ttl = 2 * time.Minute
	}
	return directory.New(os.Getenv("CLOUDRON_SCIM_ORIGIN"), os.Getenv("CLOUDRON_SCIM_TOKEN"), ttl)
}

func accessSyncInterval() time.Duration { return durationEnv("PANEL_ACCESS_SYNC", time.Minute) }

func durationEnv(key string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(os.Getenv(key))
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func splitList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
}

func admins() access.Admins {
	return access.Admins{Users: splitList(os.Getenv("MARIADB_ADMINS")), Groups: splitList(env("MARIADB_ADMIN_GROUP", "mariadb-admins"))}
}

func maxSnapshots() int {
	n, err := strconv.Atoi(os.Getenv("MARIADB_MAX_SNAPSHOTS"))
	if err != nil || n < 1 {
		return 7
	}
	return n
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "worker":
		err = runWorker(log)
	case "proxy":
		err = runProxy(log)
	case "migrate":
		err = migrate()
	case "pam-auth":
		if err = pamAuth(); err != nil {
			fmt.Fprintf(os.Stderr, "pam-auth: login %q: %v\n", os.Getenv("PAM_USER"), err)
			os.Exit(1)
		}
		return
	case "render-tuning":
		err = renderTuning(os.Args[2:])
	case "":
		err = serve(log)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		log.Error(cmd+" stopped", "err", err)
		os.Exit(1)
	}
}

// migrate creates the panel's tables, start.sh runs it as cloudron while
// MariaDB only listens on the socket.
func migrate() error {
	st, err := store.Open(socket, "cloudron")
	if err != nil {
		return err
	}
	defer st.Close()
	return st.CreateSchema(context.Background())
}

// runWorker runs as the mysql OS user, see package worker.
func runWorker(log *slog.Logger) error {
	// snapshot files and directories must be usable by the panel's group
	syscall.Umask(0o007)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	st, err := store.Open(socket, "mysql")
	if err != nil {
		return err
	}
	defer st.Close()
	// supervisor starts MariaDB and the worker at the same time
	for deadline := time.Now().Add(120 * time.Second); ; {
		err = st.Ping(ctx)
		if err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return err
		}
		time.Sleep(time.Second)
	}
	panelUser, err := user.Lookup(env("PANEL_USER", "cloudron"))
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(panelUser.Uid)
	srv := &rpc.Server{Catalog: st, AllowUID: uid, Log: log}
	go func() {
		if err := srv.Serve(ctx, rpcSocket); err != nil {
			log.Error("rpc server stopped", "err", err)
			stop()
		}
	}()
	w := worker.New(worker.Config{Directory: scimClient(), Admins: admins(), SnapshotDir: snapshotDir, UploadDir: uploadDir, Socket: socket,
		MaxSnapshots: maxSnapshots(), Mailer: notify.FromEnv(), Origin: os.Getenv("CLOUDRON_APP_ORIGIN"), TuningFile: tuningFile}, st, log)
	go w.AccessSync(ctx, accessSyncInterval())
	go w.ApplyTuning(ctx)
	go w.NotifyFailures(ctx, durationEnv("PANEL_NOTIFY_INTERVAL", 30*time.Second))
	return w.Run(ctx)
}

func runProxy(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	list, errs := allowlist.ReadFile(allowlistFile)
	for _, err := range errs {
		log.Warn("ignoring invalid allowlist entry", "err", err)
	}
	for _, p := range list {
		if p.Bits() == 0 {
			log.Warn("the allowlist opens MariaDB to the whole internet", "network", p.String())
		}
	}
	p := &proxy.Proxy{Listen: env("PROXY_LISTEN", ":3306"), Backend: env("PROXY_BACKEND", "127.0.0.1:3307"), Allowlist: allowlistFile, Log: log}
	return p.Run(ctx)
}

// pamAuth is run by pam_exec for logins that use app passwords, see /etc/pam.d/mdbpanel.
func pamAuth() error {
	if t := os.Getenv("PAM_TYPE"); t != "auth" {
		return fmt.Errorf("unexpected PAM_TYPE %q", t)
	}
	m, err := apppw.ReadMap(appPasswordMap)
	if err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
	if err != nil {
		return err
	}
	return apppw.Verify(context.Background(), m, http.DefaultClient, os.Getenv("PAM_USER"), apppw.ReadPassword(b))
}

// renderTuning is called by start.sh as mysql before MariaDB starts, see worker.ApplyTuning.
func renderTuning(args []string) error {
	fs := flag.NewFlagSet("render-tuning", flag.ExitOnError)
	out := fs.String("out", tuningFile, "include file to write")
	fs.Parse(args)

	mem := tuning.MemoryMB()
	content, _ := tuning.Render(nil, mem)
	st, err := store.Open(socket, "mysql")
	if err == nil {
		defer st.Close()
		var values map[string]string
		var invalid []error
		values, invalid, err = worker.StoredTuning(context.Background(), st)
		for _, e := range invalid {
			fmt.Fprintln(os.Stderr, "==> Ignoring server variable:", e)
		}
		if err == nil {
			content, err = tuning.Render(values, mem)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "==> No server variables from the panel:", err)
		content, _ = tuning.Render(nil, mem)
	}
	return allowlist.WriteFileAtomic(*out, content, 0o644)
}

func serve(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	origin := os.Getenv("CLOUDRON_APP_ORIGIN")
	if origin == "" {
		return errors.New("CLOUDRON_APP_ORIGIN is not set")
	}
	if os.Getenv("CLOUDRON_OIDC_ISSUER") == "" {
		return errors.New("CLOUDRON_OIDC_ISSUER is not set, the oidc addon is required")
	}

	st, err := store.Open(socket, "cloudron")
	if err != nil {
		return err
	}
	defer st.Close()
	// supervisor starts MariaDB and the panel at the same time
	for deadline := time.Now().Add(120 * time.Second); ; {
		err = st.Ping(ctx)
		if err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("connecting to mariadb: %w", err)
		}
		log.Info("waiting for mariadb", "err", err)
		time.Sleep(2 * time.Second)
	}

	srv, err := web.New(web.Config{
		Origin:        origin,
		PublicHost:    os.Getenv("CLOUDRON_APP_DOMAIN"),
		PublicPort:    os.Getenv("MARIADB_PORT"),
		Issuer:        os.Getenv("CLOUDRON_OIDC_ISSUER"),
		ClientID:      os.Getenv("CLOUDRON_OIDC_CLIENT_ID"),
		ClientSecret:  os.Getenv("CLOUDRON_OIDC_CLIENT_SECRET"),
		Directory:     scimClient(),
		Admins:        admins(),
		AllowlistFile: allowlistFile,
		SnapshotDir:   snapshotDir,
		UploadDir:     uploadDir,
		UploadKeep:    uploadRetention(),
		MaxSnapshots:  maxSnapshots(),
		Mailer:        notify.FromEnv(),
		BridgeOrigin:  os.Getenv("CLOUDRON_APP_BRIDGE_ORIGIN"),
		DashboardURL:  os.Getenv("CLOUDRON_WEBADMIN_ORIGIN"),
		AppPWMap:      appPasswordMap,
	}, st, rpc.NewClient(rpcSocket), log)
	if err != nil {
		return err
	}
	if err := srv.WriteAppPasswordMap(ctx); err != nil {
		return fmt.Errorf("writing the app password map: %w", err)
	}
	go srv.PurgeLoop(ctx)
	go srv.HashLoop(ctx)

	httpServer := &http.Server{
		Addr:              env("PANEL_LISTEN", "127.0.0.1:3000"),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// ends the live update streams on shutdown, Shutdown does not cancel running requests
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpServer.Shutdown(shutdownCtx)
	}()
	log.Info("panel listening", "addr", httpServer.Addr)
	if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
