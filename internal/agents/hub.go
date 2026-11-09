// Package agents is the Control Plane's side of the Agents' connections (Q20, Q22, Q28): it authenticates each one by
// a signed challenge, enrolls an Agent that brings an invite, and holds an unapproved Agent's connection without
// sending it anything until an admin approves its key. Two live connections with one key lock the Host (Q75); an
// admin rotates an Agent's key (Q76) or revokes it by deleting the Host.
package agents

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/logging"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
	"github.com/obleey/omegacd/internal/version"
	"github.com/obleey/omegacd/internal/wire"
)

// ChallengePrefix comes before the nonce in what an Agent signs, RotatePrefix before its new public key: its key signs
// nothing else that could pass for either.
const (
	ChallengePrefix = "omegacd-agent-v1 challenge\n"
	RotatePrefix    = "omegacd-agent-v1 rotate\n"
)

// authTimeout bounds how long a connection may take to authenticate, aliveTimeout how long a connection may take to
// answer the ping that decides whether a second one with its key is a duplicate, rotateTimeout a key rotation.
var (
	authTimeout   = 30 * time.Second // tests wait less
	aliveTimeout  = 5 * time.Second
	rotateTimeout = 30 * time.Second
)

// ErrNotApproved: the Host's Agent is connected but waits for approval; nothing is sent to it (Q28, Q74).
var ErrNotApproved = errors.New("the Host's Agent is not approved yet")

// ErrOffline: no Agent of the Host is connected.
var ErrOffline = errors.New("the Host's Agent is not connected")

// ErrDisconnected: the Agent's connection dropped during a Sync; the Agent finishes it and reports through its outbox
// on reconnect (Q93).
var ErrDisconnected = errors.New("the Host's Agent disconnected during the Sync")

type Hub struct {
	store   domain.Store
	log     *slog.Logger
	now     func() time.Time // tests move it
	version string           // this Control Plane's, which Agents' versions are checked against (Q41)
	// Blob is a file's content by its sha256 (hex), as the Source Server rendered it: what Agents ask for
	Blob func(ctx context.Context, hash string) ([]byte, error)
	// Reported is told an Application's Sync Operations changed by an Agent's report (M3-06); may be nil
	Reported func(ctx context.Context, app string)
	// Resynced is told a Host's Agent sent its checksums: Syncs waiting for the Host can go (Q81); may be nil
	Resynced func(ctx context.Context, host string)

	mu    sync.Mutex
	conns map[string]*session // by Host
}

type session struct {
	conn     *wire.Conn
	at       domain.AgentConnection
	approved chan struct{} // closed once approved
	once     sync.Once

	mu      sync.Mutex
	key     []byte           // the key it authenticated with; a rotation's signature must be by it
	rotated chan rotateReply // an admin's rotation waiting for the Agent's KeyRotation
	// deploys waiting for their SyncOperationReport, by op; files, the blobs this Agent was sent Desired States with:
	// all it may ask for, so no Host reads another's files by guessing hashes
	deploys map[uint64]chan *agentv1.SyncOperationReport
	sent    map[uint64]*agentv1.DesiredState // in flight, by op: a resync re-sends one the Agent doesn't have
	files   map[string]bool
}

type rotateReply struct {
	fingerprint string
	err         error
}

func New(store domain.Store, log *slog.Logger) *Hub {
	return &Hub{store: store, log: cmp.Or(log, logging.Discard), now: time.Now, version: version.String(), conns: map[string]*session{}}
}

// ServeHTTP is /api/agents/v1/connect.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, err := wire.Accept(w, r)
	if err != nil {
		h.log.Warn("agent connection", "err", err)
		return
	}
	defer c.Close()
	ctx := r.Context()
	addr := clientIP(r)
	host, key, hello, err := h.authenticate(ctx, c)
	if err == nil {
		ctx = logging.With(ctx, "host", host.Name)
		err = allowed(host, addr)
	}
	if err == nil {
		err = h.checkVersion(ctx, host.Name, hello)
	}
	if err == nil {
		err = h.admit(ctx, host, addr)
	}
	if err != nil {
		h.log.WarnContext(ctx, "agent refused", "remote", addr, "err", err)
		go func() { // reading finds the Agent hanging up
			for {
				if _, err := c.Recv(ctx); err != nil {
					return
				}
			}
		}()
		refuse(ctx, c, 0, err)
		return
	}
	if err := h.store.ConfirmSigningKey(ctx, host.Name, key); err != nil {
		h.log.ErrorContext(ctx, "confirm the agent's key", "err", err)
		return
	}
	s := &session{conn: c, at: domain.AgentConnection{Address: addr, Since: h.now()}, approved: make(chan struct{}), key: key,
		deploys: map[uint64]chan *agentv1.SyncOperationReport{}, sent: map[uint64]*agentv1.DesiredState{}, files: map[string]bool{}}
	if host.Approved {
		s.once.Do(func() { close(s.approved) })
	}
	h.mu.Lock()
	h.conns[host.Name] = s
	h.mu.Unlock()
	// an approval between authenticating and the line above found no session to tell: read it again
	if now, err := h.store.GetHost(ctx, host.Name); err == nil && now.Approved && now.Fingerprint == domain.Fingerprint(key) {
		s.once.Do(func() { close(s.approved) })
	}
	defer func() {
		h.mu.Lock()
		if h.conns[host.Name] == s {
			delete(h.conns, host.Name)
		}
		h.mu.Unlock()
	}()
	fingerprint := domain.Fingerprint(key)
	h.log.InfoContext(ctx, "agent connected", "fingerprint", fingerprint, "approved", host.Approved, "remote", addr)
	if err := status(ctx, c, 0, host.Name, fingerprint, host.Approved); err != nil {
		return
	}
	go func() {
		select {
		case <-s.approved:
			if !host.Approved {
				h.log.InfoContext(ctx, "agent approved", "fingerprint", fingerprint)
				status(ctx, c, 0, host.Name, fingerprint, true)
			}
		case <-c.Done():
		}
	}()
	for { // reads answer the pings; what else an Agent sends is handled from M3-06 and M4 on
		e, err := c.Recv(ctx)
		if err != nil {
			h.log.InfoContext(ctx, "agent disconnected", "err", err)
			return
		}
		switch {
		case e.GetKeyRotation() != nil:
			h.rotated(ctx, host.Name, s, e.Id, e.GetKeyRotation())
		case e.GetBlobRequest() != nil:
			go h.serveBlobs(ctx, s, e.Id, e.GetBlobRequest())
		case e.GetSyncOperationReport() != nil:
			r := e.GetSyncOperationReport()
			s.mu.Lock()
			wait := s.deploys[r.Op]
			delete(s.deploys, r.Op)
			s.mu.Unlock()
			if wait != nil {
				wait <- r
			}
		case e.GetOutboxEntry() != nil:
			h.outbox(ctx, host.Name, s, e.GetOutboxEntry())
		case e.GetResync() != nil:
			h.resync(ctx, host.Name, s, e.GetResync())
		}
	}
}

type clientIPKey struct{}

// WithClientIP gives the hub the client's address as the API worked it out: behind trusted proxies, from
// X-Forwarded-For (OMEGACD_TRUSTED_PROXIES).
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, ip)
}

func clientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey{}).(string); ok {
		return ip
	}
	addr, _, _ := net.SplitHostPort(r.RemoteAddr)
	return addr
}

// allowed refuses an address outside the Host's allowlist (Q28), whatever key it holds.
func allowed(host domain.Host, addr string) error {
	if len(host.AllowIPs) == 0 {
		return nil
	}
	prefixes, err := domain.ParsePrefixes(host.AllowIPs)
	if err != nil {
		return err
	}
	if a, err := netip.ParseAddr(addr); err == nil {
		for _, p := range prefixes {
			if p.Contains(a.Unmap()) {
				return nil
			}
		}
	}
	return fmt.Errorf("Host %s's Agent may connect only from %s, not from %s", host.Name, strings.Join(host.AllowIPs, ", "), addr)
}

// SetAllowlist limits where the Host's Agent may connect from (CIDRs or IPs; none: anywhere). A live connection from
// outside the new list is closed.
func (h *Hub) SetAllowlist(ctx context.Context, host string, cidrs []string) ([]string, error) {
	prefixes, err := domain.ParsePrefixes(cidrs)
	if err != nil {
		return nil, err
	}
	list := []string{}
	for _, p := range prefixes {
		list = append(list, p.String())
	}
	if err := h.store.SetAllowIPs(ctx, host, list); err != nil {
		return nil, err
	}
	h.log.InfoContext(ctx, "host allowlist", "host", host, "allow", list)
	if s := h.session(host); s != nil {
		if err := allowed(domain.Host{Name: host, AllowIPs: list}, s.at.Address); err != nil {
			h.log.WarnContext(ctx, "agent outside the new allowlist: disconnected", "host", host, "remote", s.at.Address)
			h.drop(ctx, host, s, err)
		}
	}
	return list, nil
}

// checkVersion records what the Agent said about itself and refuses a version this Control Plane doesn't serve (Q41).
func (h *Hub) checkVersion(ctx context.Context, host string, hello *agentv1.Hello) error {
	if hello == nil {
		return errors.New("the Agent sent no Hello before its challenge response")
	}
	err := compatible(h.version, hello.AgentVersion)
	info := domain.AgentInfo{Version: hello.AgentVersion, Protocol: hello.ProtocolVersion, Hostname: hello.Hostname,
		OS: hello.Os, Arch: hello.Arch, Engine: hello.Engine, EngineVersion: hello.EngineVersion, Rootless: hello.Rootless,
		SeenAt: h.now()}
	if err != nil {
		info.Refused = err.Error()
	}
	if rerr := h.store.RecordAgent(ctx, host, info); rerr != nil {
		h.log.ErrorContext(ctx, "record the agent", "err", rerr)
	}
	return err
}

// admit refuses a locked Host, and locks one whose Agent is connected and still answers: two live connections with one
// key mean a copied key (Q28, Q75). A connection that doesn't answer is a dead one the new connection replaces (a
// network change, a restarted Agent).
func (h *Hub) admit(ctx context.Context, host domain.Host, addr string) error {
	locked := func(conns []domain.AgentConnection) error {
		var at []string
		for _, c := range conns {
			at = append(at, c.Address+" since "+c.Since.Format(time.RFC3339))
		}
		return fmt.Errorf("Host %s is locked: two connections used its key at once (%s); an admin unlocks it, or "+
			"revokes it and adds it again", host.Name, strings.Join(at, ", "))
	}
	if len(host.Locked) > 0 {
		return locked(host.Locked)
	}
	old := h.session(host.Name)
	if old == nil {
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, aliveTimeout)
	defer cancel()
	if err := old.conn.Ping(pctx); err != nil {
		h.log.InfoContext(ctx, "agent reconnected: its old connection is gone", "old", old.at.Address, "err", err)
		old.conn.Close()
		return nil
	}
	conns := []domain.AgentConnection{old.at, {Address: addr, Since: h.now()}}
	if err := h.store.LockHost(ctx, host.Name, conns); err != nil {
		return err
	}
	h.log.ErrorContext(ctx, "two live connections with one Host's key: the Host is locked until an admin unlocks or "+
		"revokes it", "first", old.at.Address, "second", addr)
	err := locked(conns)
	h.drop(ctx, host.Name, old, err)
	return err
}

// refuse sends err (as the answer to replyTo, if not 0) and waits for the Agent to hang up: closing first could drop
// the error.
func refuse(ctx context.Context, c *wire.Conn, replyTo uint64, err error) {
	c.Send(ctx, &agentv1.Envelope{ReplyTo: replyTo, Body: &agentv1.Envelope_Error{Error: &agentv1.Error{Message: err.Error()}}})
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
	}
	c.Close()
}

func status(ctx context.Context, c *wire.Conn, replyTo uint64, host, fingerprint string, approved bool) error {
	return c.Send(ctx, &agentv1.Envelope{ReplyTo: replyTo, Body: &agentv1.Envelope_EnrollmentStatus{EnrollmentStatus: &agentv1.EnrollmentStatus{
		Host: host, Approved: approved, Fingerprint: fingerprint}}})
}

// authenticate sends a fresh challenge and returns the Host whose key signed it, the key and the Agent's Hello,
// enrolling the key first when the Agent brings an invite.
func (h *Hub) authenticate(ctx context.Context, c *wire.Conn) (domain.Host, []byte, *agentv1.Hello, error) {
	ctx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()
	nonce := make([]byte, 32)
	rand.Read(nonce)
	if err := c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_Challenge{Challenge: &agentv1.Challenge{Nonce: nonce}}}); err != nil {
		return domain.Host{}, nil, nil, err
	}
	var enroll *agentv1.Enroll
	var hello *agentv1.Hello
	for {
		e, err := c.Recv(ctx)
		if err != nil {
			return domain.Host{}, nil, nil, err
		}
		switch {
		case e.GetHello() != nil:
			hello = e.GetHello()
		case e.GetEnroll() != nil:
			enroll = e.GetEnroll()
		case e.GetChallengeResponse() != nil:
			r := e.GetChallengeResponse()
			if len(r.PublicKey) != ed25519.PublicKeySize ||
				!ed25519.Verify(r.PublicKey, append([]byte(ChallengePrefix), nonce...), r.Signature) {
				return domain.Host{}, nil, nil, errors.New("the challenge's signature doesn't verify")
			}
			host, err := h.store.HostBySigningKey(ctx, r.PublicKey)
			if enroll == nil || !errors.Is(err, domain.ErrNotFound) { // an enrolled key needs no invite, even if it brings one
				if errors.Is(err, domain.ErrNotFound) {
					return host, nil, nil, fmt.Errorf("no Host has the key %s: enroll with an invite (OMEGACD_AGENT_INVITE)", domain.Fingerprint(r.PublicKey))
				}
				return host, r.PublicKey, hello, err
			}
			host, err = h.store.Enroll(ctx, enroll.Invite, r.PublicKey, enroll.AgeRecipient, h.now())
			return host, r.PublicKey, hello, err
		}
	}
}

// Approve approves the Host's Agent if fingerprint is its key's (Q28): a connected Agent hears it at once.
func (h *Hub) Approve(ctx context.Context, host, fingerprint string) error {
	if err := h.store.ApproveHost(ctx, host, fingerprint); err != nil {
		return err
	}
	if s := h.session(host); s != nil {
		s.once.Do(func() { close(s.approved) })
	}
	return nil
}

// Unlock lets the Host's Agent connect again (Q75): the second connection had a harmless cause, such as a cloned
// machine that is gone.
func (h *Hub) Unlock(ctx context.Context, host string) error {
	if err := h.store.UnlockHost(ctx, host); err != nil {
		return err
	}
	h.log.InfoContext(ctx, "host unlocked", "host", host)
	return nil
}

// Disconnect closes the Host's live connection: its Host was deleted, which revokes its key (Q22).
func (h *Hub) Disconnect(host string) {
	if s := h.session(host); s != nil {
		h.log.Info("agent revoked: its Host was deleted", "host", host)
		s.conn.Close()
	}
}

// Rotate has the Host's connected, approved Agent make a new signing key (Q76) and returns its fingerprint. The
// connection stays up; the next one authenticates with the new key.
func (h *Hub) Rotate(ctx context.Context, host string) (string, error) {
	s, err := h.approved(host)
	if err != nil {
		return "", err
	}
	reply := make(chan rotateReply, 1)
	s.mu.Lock()
	s.rotated = reply
	s.mu.Unlock()
	if err := s.conn.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_RotateKey{RotateKey: &agentv1.RotateKey{}}}); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, rotateTimeout)
	defer cancel()
	select {
	case r := <-reply:
		return r.fingerprint, r.err
	case <-s.conn.Done():
		return "", fmt.Errorf("host %s: the Agent disconnected during the rotation: %w", host, ErrOffline)
	case <-ctx.Done():
		return "", fmt.Errorf("host %s: the Agent didn't rotate its key: %w", host, ctx.Err())
	}
}

// rotated takes the Agent's new key, signed by the one it authenticated with, answers it and tells Rotate.
func (h *Hub) rotated(ctx context.Context, host string, s *session, id uint64, r *agentv1.KeyRotation) {
	s.mu.Lock()
	key, reply := s.key, s.rotated
	s.rotated = nil
	s.mu.Unlock()
	err := errors.New("no rotation was asked for")
	if reply != nil {
		err = errors.New("the new key's signature doesn't verify")
		if len(r.PublicKey) == ed25519.PublicKeySize && ed25519.Verify(key, append([]byte(RotatePrefix), r.PublicKey...), r.Signature) {
			err = h.store.RotateSigningKey(ctx, host, key, r.PublicKey)
		}
	}
	fingerprint := domain.Fingerprint(r.PublicKey)
	if err != nil {
		h.log.WarnContext(ctx, "key rotation refused", "err", err)
		s.conn.Send(ctx, &agentv1.Envelope{ReplyTo: id, Body: &agentv1.Envelope_Error{Error: &agentv1.Error{Message: err.Error()}}})
	} else {
		s.mu.Lock()
		s.key = r.PublicKey
		s.mu.Unlock()
		h.log.InfoContext(ctx, "agent key rotated", "fingerprint", fingerprint)
		status(ctx, s.conn, id, host, fingerprint, true)
	}
	if reply != nil {
		reply <- rotateReply{fingerprint, err}
	}
}

// drop refuses a live session with err and forgets it now, not when its Agent hangs up: a connection right after must
// not find it (and, still answering, take it for a second live one).
func (h *Hub) drop(ctx context.Context, host string, s *session, err error) {
	h.mu.Lock()
	if h.conns[host] == s {
		delete(h.conns, host)
	}
	h.mu.Unlock()
	go refuse(ctx, s.conn, 0, err)
}

func (h *Hub) session(host string) *session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[host]
}

func (h *Hub) approved(host string) (*session, error) {
	s := h.session(host)
	if s == nil {
		return nil, fmt.Errorf("host %s: %w", host, ErrOffline)
	}
	select {
	case <-s.approved:
		return s, nil
	default:
		return nil, fmt.Errorf("host %s: %w", host, ErrNotApproved)
	}
}

// Deploy sends ds to the Host's Agent and waits for its report: nil once it deployed, or why it failed. ErrOffline
// and ErrNotApproved when there's no Agent to send it to; ErrDisconnected when the connection dropped meanwhile.
func (h *Hub) Deploy(ctx context.Context, host string, ds *agentv1.DesiredState) error {
	s, err := h.approved(host)
	if err != nil {
		return err
	}
	report := make(chan *agentv1.SyncOperationReport, 1)
	s.mu.Lock()
	s.deploys[ds.Op] = report
	s.sent[ds.Op] = ds
	for _, f := range ds.Files {
		s.files[hex.EncodeToString(f.Sha256)] = true
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.deploys, ds.Op)
		delete(s.sent, ds.Op)
		s.mu.Unlock()
	}()
	if err := s.conn.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_DesiredState{DesiredState: ds}}); err != nil {
		return fmt.Errorf("host %s: %w", host, ErrDisconnected)
	}
	select {
	case r := <-report:
		if r.Phase != string(domain.OpSucceeded) {
			return errors.New(r.Message)
		}
		return nil
	case <-s.conn.Done():
		return fmt.Errorf("host %s: %w", host, ErrDisconnected)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// outbox applies an entry the Agent recorded (offline, or when a report couldn't be sent) and acknowledges it; one
// sent again is acknowledged again and applied once (M3-06).
func (h *Hub) outbox(ctx context.Context, host string, s *session, e *agentv1.OutboxEntry) {
	r := e.GetSyncOperationReport()
	if r == nil { // a StatusReport: Health and drift come with M4-02
		s.conn.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_OutboxAck{OutboxAck: &agentv1.OutboxAck{Seq: e.Seq}}})
		return
	}
	applied, err := h.store.ApplyAgentReport(ctx, host, e.Seq, domain.AgentReport{App: r.App, Revision: r.Revision,
		Checksum: r.Checksum, Phase: domain.OpPhase(r.Phase), Message: r.Message, Trigger: r.Trigger, Op: int64(r.Op),
		At: time.Unix(e.TimeUnix, 0)})
	if err != nil { // no ack: the Agent sends it again on its next connection
		h.log.ErrorContext(ctx, "apply the agent's report", "seq", e.Seq, "err", err)
		return
	}
	h.log.InfoContext(ctx, "agent report", "seq", e.Seq, "app", r.App, "op", r.Op, "trigger", r.Trigger, "phase", r.Phase, "applied", applied)
	s.conn.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_OutboxAck{OutboxAck: &agentv1.OutboxAck{Seq: e.Seq}}})
	if applied && h.Reported != nil {
		h.Reported(ctx, r.App)
	}
}

// resync takes the Agent's checksums (Q78): a Desired State in flight that the Agent doesn't have was lost on the way
// and goes again; then Syncs waiting for the Host go (Resynced). It never deletes (Q91).
func (h *Hub) resync(ctx context.Context, host string, s *session, r *agentv1.Resync) {
	s.mu.Lock()
	var again []*agentv1.DesiredState
	for _, ds := range s.sent {
		if hex.EncodeToString(r.Checksums[ds.App]) != ds.Checksum {
			again = append(again, ds)
		}
	}
	s.mu.Unlock()
	for _, ds := range again {
		h.log.InfoContext(ctx, "resync: a desired state the agent lacks, again", "app", ds.App, "op", ds.Op)
		s.conn.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_DesiredState{DesiredState: ds}})
	}
	if h.Resynced != nil {
		h.Resynced(ctx, host)
	}
}

// serveBlobs sends the blobs the Agent asks for, each from the offset it already has: only files of Desired States
// it was sent. A blob it can't have is answered with an Error to the request.
func (h *Hub) serveBlobs(ctx context.Context, s *session, id uint64, req *agentv1.BlobRequest) {
	for i, sum := range req.Sha256 {
		hash := hex.EncodeToString(sum)
		s.mu.Lock()
		ok := s.files[hash]
		s.mu.Unlock()
		err := fmt.Errorf("blob %s: not a file of a Desired State sent to this Agent", hash)
		var b []byte
		if ok && h.Blob != nil {
			b, err = h.Blob(ctx, hash)
		}
		var offset uint64
		if i < len(req.Offset) {
			offset = req.Offset[i]
		}
		if err == nil {
			err = s.conn.SendBlob(ctx, sum, b, offset)
		}
		if err != nil {
			h.log.WarnContext(ctx, "blob for an agent", "blob", hash, "err", err)
			s.conn.Send(ctx, &agentv1.Envelope{ReplyTo: id, Body: &agentv1.Envelope_Error{Error: &agentv1.Error{Message: err.Error()}}})
			return
		}
	}
}

// Send sends e to the Host's Agent: never to one that isn't approved.
func (h *Hub) Send(ctx context.Context, host string, e *agentv1.Envelope) error {
	s, err := h.approved(host)
	if err != nil {
		return err
	}
	return s.conn.Send(ctx, e)
}
