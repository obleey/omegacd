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

// Q58 against a real engine, rootful or rootless: a container running as a non-root user can't read a decrypted 0600
// file, and can once the files belong to OMEGACD_AGENT_SECRET_GROUP and the service joins it (group_add: the gid
// rootful, "0" rootless, where the Agent's user and group are root inside containers).
func TestAgentSecretModes(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS: Docker Desktop and OrbStack share host folders ignoring owner and mode; runs on Linux (CI)")
	}
	if os.Getenv("OMEGACD_IT_ROOT") != "" {
		t.Skip("the matrix's containerised engines run as another user than this test, which a real rootless Agent never " +
			"is (it is its engine's own user); rootless Q58 runs on the machine's own rootless Podman (host-podman)")
	}
	engine, err := DetectEngine(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	key, _ := HostKey(t.TempDir())
	dir := appWithSecrets(t, key)
	read := func(groupAdd ...string) (string, error) {
		args := []string{"run", "--rm", "--user", "4242:4242", "-v", filepath.Join(dir, "tls") + ":/secrets:ro"}
		for _, g := range groupAdd {
			args = append(args, "--group-add", g)
		}
		out, err := exec.Command("docker", append(args, "docker.io/library/busybox:1.36", "cat", "/secrets/server.pem")...).CombinedOutput()
		return string(out), err
	}

	if _, err := NewSecrets(-1, SOPS{HostKey: key}).Decrypt(dir); err != nil {
		t.Fatal(err)
	}
	if out, err := read(); err == nil || strings.Contains(out, plainTLS) {
		t.Fatalf("%s: a non-root container read a 0600 secret: %s", engine.Name, out)
	}

	gid := os.Getgid() // a group the Agent's user is in
	groupAdd := fmt.Sprint(gid)
	if engine.Rootless {
		groupAdd = "0"
	}
	dir = appWithSecrets(t, key)
	if _, err := NewSecrets(gid, SOPS{HostKey: key}).Decrypt(dir); err != nil {
		t.Fatal(err)
	}
	if out, err := read(groupAdd); err != nil || !strings.Contains(out, plainTLS) {
		t.Fatalf("%s (rootless %v): with OMEGACD_AGENT_SECRET_GROUP and group_add %s the container can't read it: %v %s",
			engine.Name, engine.Rootless, groupAdd, err, out)
	}
	if out, err := read(); err == nil || strings.Contains(out, plainTLS) {
		t.Fatalf("%s: a container outside the group read it: %s", engine.Name, out)
	}
	t.Logf("checked on %s %s (rootless %v), group_add %s", engine.Name, engine.Version, engine.Rootless, groupAdd)
}
