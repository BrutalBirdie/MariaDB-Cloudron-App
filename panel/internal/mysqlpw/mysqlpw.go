package mysqlpw

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"math/big"
	"regexp"
	"strings"
)

// NativeHash returns the mysql_native_password hash, so the plaintext never
// has to be sent to the server (IDENTIFIED BY PASSWORD).
// https://mariadb.com/docs/server/reference/plugins/authentication-plugins/authentication-plugin-mysql_native_password
func NativeHash(password string) string {
	once := sha1.Sum([]byte(password))
	twice := sha1.Sum(once[:])
	return "*" + strings.ToUpper(hex.EncodeToString(twice[:]))
}

var hashPattern = regexp.MustCompile(`^\*[0-9A-F]{40}$`)

// ValidHash reports whether s has the form of a native password hash, which
// needs no quoting in SQL.
func ValidHash(s string) bool { return hashPattern.MatchString(s) }

const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// Password returns 32 characters from a 62 symbol alphabet (about 190 bits).
func Password() (string, error) {
	b := make([]byte, 32)
	max := big.NewInt(int64(len(alphabet)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = alphabet[n.Int64()]
	}
	return string(b), nil
}
