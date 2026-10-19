package names

import (
	"errors"
	"regexp"
)

var pattern = regexp.MustCompile(`^[a-z][a-z0-9_]{2,62}$`)

var reserved = map[string]bool{
	"mysql":              true,
	"mariadb":            true,
	"information_schema": true,
	"performance_schema": true,
	"sys":                true,
	"root":               true,
	"cloudron":           true,
	"mdbpanel":           true,
	"tenants":            true,
	"public":             true,
	"test":               true,
}

var ErrInvalid = errors.New("use 3 to 63 characters a-z, 0-9 and _, starting with a letter")
var ErrReserved = errors.New("this name is reserved")

// Validate is applied to every role and database name before it reaches SQL.
// Internal names start with an underscore, so they can never pass.
func Validate(name string) error {
	if !pattern.MatchString(name) {
		return ErrInvalid
	}
	if reserved[name] {
		return ErrReserved
	}
	return nil
}
