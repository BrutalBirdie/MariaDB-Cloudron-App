package mysqlpw

import (
	"strings"
	"testing"
)

// expected value from SELECT PASSWORD('password') on MariaDB 11.8
func TestNativeHashKnownValue(t *testing.T) {
	if got := NativeHash("password"); got != "*2470C0C06DEE42FD1618BB99005ADCA2EC9D1E19" {
		t.Fatalf("got %s", got)
	}
	if !ValidHash(NativeHash("x")) {
		t.Fatal("hash does not validate")
	}
	for _, bad := range []string{"", "*2470c0c06dee42fd1618bb99005adca2ec9d1e19", "*2470C0C06DEE42FD1618BB99005ADCA2EC9D1E1", "*2470C0C06DEE42FD1618BB99005ADCA2EC9D1E19'"} {
		if ValidHash(bad) {
			t.Errorf("%q validated", bad)
		}
	}
}

func TestPassword(t *testing.T) {
	p, err := Password()
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 32 || strings.Trim(p, alphabet) != "" {
		t.Fatalf("bad password %q", p)
	}
}
