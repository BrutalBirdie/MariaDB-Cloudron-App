package names

import (
	"errors"
	"regexp"
	"slices"
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

// Reserved lists the reserved names for the form hints.
func Reserved() []string {
	list := make([]string, 0, len(reserved))
	for n := range reserved {
		list = append(list, n)
	}
	slices.Sort(list)
	return list
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
