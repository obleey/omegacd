//go:build integration

package agent

import (
	"cmp"
	"crypto/ed25519"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/obleey/omegacd/internal/agents"
	"github.com/obleey/omegacd/internal/crypto"
	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/importer"
	"github.com/obleey/omegacd/internal/secrets"
	"github.com/obleey/omegacd/internal/store"
)

// M6-05 (Q326): an import through a real Agent. The hub relays the request; the Agent reads a stack running on its
// engine, with an env file beside its folder (Q323), and answers with its files, the secret encrypted for the keys
// sent (Q230); a Host-only set refused is refused for a stack with secrets (Q320).
func TestImportThroughHub(t *testing.T) {
	ag, _, recipient := testAgent(t)
	hostKey := ag.secrets.sources[0].(SOPS).HostKey
	ctx := t.Context()
	id, _ := age.GenerateX25519Identity()
	st, err := store.Open(filepath.Join(t.TempDir(), "omegacd.db"), crypto.NewSealer(id))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hub := agents.New(st, nil)
	srv := httptest.NewServer(hub)
	defer srv.Close()
	invite, _ := st.CreateInvite(ctx, "edge", time.Now().Add(time.Hour))
	key, _ := SigningKey(t.TempDir())
	go ag.Connect(ctx, Connection{URL: srv.URL, Invite: invite, Key: key, AgeRecipient: recipient, HostKey: hostKey})
	eventually(t, "the Agent enrolled", func() bool { h, _ := st.GetHost(ctx, "edge"); return h.Fingerprint != "" })
	hub.Approve(ctx, "edge", domain.Fingerprint(key.Public().(ed25519.PublicKey)))

	// a stack started by hand, readable by a rootless engine's user
	base, err := os.MkdirTemp(cmp.Or(os.Getenv("OMEGACD_IT_ROOT"), t.TempDir()), "import-it-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	os.Chmod(base, 0o755)
	base, _ = filepath.EvalSymlinks(base) // compose records the resolved path (macOS: /private/var/…)
	dir := filepath.Join(base, "stack")
	for f, c := range map[string]string{
		"stack/compose.yaml":  "services:\n  web:\n    image: busybox:1.36\n    command: [sleep, \"3600\"]\n    env_file: ../shared/app.env\n    volumes: [./data:/data]\n",
		"shared/app.env":      "API_TOKEN=t0ken-value\n",
		"stack/data/keep.bin": "data",
	} {
		os.MkdirAll(filepath.Dir(filepath.Join(base, f)), 0o755)
		os.WriteFile(filepath.Join(base, f), []byte(c), 0o644)
	}
	name := appName(t)
	if out, err := exec.Command("docker", "compose", "-p", name, "--project-directory", dir, "-f", filepath.Join(dir, "compose.yaml"), "up", "-d", "--quiet-pull").CombinedOutput(); err != nil {
		t.Fatalf("compose up: %v: %s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "compose", "-p", name, "down", "-v").Run() })

	person, _ := age.GenerateX25519Identity()
	req := importer.Request{AppsDir: "apps", HostName: "edge", Only: []string{name}, Recipients: []string{recipient, person.Recipient().String()}}
	var results []importer.Result
	eventually(t, "the approved Agent's import", func() bool {
		results, err = hub.Import(ctx, "edge", req)
		return err == nil
	})
	if len(results) != 1 || results[0].Error != "" {
		t.Fatalf("results %+v", results)
	}
	r := results[0]
	if c := string(r.Files["compose.yaml"]); !strings.Contains(c, "env_file: ./shared/app.env") || !strings.Contains(c, dir+"/data:/data") {
		t.Errorf("compose.yaml:\n%s", c)
	}
	enc := r.Files["shared/app.sops.env"]
	if len(enc) == 0 || strings.Contains(string(enc), "t0ken-value") {
		t.Fatalf("shared/app.sops.env: %q (files %v)", enc, r.Files)
	}
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "app.sops.env"), enc, 0o600)
	if _, err := (secrets.Decryptor{}).DecryptDir(tmp, []string{person.String()}); err != nil {
		t.Fatalf("the person's key opens it? %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(tmp, "app.env")); string(b) != "API_TOKEN=t0ken-value\n" {
		t.Errorf("decrypts to %q", b)
	}

	req.Refuse = "no person's key covers Host edge"
	if results, err := hub.Import(ctx, "edge", req); err != nil || len(results) != 1 || results[0].Error != req.Refuse {
		t.Fatalf("Host-only refused: %+v %v", results, err)
	}
}
