//go:build integration

package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Q58 against a real engine: a container running as a non-root user can't read a decrypted 0600 file, and can
// once the files belong to OMEGACD_AGENT_SECRET_GROUP and the service joins it (group_add:).
func TestSecretModesInContainers(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS: Docker Desktop and OrbStack share host folders ignoring owner and mode; runs on Linux (CI)")
	}
	key, _ := HostKey(t.TempDir())
	dir := appWithSecrets(t, key)
	read := func(groupAdd ...string) (string, error) {
		args := []string{"run", "--rm", "--user", "4242:4242", "-v", filepath.Join(dir, "tls") + ":/secrets:ro"}
		for _, g := range groupAdd {
			args = append(args, "--group-add", g)
		}
		out, err := exec.Command("docker", append(args, "alpine:3", "cat", "/secrets/server.pem")...).CombinedOutput()
		return string(out), err
	}

	if _, err := NewSecrets(-1, SOPS{HostKey: key}).Decrypt(dir); err != nil {
		t.Fatal(err)
	}
	if out, err := read(); err == nil || strings.Contains(out, plainTLS) {
		t.Fatalf("a non-root container read a 0600 secret: %s", out)
	}

	gid := os.Getgid() // a group the Agent's user is in
	dir = appWithSecrets(t, key)
	if _, err := NewSecrets(gid, SOPS{HostKey: key}).Decrypt(dir); err != nil {
		t.Fatal(err)
	}
	if out, err := read(fmt.Sprint(gid)); err != nil || !strings.Contains(out, plainTLS) {
		t.Fatalf("with OMEGACD_AGENT_SECRET_GROUP and group_add the container can't read it: %v %s", err, out)
	}
	if out, err := read(); err == nil || strings.Contains(out, plainTLS) {
		t.Fatalf("a container outside the group read it: %s", out)
	}
}
