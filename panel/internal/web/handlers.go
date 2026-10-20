package web

import (
	"context"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"syscall"

	"mdbpanel/internal/allowlist"
	"mdbpanel/internal/notify"
	"mdbpanel/internal/store"
	"mdbpanel/internal/tuning"
	"mdbpanel/internal/worker"
)

func (s *Server) deleteDatabase(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx := r.Context()
	db, err := s.authorizeDatabase(r, name)
	if err != nil {
		s.userError(w, r, err)
		return
	}
	if r.PostForm.Get("confirm") != name {
		s.userError(w, r, badInput("Type the database name to confirm the deletion."))
		return
	}
	if err := s.cat.DropDatabase(ctx, name); err != nil {
		s.userError(w, r, err)
		return
	}
	s.audit(r, "database.drop", name, "owner="+db.Owner)
	if err := s.store.DeleteSchedule(ctx, name); err != nil {
		s.log.Error("deleting schedule of dropped database", "database", name, "err", err)
	}
	files, err := s.store.DeleteSnapshotsOf(ctx, name)
	if err != nil {
		s.log.Error("deleting snapshots of dropped database", "database", name, "err", err)
	}
	worker.RemoveFiles(s.cfg.SnapshotDir, files, s.log)
	if dir, err := worker.Resolve(s.cfg.SnapshotDir, name); err == nil {
		os.Remove(dir)
	}
	http.Redirect(w, r, s.afterDelete(r, db), http.StatusSeeOther)
}

type adminTab struct{ ID, Label, Icon string }

var adminTabs = []adminTab{{"overview", "Overview", "layout-dashboard"}, {"roles", "Roles", "users"}, {"databases", "Databases", "database"},
	{"network", "Network", "globe"}, {"notifications", "Notifications", "mail"}, {"server", "Server", "server"}, {"audit", "Audit log", "list"}}

// adminViews maps every admin page to the tab it belongs to. Connections and
// snapshots open from the overview tiles.
var adminViews = map[string]string{"overview": "overview", "connections": "overview", "snapshots": "overview", "roles": "roles",
	"databases": "databases", "network": "network", "notifications": "notifications", "server": "server", "audit": "audit"}

const auditPageSize = 50

type filterOption struct {
	Value, Label string
	Mono         bool
}

type paramView struct {
	Name, Value, Running, Default, DefaultHint, Range, Help string
	Restart                                                 bool
}

type adminData struct {
	View, Tab      string
	Tabs           []adminTab
	Roles          []roleSheet
	Databases      []store.Database
	Stats          map[string]store.DatabaseStats
	Total          store.DatabaseStats
	Disk           *diskUsage // nil when the volume cannot be read
	MaxConnections string
	Allowlist      string
	ClientIP       string
	NotifyTo       string
	MailReady      bool
	TestSent       bool
	OptIns         int
	Params         []paramView
	ParamsApplying bool
	ParamsError    string
	Connections    []store.Connection
	Snapshots      []store.Snapshot
	Filter         string
	Filters        []filterOption
	Audit          []store.AuditEntry
	Search         string
	Page           int
	MorePages      bool
}

type diskUsage struct {
	Used, Size int64
}

func (d diskUsage) Percent() int {
	if d.Size == 0 {
		return 0
	}
	return int(d.Used * 100 / d.Size)
}

func diskUsageOf(path string) (*diskUsage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return nil, err
	}
	size := int64(st.Blocks) * st.Bsize
	return &diskUsage{Used: size - int64(st.Bavail)*st.Bsize, Size: size}, nil
}

func (s *Server) admin(w http.ResponseWriter, r *http.Request) {
	view := r.PathValue("tab")
	if view == "" {
		view = "overview"
	}
	tab, ok := adminViews[view]
	if !ok {
		s.userError(w, r, store.ErrNotFound)
		return
	}
	d := adminData{View: view, Tab: tab, Tabs: adminTabs, Filter: r.URL.Query().Get("f")}
	if err := s.loadAdmin(r, &d); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "admin", d)
}

func (s *Server) loadAdmin(r *http.Request, d *adminData) error {
	ctx := r.Context()
	var err error
	switch d.View {
	case "overview":
		if d.Stats, err = s.cat.DatabaseStats(ctx, ""); err != nil {
			return err
		}
		for _, st := range d.Stats {
			d.Total.SizeBytes += st.SizeBytes
			d.Total.Connections += st.Connections
			d.Total.Snapshots += st.Snapshots
			d.Total.SnapshotBytes += st.SnapshotBytes
		}
		if d.Disk, err = diskUsageOf(s.cfg.SnapshotDir); err != nil {
			s.log.Warn("reading disk usage", "err", err)
		}
		set, err := s.cat.ServerSettings(ctx, []string{"max_connections"})
		if err != nil {
			return err
		}
		d.MaxConnections = set["max_connections"]
	case "connections":
		if d.Connections, err = s.cat.Connections(ctx); err != nil {
			return err
		}
		seen := map[string]bool{}
		d.Filters = []filterOption{{"", "All databases", false}}
		var shown []store.Connection
		for _, c := range d.Connections {
			if !seen[c.Database] {
				seen[c.Database] = true
				d.Filters = append(d.Filters, filterOption{c.Database, c.Database, true})
			}
			if d.Filter == "" || d.Filter == c.Database {
				shown = append(shown, c)
			}
		}
		d.Total.Connections = len(d.Connections)
		d.Connections = shown
		set, err := s.cat.ServerSettings(ctx, []string{"max_connections"})
		if err != nil {
			return err
		}
		d.MaxConnections = set["max_connections"]
	case "snapshots":
		all, err := s.store.ListAllSnapshots(ctx)
		if err != nil {
			return err
		}
		d.Filters = []filterOption{{"", "All", false}, {store.KindManual, "Manual", false}, {store.KindScheduled, "Scheduled", false}, {store.KindPreRestore, "Before restore", false}}
		seen := map[string]bool{}
		for _, sn := range all {
			d.Total.Snapshots++
			d.Total.SnapshotBytes += sn.SizeBytes
			if !seen[sn.Database] {
				seen[sn.Database] = true
				d.Filters = append(d.Filters, filterOption{"db:" + sn.Database, sn.Database, true})
			}
			if d.Filter == "" || d.Filter == sn.Kind || d.Filter == "db:"+sn.Database {
				d.Snapshots = append(d.Snapshots, sn)
			}
		}
	case "roles":
		roles, err := s.cat.ListRoles(ctx)
		if err != nil {
			return err
		}
		for _, role := range roles {
			sheet, err := s.roleSheet(r, role, true)
			if err != nil {
				return err
			}
			d.Roles = append(d.Roles, sheet)
		}
	case "databases":
		if d.Databases, err = s.cat.ListDatabases(ctx); err != nil {
			return err
		}
		if d.Stats, err = s.cat.DatabaseStats(ctx, ""); err != nil {
			return err
		}
		roles, err := s.cat.ListRoles(ctx)
		if err != nil {
			return err
		}
		for _, role := range roles {
			d.Roles = append(d.Roles, roleSheet{Role: role})
		}
	case "network":
		list, errs := allowlist.ReadFile(s.cfg.AllowlistFile)
		for _, e := range errs {
			s.log.Warn("allowlist", "err", e)
		}
		var b strings.Builder
		for _, p := range list {
			b.WriteString(p.String() + "\n")
		}
		d.Allowlist = b.String()
		ip, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ",")
		if a, err := netip.ParseAddr(strings.TrimSpace(ip)); err == nil {
			d.ClientIP = a.Unmap().String()
		}
	case "notifications":
		if d.NotifyTo, err = s.store.Setting(ctx, worker.SettingNotifyTo); err != nil {
			return err
		}
		optIns, err := s.store.NotifyOptIns(ctx)
		if err != nil {
			return err
		}
		d.OptIns = len(optIns)
		d.MailReady, d.TestSent = s.cfg.Mailer != nil, r.URL.Query().Get("mail") == "sent"
	case "server":
		return s.loadParams(ctx, d)
	case "audit":
		cats, err := s.store.AuditCategories(ctx)
		if err != nil {
			return err
		}
		d.Filters = []filterOption{{"", "All", false}}
		for _, c := range cats {
			d.Filters = append(d.Filters, filterOption{c, c, false})
		}
		d.Search = strings.TrimSpace(r.URL.Query().Get("q"))
		d.Page, _ = strconv.Atoi(r.URL.Query().Get("p"))
		d.Page = max(d.Page, 1)
		if d.Audit, err = s.store.ListAudit(ctx, store.AuditQuery{Category: d.Filter, Search: d.Search,
			Limit: auditPageSize + 1, Offset: (d.Page - 1) * auditPageSize}); err != nil {
			return err
		}
		if len(d.Audit) > auditPageSize {
			d.Audit, d.MorePages = d.Audit[:auditPageSize], true
		}
	}
	return nil
}

func (s *Server) loadParams(ctx context.Context, d *adminData) error {
	raw, err := s.store.Setting(ctx, worker.SettingTuning)
	if err != nil {
		return err
	}
	stored, err := tuning.Decode(raw)
	if err != nil {
		return err
	}
	running, err := s.cat.ServerSettings(ctx, tuning.Names())
	if err != nil {
		return err
	}
	requested, err := s.store.Setting(ctx, worker.SettingTuningRequested)
	if err != nil {
		return err
	}
	if d.ParamsError, err = s.store.Setting(ctx, worker.SettingTuningError); err != nil {
		return err
	}
	d.ParamsApplying = requested != ""
	mem := tuning.MemoryMB()
	for _, p := range tuning.Params {
		d.Params = append(d.Params, paramView{Name: p.Name, Value: stored[p.Name], Running: p.FormatServer(running[p.Name]), Default: p.Default(mem), DefaultHint: p.DefaultHint(mem),
			Range: p.Range(mem), Help: p.Help, Restart: p.Restart})
	}
	return nil
}

// saveServerParams validates the parameters and asks the worker to apply them.
func (s *Server) saveServerParams(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	values := map[string]string{}
	for _, p := range tuning.Params {
		values[p.Name] = r.PostForm.Get(p.Name)
	}
	clean, errs := tuning.Validate(values, tuning.MemoryMB())
	if len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		s.userError(w, r, badInput("Nothing saved. "+strings.Join(msgs, ". ")+"."))
		return
	}
	encoded := tuning.Encode(clean)
	if err := s.store.SetSetting(ctx, worker.SettingTuning, encoded); err != nil {
		s.internalError(w, r, err)
		return
	}
	if err := s.store.SetSetting(ctx, worker.SettingTuningError, ""); err != nil {
		s.internalError(w, r, err)
		return
	}
	if err := s.store.SetSetting(ctx, worker.SettingTuningRequested, "1"); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "server.params", "server variables", encoded)
	http.Redirect(w, r, "/admin/server", http.StatusSeeOther)
}

// saveNotifications stores the admin addresses for failure mails, or sends a
// test mail to the addresses in the form.
func (s *Server) saveNotifications(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, err := notify.ParseList(r.PostForm.Get("notify_to"))
	if err != nil {
		s.userError(w, r, badInput(err.Error()))
		return
	}
	value := strings.Join(list, ", ")
	if r.PostForm.Get("action") == "test" {
		if len(list) == 0 {
			s.userError(w, r, badInput("Enter an address to send the test mail to."))
			return
		}
		if _, err := s.cfg.Mailer.Send(ctx, list, "MariaDB: test mail", "This is a test mail from the MariaDB panel at "+s.cfg.Origin+".\n"); err != nil {
			s.log.Error("sending test mail", "err", err)
			s.userError(w, r, badInput("The test mail could not be sent: "+err.Error()))
			return
		}
		s.audit(r, "notify.test", "mail", value)
		http.Redirect(w, r, "/admin/notifications?mail=sent", http.StatusSeeOther)
		return
	}
	if err := s.store.SetSetting(ctx, worker.SettingNotifyTo, value); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "notify.update", "mail", value)
	http.Redirect(w, r, "/admin/notifications", http.StatusSeeOther)
}

func (s *Server) createDatabase(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostForm.Get("name"))
	owner := r.PostForm.Get("owner")
	if err := s.cat.CreateDatabase(r.Context(), name, owner, 0); err != nil {
		s.userError(w, r, err)
		return
	}
	s.audit(r, "database.create", name, "owner="+owner)
	http.Redirect(w, r, "/admin/databases", http.StatusSeeOther)
}

// saveAllowlist validates every entry and writes the allowlist. The proxy
// reads it again for the next connection.
func (s *Server) saveAllowlist(w http.ResponseWriter, r *http.Request) {
	var list []netip.Prefix
	var problems []string
	seen := map[netip.Prefix]bool{}
	for line := range strings.Lines(r.PostForm.Get("allowlist")) {
		line, _, _ = strings.Cut(line, "#")
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		p, err := allowlist.ParseEntry(line)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if !seen[p] {
			seen[p] = true
			list = append(list, p)
		}
	}
	if len(problems) > 0 {
		s.userError(w, r, badInput("Nothing saved. "+strings.Join(problems, "; ")))
		return
	}
	s.allowMu.Lock()
	err := allowlist.WriteFileAtomic(s.cfg.AllowlistFile, allowlist.FormatAllowlist(list), 0o640)
	s.allowMu.Unlock()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	entries := make([]string, len(list))
	for i, p := range list {
		entries[i] = p.String()
	}
	s.audit(r, "allowlist.update", "allowlist", strings.Join(entries, " "))
	http.Redirect(w, r, "/admin/network", http.StatusSeeOther)
}
