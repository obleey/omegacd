// Package secretkey tells whether an environment variable is a secret written out in plain text.
package secretkey

import (
	"regexp"
	"strings"
)

var (
	secretName = regexp.MustCompile(`(?i)PASSWORD|PASSWD|SECRET|TOKEN|API_?KEY|PRIVATE_?KEY|ACCESS_?KEY|ENCRYPTION_?KEY`)
	pathName   = regexp.MustCompile(`(?i)_(FILE|PATH)$`)
	flagValue  = regexp.MustCompile(`(?i)^(true|false|yes|no|on|off|0|1)?$`)
)

// Plain reports whether key=value looks like a secret written out literally: a secret-like name (not a
// *_FILE or *_PATH path), a real value (not empty or a flag), and not ${VAR} filled in from elsewhere.
func Plain(key, value string) bool {
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	return secretName.MatchString(key) && !pathName.MatchString(key) && !flagValue.MatchString(value) && !strings.Contains(value, "$")
}
