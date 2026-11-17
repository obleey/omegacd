package agents

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/logging"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
	"github.com/obleey/omegacd/internal/store"
	"github.com/obleey/omegacd/internal/wire"
)

// faultyStore fails the writes a test picks; approve approves the Host as its Agent's key is confirmed, as an admin
// approving at that moment does; allow is the allowlist the Host is read with.
type faultyStore struct {
	*store.Store
	mu   sync.Mutex // sessions read it while the test changes it
	what faults
}

type faults struct {
	confirm, record, lock, apply error
	approve                      string
	allow                        []string
}

func (f *faultyStore) set(w faults) { f.mu.Lock(); f.what = w; f.mu.Unlock() }
func (f *faultyStore) get() faults  { f.mu.Lock(); defer f.mu.Unlock(); return f.what }

func (f *faultyStore) ConfirmSigningKey(ctx context.Context, host string, key []byte) error {
	w := f.get()
	if w.confirm != nil {
		return w.confirm
	}
	if w.approve != "" {
		f.Store.ApproveHost(ctx, host, w.approve)
	}
	return f.Store.ConfirmSigningKey(ctx, host, key)
}

func (f *faultyStore) RecordAgent(ctx context.Context, host string, info domain.AgentInfo) error {
	if w := f.get(); w.record != nil {
		return w.record
	}
	return f.Store.RecordAgent(ctx, host, info)
}

func (f *faultyStore) LockHost(ctx context.Context, host string, conns []domain.AgentConnection) error {
	if w := f.get(); w.lock != nil {
		return w.lock
	}
	return f.Store.LockHost(ctx, host, conns)
}

func (f *faultyStore) HostBySigningKey(ctx context.Context, key []byte) (domain.Host, error) {
	h, err := f.Store.HostBySigningKey(ctx, key)
	if w := f.get(); w.allow != nil {
		h.AllowIPs = w.allow
	}
	return h, err
}

func (f *faultyStore) ApplyAgentReport(ctx context.Context, host string, seq uint64, r domain.AgentReport) (bool, error) {
	if w := f.get(); w.apply != nil {
		return false, w.apply
	}
	return f.Store.ApplyAgentReport(ctx, host, seq, r)
}

func (f *faultyStore) ApplyAgentRemoval(ctx context.Context, host string, seq uint64, app string) (bool, error) {
	if w := f.get(); w.apply != nil {
		return false, w.apply
	}
	return f.Store.ApplyAgentRemoval(ctx, host, seq, app)
}

func faultyHub(t *testing.T) (*Hub, *store.Store, string, *faultyStore) {
	t.Helper()
	var fs *faultyStore
	h, st, url := newHubOn(t, func(st *store.Store) domain.Store { fs = &faultyStore{Store: st}; return fs })
	return h, st, url, fs
}

func logTo(h *Hub) *logging.Buffer {
	logs := &logging.Buffer{}
	h.log, _ = logging.New(logs, "debug", "text") // with what the context carries (host), as omegacd logs
	return logs
}

func waitLog(t *testing.T, logs *logging.Buffer, want string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(logs.String(), want); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the log never said %q:\n%s", want, logs.String())
		}
	}
}

// What the hub can't record or read on a connection: the Agent's Hello (logged, the connection served), its key's
// confirmation (logged, the connection ended), the lock of a copied key (the copy refused with it), an allowlist it
// can't parse (refused); and a request that isn't a WebSocket (logged). Each logged one is checked (Q132).
func TestHubStoreFailures(t *testing.T) {
	h, st, url, fs := faultyHub(t)
	logs := logTo(h)
	key := approvedAgent(t, h, st, url, "web")
	fs.set(faults{record: errors.New("disk full")})
	if _, e := agent(t, url, key, "", nil); !e.GetEnrollmentStatus().GetApproved() {
		t.Fatalf("not served when the Hello can't be recorded: %v", e)
	}
	waitLog(t, logs, `msg="record the agent" err="disk full" host=web`)

	fs.set(faults{lock: errors.New("database is locked")})
	first, _ := agent(t, url, key, "", nil)
	reading(first)
	_, e := agent(t, url, key, "", nil)
	refused(t, e, "database is locked")
	first.Close()

	fs.set(faults{allow: []string{"not an address"}})
	_, e = agent(t, url, key, "", nil)
	refused(t, e, "not an address")

	fs.set(faults{confirm: errors.New("disk full")})
	c, err := wire.Dial(t.Context(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := c.Recv(t.Context())
	c.Send(t.Context(), &agentv1.Envelope{Body: &agentv1.Envelope_Hello{Hello: &agentv1.Hello{AgentVersion: "(devel)", ProtocolVersion: 1}}})
	c.Send(t.Context(), &agentv1.Envelope{Body: &agentv1.Envelope_ChallengeResponse{ChallengeResponse: &agentv1.ChallengeResponse{
		PublicKey: key.Public().(ed25519.PublicKey), Signature: ed25519.Sign(key, append([]byte(ChallengePrefix), ch.GetChallenge().Nonce...))}}})
	if _, err := c.Recv(t.Context()); err == nil {
		t.Fatal("served without its key confirmed")
	}
	waitLog(t, logs, `msg="confirm the agent's key" err="disk full" host=web`)

	resp, err := http.Get("http" + strings.TrimPrefix(url, "ws"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	waitLog(t, logs, `msg="agent connection"`)

	if _, err := h.SetAllowlist(t.Context(), "nowhere", []string{"10.0.0.0/8"}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("allowlist of an unknown host: %v", err)
	}
	if err := h.Unlock(t.Context(), "nowhere"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unlock of an unknown host: %v", err)
	}
}

// An admin approving between the Agent authenticating and its session starting isn't missed: the Agent hears it.
func TestApprovalWhileConnecting(t *testing.T) {
	h, st, url, fs := faultyHub(t)
	invite, _ := st.CreateInvite(t.Context(), "web", time.Now().Add(time.Hour))
	key := newKey()
	c, e := agent(t, url, key, invite, nil)
	if e.GetEnrollmentStatus().GetApproved() {
		t.Fatal("approved at once")
	}
	c.Close()
	fs.set(faults{approve: domain.Fingerprint(key.Public().(ed25519.PublicKey))})
	c, e = agent(t, url, key, "", nil)
	got := reading(c)
	if !e.GetEnrollmentStatus().GetApproved() {
		if e := next(t, got); !e.GetEnrollmentStatus().GetApproved() {
			t.Fatalf("never heard its approval: %v", e)
		}
	}
	if err := h.Send(t.Context(), "web", &agentv1.Envelope{Body: &agentv1.Envelope_DesiredState{DesiredState: &agentv1.DesiredState{}}}); err != nil {
		t.Fatalf("not approved on the hub's side: %v", err)
	}
}

// A rotation the Agent doesn't finish ends with why: it disconnected, or it never answered in time.
func TestRotationUnanswered(t *testing.T) {
	rotateTimeout = 200 * time.Millisecond
	t.Cleanup(func() { rotateTimeout = 30 * time.Second })
	h, st, url := newHub(t)
	key := approvedAgent(t, h, st, url, "web")
	c, _ := agent(t, url, key, "", nil)
	got := reading(c)
	if _, err := h.Rotate(t.Context(), "web"); err == nil || !strings.Contains(err.Error(), "didn't rotate its key") {
		t.Fatalf("unanswered: %v", err)
	}
	next(t, got) // its RotateKey, never answered
	go func() { next(t, got); c.Close() }()
	if _, err := h.Rotate(t.Context(), "web"); !errors.Is(err, ErrOffline) {
		t.Fatalf("disconnected: %v", err)
	}
}

// A Deploy given up while its Agent works on it returns at once; the report, if it comes, finds no one waiting.
func TestDeployGivenUp(t *testing.T) {
	h, st, url := newHub(t)
	key := approvedAgent(t, h, st, url, "web")
	c, _ := agent(t, url, key, "", nil)
	reading(c)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := h.Deploy(ctx, "web", &agentv1.DesiredState{App: "app", Op: 1}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
}

// Q144: an outbox entry of a kind the hub doesn't know is acknowledged, so the Agent stops sending it, and logged at
// Warn; an envelope it doesn't handle is logged at debug.
func TestUnknownFromTheAgent(t *testing.T) {
	h, st, url := newHub(t)
	logs := logTo(h)
	key := approvedAgent(t, h, st, url, "web")
	c, _ := agent(t, url, key, "", nil)
	got := reading(c)
	c.Send(t.Context(), &agentv1.Envelope{Body: &agentv1.Envelope_OutboxEntry{OutboxEntry: &agentv1.OutboxEntry{Seq: 5}}})
	if e := next(t, got); e.GetOutboxAck().GetSeq() != 5 {
		t.Fatalf("got %v, want the ack", e)
	}
	waitLog(t, logs, `level=WARN msg="outbox entry of a kind this Control Plane doesn't know: acknowledged and dropped; upgrade the Control Plane" seq=5 host=web`)
	c.Send(t.Context(), &agentv1.Envelope{Body: &agentv1.Envelope_Challenge{Challenge: &agentv1.Challenge{}}})
	waitLog(t, logs, `msg="an envelope the hub doesn't handle: ignored"`)
}

// An Agent's report the store can't apply is logged and not acknowledged: the Agent sends it again on its next
// connection (Q132).
func TestOutboxNotApplied(t *testing.T) {
	h, st, url, fs := faultyHub(t)
	logs := logTo(h)
	key := approvedAgent(t, h, st, url, "web")
	fs.set(faults{apply: errors.New("database is locked")})
	c, _ := agent(t, url, key, "", nil)
	c.Send(t.Context(), &agentv1.Envelope{Body: &agentv1.Envelope_OutboxEntry{OutboxEntry: &agentv1.OutboxEntry{Seq: 1,
		Event: &agentv1.OutboxEntry_SyncOperationReport{SyncOperationReport: &agentv1.SyncOperationReport{App: "app", Phase: "Succeeded"}}}}})
	c.Send(t.Context(), &agentv1.Envelope{Body: &agentv1.Envelope_OutboxEntry{OutboxEntry: &agentv1.OutboxEntry{Seq: 2,
		Event: &agentv1.OutboxEntry_AppRemoved{AppRemoved: &agentv1.AppRemoved{App: "app"}}}}})
	waitLog(t, logs, `level=ERROR msg="apply the agent's report" seq=1 err="database is locked" host=web`)
	waitLog(t, logs, `level=ERROR msg="apply the agent's removal" seq=2 err="database is locked" host=web`)
}
