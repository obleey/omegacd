package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/obleey/omegacd/internal/crypto"
	"github.com/obleey/omegacd/internal/domain"
)

var ctx = context.Background()

func open(t *testing.T) (*Store, string) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "omegacd.db")
	s, err := Open(path, crypto.NewSealer(id))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("got %v, want %v", err, target)
	}
}

func TestReopenKeepsSchema(t *testing.T) {
	s, path := open(t)
	must(t, s.CreateApp(ctx, domain.Application{Name: "a"}))
	s.Close()
	s2, err := Open(path, s.sealer)
	must(t, err)
	defer s2.Close()
	_, err = s2.GetApp(ctx, "a")
	must(t, err)
}

func TestApps(t *testing.T) {
	s, _ := open(t)
	a := domain.Application{
		Name: "web", RepoURL: "https://example.com/r.git", Path: "apps/web", TargetRevision: "main",
		ComposeFiles: []string{"compose.yaml", "override.yaml"}, Profiles: []string{"prod"},
		Host: domain.LocalHost, AutoSync: true, Prune: true, PruneVolumes: true, RolledBack: true,
		Kind: domain.KindCompose, Parent: "root", NotInGit: true, WorkDir: "/srv/apps",
		CreatedAt: time.Unix(1700000000, 0).UTC(), Dirs: []string{},
	}
	must(t, s.CreateApp(ctx, a))
	wantErr(t, s.CreateApp(ctx, a), domain.ErrAlreadyExists)

	got, err := s.GetApp(ctx, "web")
	must(t, err)
	if !reflect.DeepEqual(got, a) {
		t.Fatalf("got %+v\nwant %+v", got, a)
	}

	a.SelfHeal, a.RolledBack, a.Profiles, a.Parent, a.NotInGit = true, false, nil, "", false
	must(t, s.UpdateApp(ctx, a))
	got, _ = s.GetApp(ctx, "web")
	if !got.SelfHeal || got.RolledBack || got.Profiles != nil || got.Parent != "" || got.NotInGit || !got.CreatedAt.Equal(a.CreatedAt) {
		t.Fatalf("update not applied: %+v", got)
	}
	wantErr(t, s.UpdateApp(ctx, domain.Application{Name: "nope"}), domain.ErrNotFound)

	dirs := []string{"local:/work/web", "prod:/srv/omegacd/root/web"}
	must(t, s.SetAppDirs(ctx, "web", dirs))
	must(t, s.UpdateApp(ctx, a)) // keeps them
	if got, _ = s.GetApp(ctx, "web"); !reflect.DeepEqual(got.Dirs, dirs) {
		t.Fatalf("dirs = %v", got.Dirs)
	}
	wantErr(t, s.SetAppDirs(ctx, "nope", dirs), domain.ErrNotFound)

	must(t, s.CreateApp(ctx, domain.Application{Name: "api"}))
	apps, err := s.ListApps(ctx)
	must(t, err)
	if len(apps) != 2 || apps[0].Name != "api" || apps[1].Name != "web" {
		t.Fatalf("list: %+v", apps)
	}
	if apps[0].CreatedAt.IsZero() {
		t.Fatal("CreatedAt should default to now")
	}

	must(t, s.DeleteApp(ctx, "web"))
	wantErr(t, s.DeleteApp(ctx, "web"), domain.ErrNotFound)
	_, err = s.GetApp(ctx, "web")
	wantErr(t, err, domain.ErrNotFound)
}

func TestOps(t *testing.T) {
	s, _ := open(t)
	must(t, s.CreateApp(ctx, domain.Application{Name: "web"}))
	_, err := s.CreateOp(ctx, domain.SyncOperation{App: "missing"})
	wantErr(t, err, domain.ErrNotFound)

	_, err = s.LastSucceededOp(ctx, "web")
	wantErr(t, err, domain.ErrNotFound)

	var ids []int64
	for i := range 25 {
		id, err := s.CreateOp(ctx, domain.SyncOperation{App: "web", Revision: "sha", TreeHash: "tree", Trigger: "manual"})
		must(t, err)
		ids = append(ids, id)
		phase := domain.OpSucceeded
		if i == 24 {
			phase = domain.OpFailed
		}
		must(t, s.FinishOp(ctx, id, phase, "msg"))
	}
	wantErr(t, s.FinishOp(ctx, 99999, domain.OpFailed, ""), domain.ErrNotFound)

	ops, err := s.ListOps(ctx, "web")
	must(t, err)
	if len(ops) != 20 || ops[0].ID != ids[24] || ops[19].ID != ids[5] {
		t.Fatalf("want newest 20, got %d (first %d)", len(ops), ops[0].ID)
	}
	if ops[0].Phase != domain.OpFailed || ops[0].Message != "msg" || ops[0].FinishedAt.IsZero() || ops[0].StartedAt.IsZero() {
		t.Fatalf("op fields: %+v", ops[0])
	}

	last, err := s.LastSucceededOp(ctx, "web")
	must(t, err)
	if last.ID != ids[23] {
		t.Fatalf("last succeeded = %d, want %d", last.ID, ids[23])
	}

	running, err := s.CreateOp(ctx, domain.SyncOperation{App: "web", Revision: "r2"})
	must(t, err)
	op, err := s.GetOp(ctx, running)
	must(t, err)
	if op.Phase != domain.OpRunning || op.Revision != "r2" || !op.FinishedAt.IsZero() {
		t.Fatalf("running op: %+v", op)
	}
	_, err = s.GetOp(ctx, ids[0])
	wantErr(t, err, domain.ErrNotFound)

	must(t, s.DeleteApp(ctx, "web"))
	ops, _ = s.ListOps(ctx, "web")
	if len(ops) != 0 {
		t.Fatalf("ops not deleted with app: %d", len(ops))
	}
}

func TestRepoCredentials(t *testing.T) {
	s, _ := open(t)
	id, err := s.SaveRepoCredential(ctx, domain.RepoCredential{URLPrefix: "https://git.example.com/", Username: "u", Password: secretPW, SSHPrivateKey: "key"})
	must(t, err)
	_, err = s.SaveRepoCredential(ctx, domain.RepoCredential{URLPrefix: "https://git.example.com/"})
	wantErr(t, err, domain.ErrAlreadyExists)

	var raw string
	must(t, s.db.QueryRow(`SELECT password FROM repo_credentials WHERE id = ?`, id).Scan(&raw))
	if raw == "" || strings.Contains(raw, secretPW) {
		t.Fatalf("password not sealed: %q", raw)
	}

	// empty secrets keep old values
	_, err = s.SaveRepoCredential(ctx, domain.RepoCredential{ID: id, URLPrefix: "https://git.example.com/x", Username: "u2"})
	must(t, err)
	creds, err := s.ListRepoCredentials(ctx)
	must(t, err)
	want := domain.RepoCredential{ID: id, URLPrefix: "https://git.example.com/x", Username: "u2", Password: secretPW, SSHPrivateKey: "key", HasPassword: true, HasSSHKey: true}
	if len(creds) != 1 || creds[0] != want {
		t.Fatalf("got %+v", creds)
	}

	_, err = s.SaveRepoCredential(ctx, domain.RepoCredential{ID: id, URLPrefix: "p", Password: "new"})
	must(t, err)
	creds, _ = s.ListRepoCredentials(ctx)
	if creds[0].Password != "new" || creds[0].SSHPrivateKey != "key" {
		t.Fatalf("got %+v", creds[0])
	}

	_, err = s.SaveRepoCredential(ctx, domain.RepoCredential{ID: 999, URLPrefix: "q"})
	wantErr(t, err, domain.ErrNotFound)
	must(t, s.DeleteRepoCredential(ctx, id))
	wantErr(t, s.DeleteRepoCredential(ctx, id), domain.ErrNotFound)
}

func TestRegistryCredentials(t *testing.T) {
	s, _ := open(t)
	id, err := s.SaveRegistryCredential(ctx, domain.RegistryCredential{Registry: "ghcr.io", Username: "u", Password: "pw"})
	must(t, err)
	_, err = s.SaveRegistryCredential(ctx, domain.RegistryCredential{Registry: "ghcr.io"})
	wantErr(t, err, domain.ErrAlreadyExists)

	_, err = s.SaveRegistryCredential(ctx, domain.RegistryCredential{ID: id, Registry: "ghcr.io", Username: "u2"})
	must(t, err)
	creds, err := s.ListRegistryCredentials(ctx)
	must(t, err)
	want := domain.RegistryCredential{ID: id, Registry: "ghcr.io", Username: "u2", Password: "pw", HasPassword: true}
	if len(creds) != 1 || creds[0] != want {
		t.Fatalf("got %+v", creds)
	}

	_, err = s.SaveRegistryCredential(ctx, domain.RegistryCredential{ID: 999, Registry: "x"})
	wantErr(t, err, domain.ErrNotFound)
	must(t, s.DeleteRegistryCredential(ctx, id))
	wantErr(t, s.DeleteRegistryCredential(ctx, id), domain.ErrNotFound)
}

func TestTokens(t *testing.T) {
	s, _ := open(t)
	tok, plain, err := s.CreateToken(ctx, "ci")
	must(t, err)
	if !strings.HasPrefix(plain, "ocd_") || len(plain) != 4+43 || tok.ID == 0 || tok.Hash != hashToken(plain) {
		t.Fatalf("token: %+v %q", tok, plain)
	}
	_, _, err = s.CreateToken(ctx, "ci")
	wantErr(t, err, domain.ErrAlreadyExists)
	_, _, err = s.CreateToken(ctx, "")
	wantErr(t, err, domain.ErrInvalid)

	_, err = s.VerifyToken(ctx, "ocd_wrong")
	wantErr(t, err, domain.ErrNotFound)
	got, err := s.VerifyToken(ctx, plain)
	must(t, err)
	if got.ID != tok.ID || got.LastUsed.IsZero() {
		t.Fatalf("verify: %+v", got)
	}

	list, err := s.ListTokens(ctx)
	must(t, err)
	if len(list) != 1 || list[0].Name != "ci" || list[0].LastUsed.IsZero() || list[0].CreatedAt.IsZero() {
		t.Fatalf("list: %+v", list)
	}

	must(t, s.DeleteToken(ctx, tok.ID))
	wantErr(t, s.DeleteToken(ctx, tok.ID), domain.ErrNotFound)
	_, err = s.VerifyToken(ctx, plain)
	wantErr(t, err, domain.ErrNotFound)
}

func TestAdmin(t *testing.T) {
	s, _ := open(t)
	_, _, err := s.AdminPasswordHash(ctx)
	wantErr(t, err, domain.ErrNotFound)
	must(t, s.SetAdminPassword(ctx, "h1", true))
	must(t, s.SetAdminPassword(ctx, "h2", false))
	h, mc, err := s.AdminPasswordHash(ctx)
	must(t, err)
	if h != "h2" || mc {
		t.Fatalf("got %q %v", h, mc)
	}
}

func TestSessions(t *testing.T) {
	s, path := open(t)
	exp := time.Now().Add(time.Hour).Truncate(time.Microsecond).UTC()
	must(t, s.CreateSession(ctx, "a", exp))
	must(t, s.CreateSession(ctx, "b", exp))
	must(t, s.CreateSession(ctx, "c", exp))
	wantErr(t, s.CreateSession(ctx, "a", exp), domain.ErrAlreadyExists)
	got, err := s.GetSession(ctx, "a")
	must(t, err)
	if !got.Equal(exp) {
		t.Fatalf("expires %v, want %v", got, exp)
	}
	_, err = s.GetSession(ctx, "nope")
	wantErr(t, err, domain.ErrNotFound)

	must(t, s.ExtendSession(ctx, "a", time.Now().Add(-time.Second)))
	_, err = s.GetSession(ctx, "a")
	wantErr(t, err, domain.ErrNotFound)
	wantErr(t, s.ExtendSession(ctx, "nope", exp), domain.ErrNotFound)
	must(t, s.CreateSession(ctx, "d", exp)) // purges expired "a"
	var n int
	must(t, s.db.QueryRow(`SELECT count(*) FROM sessions WHERE hash = 'a'`).Scan(&n))
	if n != 0 {
		t.Fatal("expired session not purged")
	}

	must(t, s.DeleteSession(ctx, "d"))
	_, err = s.GetSession(ctx, "d")
	wantErr(t, err, domain.ErrNotFound)

	must(t, s.DeleteSessionsExcept(ctx, "b"))
	_, err = s.GetSession(ctx, "c")
	wantErr(t, err, domain.ErrNotFound)

	s.Close()
	s2, err := Open(path, s.sealer)
	must(t, err)
	defer s2.Close()
	_, err = s2.GetSession(ctx, "b")
	must(t, err)
}

// long enough that random sealed base64 never contains it by chance
const secretPW = "correct-horse-battery-staple"

func TestHosts(t *testing.T) {
	s, _ := open(t)
	_, err := s.GetHost(ctx, "prod")
	wantErr(t, err, domain.ErrNotFound)

	must(t, s.SaveHost(ctx, domain.Host{Name: "prod", URL: "ssh://deploy@prod", SSHPrivateKey: "key", TLSCert: "cert", TLSKey: "tlskey"}))
	var raw string
	must(t, s.db.QueryRow(`SELECT ssh_private_key FROM hosts WHERE name = 'prod'`).Scan(&raw))
	if raw == "" || strings.Contains(raw, "key") {
		t.Fatalf("key not sealed: %q", raw)
	}

	// empty secrets keep old values; WorkDir defaults
	must(t, s.SaveHost(ctx, domain.Host{Name: "prod", URL: "tcp://prod:2376", TLSCA: "ca"}))
	h, err := s.GetHost(ctx, "prod")
	must(t, err)
	if h.URL != "tcp://prod:2376" || h.WorkDir != "/var/lib/omegacd" || h.SSHPrivateKey != "key" || h.TLSCA != "ca" ||
		h.TLSCert != "cert" || h.TLSKey != "tlskey" || !h.HasSSHKey || !h.HasTLS || h.CreatedAt.IsZero() {
		t.Fatalf("got %+v", h)
	}

	must(t, s.SaveHost(ctx, domain.Host{Name: "a", URL: "tcp://a", WorkDir: "/srv"}))
	hosts, err := s.ListHosts(ctx)
	must(t, err)
	if len(hosts) != 2 || hosts[0].Name != "a" || hosts[0].WorkDir != "/srv" || hosts[0].HasSSHKey || hosts[0].HasTLS {
		t.Fatalf("got %+v", hosts)
	}

	must(t, s.DeleteHost(ctx, "prod"))
	wantErr(t, s.DeleteHost(ctx, "prod"), domain.ErrNotFound)
}

func TestHostKeys(t *testing.T) {
	s, _ := open(t)
	wantErr(t, s.AddHostKey(ctx, domain.HostKey{Host: "prod", Recipient: "age1a", PrivateKey: "AGE-SECRET-KEY-A"}), domain.ErrNotFound)
	must(t, s.SaveHost(ctx, domain.Host{Name: "prod", URL: "ssh://deploy@prod"}))
	must(t, s.SaveHost(ctx, domain.Host{Name: "other", URL: "ssh://deploy@other"}))
	must(t, s.AddHostKey(ctx, domain.HostKey{Host: "prod", Recipient: "age1a", PrivateKey: "AGE-SECRET-KEY-A"}))
	must(t, s.AddHostKey(ctx, domain.HostKey{Host: "other", Recipient: "age1o", PrivateKey: "AGE-SECRET-KEY-O"}))
	must(t, s.AddHostKey(ctx, domain.HostKey{Host: "prod", Recipient: "age1b", PrivateKey: "AGE-SECRET-KEY-B"}))
	wantErr(t, s.AddHostKey(ctx, domain.HostKey{Host: "prod", Recipient: "age1a", PrivateKey: "AGE-SECRET-KEY-A"}), domain.ErrAlreadyExists)

	keys, err := s.ListHostKeys(ctx, "prod")
	must(t, err)
	if len(keys) != 2 || keys[0].Recipient != "age1b" || !keys[0].Current || keys[0].PrivateKey != "AGE-SECRET-KEY-B" ||
		keys[1].Recipient != "age1a" || keys[1].Current || keys[1].PrivateKey != "AGE-SECRET-KEY-A" || keys[0].CreatedAt.IsZero() {
		t.Fatalf("%+v", keys)
	}
	// the failed duplicate insert must not have demoted the current key
	if keys, _ := s.ListHostKeys(ctx, "other"); len(keys) != 1 || !keys[0].Current {
		t.Fatalf("%+v", keys)
	}

	var plain int
	must(t, s.db.QueryRow(`SELECT count(*) FROM host_keys WHERE private_key LIKE '%AGE-SECRET-KEY%'`).Scan(&plain))
	if plain != 0 {
		t.Fatal("private key stored in plaintext")
	}

	must(t, s.DeleteHostKey(ctx, "prod", "age1a"))
	wantErr(t, s.DeleteHostKey(ctx, "prod", "age1a"), domain.ErrNotFound)
	must(t, s.DeleteHost(ctx, "prod"))
	var left int
	must(t, s.db.QueryRow(`SELECT count(*) FROM host_keys WHERE host = 'prod'`).Scan(&left))
	if left != 0 {
		t.Fatalf("%d keys left after deleting the Host", left)
	}
}

func TestEnrollment(t *testing.T) {
	s, _ := open(t)
	now := time.Now()
	invite, err := s.CreateInvite(ctx, "web", now.Add(time.Hour))
	must(t, err)
	var stored string
	must(t, s.db.QueryRow(`SELECT hash FROM invites WHERE host = 'web'`).Scan(&stored))
	if !strings.HasPrefix(invite, "omegacd-invite-") || stored == "" || strings.Contains(invite, stored) || stored == invite {
		t.Fatalf("invite %q stored as %q: want a hash", invite, stored)
	}
	h, err := s.GetHost(ctx, "web")
	must(t, err)
	if h.URL != "" || h.Fingerprint != "" || h.Approved {
		t.Fatalf("an invited Host: %+v", h)
	}

	again, err := s.CreateInvite(ctx, "web", now.Add(time.Hour)) // a new invite replaces the last
	must(t, err)
	_, err = s.Enroll(ctx, invite, []byte("key-1"), "age1web", now)
	wantErr(t, err, domain.ErrInvalid)

	h, err = s.Enroll(ctx, again, []byte("key-1"), "age1web", now)
	must(t, err)
	if h.Name != "web" || h.Fingerprint != domain.Fingerprint([]byte("key-1")) || h.AgeRecipient != "age1web" || h.Approved {
		t.Fatalf("enrolled: %+v", h)
	}
	_, err = s.Enroll(ctx, again, []byte("key-2"), "age1other", now) // used up
	wantErr(t, err, domain.ErrInvalid)
	got, err := s.HostBySigningKey(ctx, []byte("key-1"))
	must(t, err)
	if got.Name != "web" {
		t.Fatalf("by key: %+v", got)
	}
	_, err = s.HostBySigningKey(ctx, []byte("key-2"))
	wantErr(t, err, domain.ErrNotFound)
	_, err = s.CreateInvite(ctx, "web", now.Add(time.Hour)) // re-enrolling means revoking first (M3-03)
	wantErr(t, err, domain.ErrAlreadyExists)

	wantErr(t, s.ApproveHost(ctx, "web", domain.Fingerprint([]byte("key-2"))), domain.ErrInvalid)
	must(t, s.ApproveHost(ctx, "web", h.Fingerprint))
	if h, _ = s.GetHost(ctx, "web"); !h.Approved {
		t.Fatalf("not approved: %+v", h)
	}
	// SaveHost, the push Hosts' path, keeps what enrollment set
	must(t, s.SaveHost(ctx, domain.Host{Name: "web", WorkDir: "/srv"}))
	if h, _ = s.GetHost(ctx, "web"); !h.Approved || h.AgeRecipient != "age1web" {
		t.Fatalf("after SaveHost: %+v", h)
	}

	expired, err := s.CreateInvite(ctx, "db", now.Add(time.Hour))
	must(t, err)
	_, err = s.Enroll(ctx, expired, []byte("key-3"), "age1db", now.Add(time.Hour))
	wantErr(t, err, domain.ErrInvalid)
	_, err = s.Enroll(ctx, expired, []byte("key-3"), "age1db", now) // and used up by the try
	wantErr(t, err, domain.ErrInvalid)
	wantErr(t, s.ApproveHost(ctx, "db", "SHA256:x"), domain.ErrInvalid) // no key to approve

	must(t, s.SaveHost(ctx, domain.Host{Name: "prod", URL: "ssh://deploy@prod"}))
	_, err = s.CreateInvite(ctx, "prod", now.Add(time.Hour))
	wantErr(t, err, domain.ErrInvalid)

	_, err = s.CreateInvite(ctx, "db", now.Add(time.Hour))
	must(t, err)
	must(t, s.DeleteHost(ctx, "db")) // its invite goes with it
	var n int
	must(t, s.db.QueryRow(`SELECT count(*) FROM invites`).Scan(&n))
	if n != 0 {
		t.Fatalf("%d invites left", n)
	}
}

func TestKeyRotationAndLock(t *testing.T) {
	s, _ := open(t)
	invite, err := s.CreateInvite(ctx, "web", time.Now().Add(time.Hour))
	must(t, err)
	_, err = s.Enroll(ctx, invite, []byte("key-1"), "age1web", time.Now())
	must(t, err)

	wantErr(t, s.RotateSigningKey(ctx, "web", []byte("not its key"), []byte("key-2")), domain.ErrInvalid)
	must(t, s.RotateSigningKey(ctx, "web", []byte("key-1"), []byte("key-2")))
	for _, k := range []string{"key-1", "key-2"} { // both, until the Agent authenticates with one
		if h, err := s.HostBySigningKey(ctx, []byte(k)); err != nil || h.Name != "web" {
			t.Fatalf("by %s: %+v %v", k, h, err)
		}
	}
	if h, _ := s.GetHost(ctx, "web"); h.Fingerprint != domain.Fingerprint([]byte("key-2")) {
		t.Fatalf("shows %s, want the new key", h.Fingerprint)
	}
	must(t, s.ConfirmSigningKey(ctx, "web", []byte("key-2"))) // the Agent switched
	_, err = s.HostBySigningKey(ctx, []byte("key-1"))
	wantErr(t, err, domain.ErrNotFound)

	must(t, s.RotateSigningKey(ctx, "web", []byte("key-2"), []byte("key-3")))
	must(t, s.ConfirmSigningKey(ctx, "web", []byte("key-2"))) // it never switched (a crash): the old key is back
	if h, _ := s.GetHost(ctx, "web"); h.Fingerprint != domain.Fingerprint([]byte("key-2")) {
		t.Fatalf("shows %s, want the old key back", h.Fingerprint)
	}
	_, err = s.HostBySigningKey(ctx, []byte("key-3"))
	wantErr(t, err, domain.ErrNotFound)

	since := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	conns := []domain.AgentConnection{{Address: "10.0.0.1", Since: since}, {Address: "10.0.0.2", Since: since.Add(time.Hour)}}
	must(t, s.LockHost(ctx, "web", conns))
	if h, _ := s.GetHost(ctx, "web"); !reflect.DeepEqual(h.Locked, conns) {
		t.Fatalf("locked: %+v", h.Locked)
	}
	must(t, s.UnlockHost(ctx, "web"))
	if h, _ := s.GetHost(ctx, "web"); h.Locked != nil {
		t.Fatalf("still locked: %+v", h.Locked)
	}
	wantErr(t, s.LockHost(ctx, "nope", conns), domain.ErrNotFound)
}

// M3-06: an outbox entry applies once, only to Applications on the Agent's own Host.
func TestApplyAgentReport(t *testing.T) {
	s, _ := open(t)
	for _, h := range []string{"edge", "other"} {
		inv, _ := s.CreateInvite(ctx, h, time.Now().Add(time.Hour))
		_, err := s.Enroll(ctx, inv, []byte("key-"+h), "age1"+h, time.Now())
		must(t, err)
	}
	must(t, s.CreateApp(ctx, domain.Application{Name: "web", Host: "edge", AutoSync: true}))
	must(t, s.CreateApp(ctx, domain.Application{Name: "db", Host: "other"}))
	id, err := s.CreateOp(ctx, domain.SyncOperation{App: "web", Revision: "r1", TreeHash: "t1"})
	must(t, err)
	must(t, s.SetOpPhase(ctx, id, domain.OpRunning, "Host edge disconnected"))

	applied, err := s.ApplyAgentReport(ctx, "edge", 1, domain.AgentReport{App: "web", Op: id, Phase: domain.OpSucceeded, Message: "synced"})
	must(t, err)
	op, _ := s.GetOp(ctx, id)
	if !applied || op.Phase != domain.OpSucceeded || op.Message != "synced" || op.FinishedAt.IsZero() {
		t.Fatalf("applied %v, op %+v", applied, op)
	}
	again, err := s.ApplyAgentReport(ctx, "edge", 1, domain.AgentReport{App: "web", Trigger: "self-heal", Phase: domain.OpSucceeded})
	must(t, err)
	if ops, _ := s.ListOps(ctx, "web"); again || len(ops) != 1 {
		t.Fatalf("an entry sent again applied again: %v, %d ops", again, len(ops))
	}

	waiting, _ := s.CreateOp(ctx, domain.SyncOperation{App: "web", Revision: "r2"})
	must(t, s.SetOpPhase(ctx, waiting, domain.OpWaiting, "waiting for Host edge"))
	at := time.Date(2026, 11, 9, 3, 0, 0, 0, time.UTC)
	_, err = s.ApplyAgentReport(ctx, "edge", 2, domain.AgentReport{App: "web", Revision: "r0", Checksum: "t0", Trigger: "rollback",
		Phase: domain.OpSucceeded, At: at})
	must(t, err)
	ops, _ := s.ListOps(ctx, "web")
	a, _ := s.GetApp(ctx, "web")
	if len(ops) != 3 || ops[0].Trigger != "rollback" || ops[0].TreeHash != "t0" || !ops[0].StartedAt.Equal(at) || !a.RolledBack || a.AutoSync {
		t.Fatalf("a rollback on the Host: ops %+v, app rolled back %v auto-sync %v", ops, a.RolledBack, a.AutoSync)
	}
	if op, _ := s.GetOp(ctx, waiting); op.Phase != domain.OpSuperseded {
		t.Fatalf("a Sync waiting for the Host would undo its rollback on reconnect: %+v", op)
	}

	_, err = s.ApplyAgentReport(ctx, "edge", 3, domain.AgentReport{App: "db", Trigger: "self-heal", Phase: domain.OpFailed})
	must(t, err)
	if ops, _ := s.ListOps(ctx, "db"); len(ops) != 0 {
		t.Fatalf("an Agent reported on another Host's Application: %+v", ops)
	}
	_, err = s.ApplyAgentReport(ctx, "nope", 1, domain.AgentReport{App: "web"})
	wantErr(t, err, domain.ErrNotFound)
}

// M4-08: a delete waiting for its Host is recorded on the app; the Agent's removal from its outbox drops it, once,
// only for an app on that Host that is being deleted.
func TestAgentRemoval(t *testing.T) {
	s, _ := open(t)
	for _, h := range []string{"edge", "other"} {
		inv, _ := s.CreateInvite(ctx, h, time.Now().Add(time.Hour))
		_, err := s.Enroll(ctx, inv, []byte("key-"+h), "age1"+h, time.Now())
		must(t, err)
	}
	must(t, s.CreateApp(ctx, domain.Application{Name: "web", Host: "edge"}))
	must(t, s.CreateApp(ctx, domain.Application{Name: "db", Host: "edge"}))
	must(t, s.CreateApp(ctx, domain.Application{Name: "api", Host: "other"}))
	wantErr(t, s.SetAppDeleting(ctx, "nope", "volumes"), domain.ErrNotFound)
	must(t, s.SetAppDeleting(ctx, "web", "volumes"))
	must(t, s.SetAppDeleting(ctx, "api", "containers"))
	if a, _ := s.GetApp(ctx, "web"); a.Deleting != "volumes" {
		t.Fatalf("deleting %q", a.Deleting)
	}
	must(t, s.UpdateApp(ctx, domain.Application{Name: "web", Host: "edge", AutoSync: true}))
	if a, _ := s.GetApp(ctx, "web"); a.Deleting != "volumes" {
		t.Fatalf("UpdateApp cleared deleting: %q", a.Deleting)
	}

	for i, app := range []string{"db", "api"} { // not being deleted; another Host's (in seq order: a later seq goes first)
		applied, err := s.ApplyAgentRemoval(ctx, "edge", uint64(i+1), app)
		must(t, err)
		if _, gerr := s.GetApp(ctx, app); !applied || gerr != nil {
			t.Fatalf("%s: applied %v, then %v", app, applied, gerr)
		}
	}
	applied, err := s.ApplyAgentRemoval(ctx, "edge", 3, "web")
	must(t, err)
	if _, gerr := s.GetApp(ctx, "web"); !applied || !errors.Is(gerr, domain.ErrNotFound) {
		t.Fatalf("applied %v, app %v", applied, gerr)
	}
	must(t, s.CreateApp(ctx, domain.Application{Name: "web", Host: "edge"}))
	must(t, s.SetAppDeleting(ctx, "web", "keep"))
	if applied, err := s.ApplyAgentRemoval(ctx, "edge", 3, "web"); err != nil || applied {
		t.Fatalf("an entry sent again applied again: %v %v", applied, err)
	}
	if _, err := s.GetApp(ctx, "web"); err != nil {
		t.Fatal("the new app of the same name went with the old entry")
	}
	_, err = s.ApplyAgentRemoval(ctx, "nope", 1, "web")
	wantErr(t, err, domain.ErrNotFound)
}

// M4-02: an Application's latest report from its Host's Agent, only from that Host; when the Agent left.
func TestAgentStatus(t *testing.T) {
	s, _ := open(t)
	for _, h := range []string{"edge", "other"} {
		inv, _ := s.CreateInvite(ctx, h, time.Now().Add(time.Hour))
		_, err := s.Enroll(ctx, inv, []byte("key-"+h), "age1"+h, time.Now())
		must(t, err)
	}
	must(t, s.CreateApp(ctx, domain.Application{Name: "web", Host: "edge"}))
	_, err := s.AgentStatus(ctx, "web")
	wantErr(t, err, domain.ErrNotFound)

	at := time.Date(2026, 11, 9, 15, 0, 0, 0, time.UTC)
	st := domain.AgentStatus{Revision: "r1", Health: domain.Healthy, Drifted: true, Message: "web stopped",
		Live:  domain.LiveState{Services: []string{"web"}, Containers: []domain.Container{{Name: "web-1", State: "running"}}},
		Drift: []domain.ServiceDiff{{Service: "web", Change: "changed"}}, Details: map[string]map[string]string{"volume/data": {"driver": "local"}}, At: at}
	must(t, s.SaveAgentStatus(ctx, "edge", "web", st))
	got, err := s.AgentStatus(ctx, "web")
	must(t, err)
	if !reflect.DeepEqual(got, st) {
		t.Fatalf("got %+v\nwant %+v", got, st)
	}
	st.Health = domain.Degraded
	must(t, s.SaveAgentStatus(ctx, "edge", "web", st)) // the latest wins
	if got, _ := s.AgentStatus(ctx, "web"); got.Health != domain.Degraded {
		t.Fatalf("not updated: %+v", got)
	}
	wantErr(t, s.SaveAgentStatus(ctx, "other", "web", st), domain.ErrNotFound) // another Host's Agent

	wantErr(t, s.AgentLeft(ctx, "edge", at), domain.ErrNotFound) // no Hello recorded yet
	must(t, s.RecordAgent(ctx, "edge", domain.AgentInfo{Version: "v0.2.0", SeenAt: at}))
	must(t, s.AgentLeft(ctx, "edge", at.Add(time.Hour)))
	h, _ := s.GetHost(ctx, "edge")
	if h.Agent == nil || !h.Agent.LeftAt.Equal(at.Add(time.Hour)) || h.Agent.Version != "v0.2.0" {
		t.Fatalf("left at: %+v", h.Agent)
	}
	must(t, s.RecordAgent(ctx, "edge", domain.AgentInfo{Version: "v0.2.0", SeenAt: at.Add(2 * time.Hour)})) // back
	if h, _ := s.GetHost(ctx, "edge"); !h.Agent.LeftAt.IsZero() {
		t.Fatalf("still left: %+v", h.Agent)
	}

	must(t, s.DeleteApp(ctx, "web"))
	_, err = s.AgentStatus(ctx, "web")
	wantErr(t, err, domain.ErrNotFound)
}

// M4-11: the last Diff an Application's Agent computed, gone with the app.
func TestAgentDiff(t *testing.T) {
	s, _ := open(t)
	_, err := s.AgentDiff(ctx, "web")
	wantErr(t, err, domain.ErrNotFound)
	wantErr(t, s.SaveAgentDiff(ctx, "web", domain.AgentDiff{Revision: "r1"}), domain.ErrNotFound) // no such app
	must(t, s.CreateApp(ctx, domain.Application{Name: "web", Host: "edge"}))
	at := time.Date(2026, 11, 9, 16, 0, 0, 0, time.UTC)
	d := domain.AgentDiff{Revision: "r2", Services: []domain.ServiceDiff{{Service: "web", Change: "changed"}}, At: at}
	must(t, s.SaveAgentDiff(ctx, "web", domain.AgentDiff{Revision: "r1"}))
	must(t, s.SaveAgentDiff(ctx, "web", d)) // the latest wins
	got, err := s.AgentDiff(ctx, "web")
	must(t, err)
	if !reflect.DeepEqual(got, d) {
		t.Fatalf("got %+v", got)
	}
	must(t, s.DeleteApp(ctx, "web"))
	_, err = s.AgentDiff(ctx, "web")
	wantErr(t, err, domain.ErrNotFound)
}
