package names

import "testing"

func TestValidate(t *testing.T) {
	ok := []string{"abc", "my_app", "a12", "xyz_123"}
	for _, n := range ok {
		if err := Validate(n); err != nil {
			t.Errorf("%q: unexpected error %v", n, err)
		}
	}
	bad := []string{"", "ab", "1abc", "Abc", "a-b", "a b", "a`b", "a;b", "ab'c", "a.b", "_mdb_staging_1", "mysql", "mdbpanel", "tenants", "information_schema",
		"performance_schema", "sys", "root", "cloudron", "a234567890123456789012345678901234567890123456789012345678901234"}
	for _, n := range bad {
		if err := Validate(n); err == nil {
			t.Errorf("%q: expected error", n)
		}
	}
}
