// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"fmt"
	"regexp"
	"strings"
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Expand replaces ${VAR} and ${VAR:-default}; $$ becomes $. Any other $ is kept,
// so $1, $2 stay for glob matches.
func Expand(s string, lookup LookupFunc) (string, error) {
	if !strings.Contains(s, "$") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' || i+1 >= len(s) {
			b.WriteByte(s[i])
			i++
			continue
		}
		switch s[i+1] {
		case '$':
			b.WriteByte('$')
			i += 2
		case '{':
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return "", fmt.Errorf("unterminated ${ in %q", s)
			}
			inner := s[i+2 : i+2+end]
			name, def, hasDef := strings.Cut(inner, ":-")
			if !envName.MatchString(name) {
				return "", fmt.Errorf("invalid variable name %q", name)
			}
			v, ok := lookup(name)
			if hasDef && (!ok || v == "") {
				v = def
			} else if !ok {
				return "", fmt.Errorf("environment variable %s is not set", name)
			}
			b.WriteString(v)
			i += 3 + end
		default:
			b.WriteByte('$')
			i++
		}
	}
	return b.String(), nil
}
