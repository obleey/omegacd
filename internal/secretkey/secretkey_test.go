package secretkey

import "testing"

func TestPlain(t *testing.T) {
	for kv, want := range map[[2]string]bool{
		{"MARIADB_ROOT_PASSWORD", "hunter2"}:    true,
		{"API_TOKEN", "abc123"}:                 true,
		{"AWS_SECRET_ACCESS_KEY", "wJalr"}:      true,
		{"apikey", "x1"}:                        true,
		{"DB_PASSWORD", "${DB_PASSWORD}"}:       false, // filled in from a (SOPS) .env
		{"DB_PASSWORD_FILE", "/run/secrets/db"}: false, // a path
		{"SSH_KEY_PATH", "/keys/id"}:            false,
		{"REQUIRE_PASSWORD", "true"}:            false, // a flag
		{"SESSION_SECRET", ""}:                  false,
		{"DB_NAME", "wordpress"}:                false, // not a secret name
		{"dataStoreEncryptionKey", "c2VjcmV0"}:  true,
	} {
		if got := Plain(kv[0], kv[1]); got != want {
			t.Errorf("%s=%s: %v, want %v", kv[0], kv[1], got, want)
		}
	}
}
