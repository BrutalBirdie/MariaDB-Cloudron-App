package tuning

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	const mem = 2048
	cases := []struct {
		name, in, want string
		ok             bool
	}{
		{"max_connections", "150", "150", true},
		{"max_connections", "19", "", false},
		{"max_connections", "1e3", "", false},
		{"tmp_table_size", "32MB", "32MB", true},
		{"tmp_table_size", "32 mb", "32MB", true},
		{"tmp_table_size", "512kB", "", false},
		{"tmp_table_size", "512MB", "512MB", true},
		{"tmp_table_size", "1GB", "", false},
		{"innodb_buffer_pool_size", "1536MB", "1536MB", true},
		{"innodb_buffer_pool_size", "1537MB", "", false},
		{"max_statement_time", "0", "0", true},
		{"max_statement_time", "30s", "30s", true},
		{"max_statement_time", "25h", "", false},
		{"max_statement_time", "30S", "", false},
		{"max_statement_time", "500ms", "", false},
		{"wait_timeout", "0", "", false},
		{"wait_timeout", "10min", "10min", true},
		{"tmp_table_size", "8MB\nskip-grant-tables", "", false},
		{"tmp_table_size", "", "", false},
	}
	for _, c := range cases {
		p, _ := Lookup(c.name)
		got, err := p.Normalize(c.in, mem)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("%s %q: got %q, %v", c.name, c.in, got, err)
		}
	}
}

func TestValidateRejectsUnknown(t *testing.T) {
	out, errs := Validate(map[string]string{"init_file": "/tmp/x", "tmp_table_size": "32MB", "max_statement_time": " "}, 2048)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "init_file") {
		t.Fatalf("errs = %v", errs)
	}
	if len(out) != 1 || out["tmp_table_size"] != "32MB" {
		t.Fatalf("out = %v", out)
	}
}

func TestRender(t *testing.T) {
	got, err := Render(map[string]string{"tmp_table_size": "32MB", "max_connections": "150", "max_statement_time": "2min"}, 2048)
	if err != nil {
		t.Fatal(err)
	}
	want := "[mariadbd]\nmax_connections = 150\ntmp_table_size = 33554432\nmax_heap_table_size = 33554432\nmax_statement_time = 120\n"
	if !strings.Contains(got, want) {
		t.Fatalf("render = %q", got)
	}
}

func TestAssignmentsResetToDefaults(t *testing.T) {
	got, err := Assignments(map[string]string{"max_connections": "150"}, 2048)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"max_connections": "150", "innodb_buffer_pool_size": "536870912", "tmp_table_size": "16777216", "max_heap_table_size": "16777216",
		"sort_buffer_size": "2097152", "max_statement_time": "0", "idle_transaction_timeout": "0", "wait_timeout": "28800"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for _, a := range got {
		if want[a.Name] != a.Value {
			t.Errorf("%s = %s, want %s", a.Name, a.Value, want[a.Name])
		}
	}
}

func TestFormatServer(t *testing.T) {
	p, _ := Lookup("innodb_buffer_pool_size")
	if got := p.FormatServer("536870912"); got != "512MB" {
		t.Fatalf("got %s", got)
	}
	p, _ = Lookup("max_statement_time")
	if got := p.FormatServer("120.000000"); got != "2min" {
		t.Fatalf("got %s", got)
	}
}
