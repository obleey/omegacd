// Package agents is the Control Plane's side of the Agents' connections (Q20, Q22, Q28): it authenticates each one by
// a signed challenge, enrolls an Agent that brings an invite, and holds an unapproved Agent's connection without
// sending it anything until an admin approves its key.
package agents

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/logging"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
	"github.com/obleey/omegacd/internal/wire"
)

// ChallengePrefix comes before the nonce in what an Agent signs: its key signs nothing else that could pass for it.
const ChallengePrefix = "omegacd-agent-v1 challenge\n"

// authTimeout bounds how long a connection may take to authenticate.
var authTimeout = 30 * time.Second // tests wait less

// ErrNotApproved: the Host's Agent is connected but waits for approval; nothing is sent to it (Q28, Q74).
var ErrNotApproved = errors.New("the Host's Agent is not approved yet")

// ErrOffline: no Agent of the Host is connected.
var ErrOffline = errors.New("the Host's Agent is not connected")

type Hub struct {
	store domain.Store
	log   *slog.Logger
	now   func() time.Time // tests move it

	mu    sync.Mutex
	conns map[string]*session // by Host
}

type session struct {
	conn     *wire.Conn
	approved chan struct{} // closed once approved
	once     sync.Once
}

func New(store domain.Store, log *slog.Logger) *Hub {
	return &Hub{store: store, log: cmp.Or(log, logging.Discard), now: time.Now, conns: map[string]*session{}}
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
	host, err := h.authenticate(ctx, c)
	if err != nil {
		h.log.Warn("agent refused", "remote", r.RemoteAddr, "err", err)
		c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_Error{Error: &agentv1.Error{Message: err.Error()}}})
		// the Agent hangs up once it has read the error; closing first could drop it
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		for {
			if _, err := c.Recv(ctx); err != nil {
				return
			}
		}
	}
	ctx = logging.With(ctx, "host", host.Name)
	s := &session{conn: c, approved: make(chan struct{})}
	if host.Approved {
		s.once.Do(func() { close(s.approved) })
	}
	h.mu.Lock()
	h.conns[host.Name] = s // ponytail: the newest connection wins; a second one locks the Host with M3-03
	h.mu.Unlock()
	// an approval between authenticating and the line above found no session to tell: read it again
	if now, err := h.store.GetHost(ctx, host.Name); err == nil && now.Approved && now.Fingerprint == host.Fingerprint {
		s.once.Do(func() { close(s.approved) })
	}
	defer func() {
		h.mu.Lock()
		if h.conns[host.Name] == s {
			delete(h.conns, host.Name)
		}
		h.mu.Unlock()
	}()
	h.log.InfoContext(ctx, "agent connected", "fingerprint", host.Fingerprint, "approved", host.Approved)
	if err := status(ctx, c, host.Name, host.Fingerprint, host.Approved); err != nil {
		return
	}
	go func() {
		select {
		case <-s.approved:
			if !host.Approved {
				h.log.InfoContext(ctx, "agent approved", "fingerprint", host.Fingerprint)
				status(ctx, c, host.Name, host.Fingerprint, true)
			}
		case <-c.Done():
		}
	}()
	for { // reads answer the pings; what an Agent sends is handled from M3-06 and M4 on
		if _, err := c.Recv(ctx); err != nil {
			h.log.InfoContext(ctx, "agent disconnected", "err", err)
			return
		}
	}
}

func status(ctx context.Context, c *wire.Conn, host, fingerprint string, approved bool) error {
	return c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_EnrollmentStatus{EnrollmentStatus: &agentv1.EnrollmentStatus{
		Host: host, Approved: approved, Fingerprint: fingerprint}}})
}

// authenticate sends a fresh challenge and returns the Host whose key signed it, enrolling the key first when the
// Agent brings an invite.
func (h *Hub) authenticate(ctx context.Context, c *wire.Conn) (domain.Host, error) {
	ctx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()
	nonce := make([]byte, 32)
	rand.Read(nonce)
	if err := c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_Challenge{Challenge: &agentv1.Challenge{Nonce: nonce}}}); err != nil {
		return domain.Host{}, err
	}
	var enroll *agentv1.Enroll
	for {
		e, err := c.Recv(ctx)
		if err != nil {
			return domain.Host{}, err
		}
		switch {
		case e.GetEnroll() != nil:
			enroll = e.GetEnroll()
		case e.GetChallengeResponse() != nil:
			r := e.GetChallengeResponse()
			if len(r.PublicKey) != ed25519.PublicKeySize ||
				!ed25519.Verify(r.PublicKey, append([]byte(ChallengePrefix), nonce...), r.Signature) {
				return domain.Host{}, errors.New("the challenge's signature doesn't verify")
			}
			host, err := h.store.HostBySigningKey(ctx, r.PublicKey)
			if enroll == nil || !errors.Is(err, domain.ErrNotFound) { // an enrolled key needs no invite, even if it brings one
				if errors.Is(err, domain.ErrNotFound) {
					return host, fmt.Errorf("no Host has the key %s: enroll with an invite (OMEGACD_AGENT_INVITE)", domain.Fingerprint(r.PublicKey))
				}
				return host, err
			}
			return h.store.Enroll(ctx, enroll.Invite, r.PublicKey, enroll.AgeRecipient, h.now())
		}
		// a Hello is read and, until M3-04, not checked
	}
}

// Approve approves the Host's Agent if fingerprint is its key's (Q28): a connected Agent hears it at once.
func (h *Hub) Approve(ctx context.Context, host, fingerprint string) error {
	if err := h.store.ApproveHost(ctx, host, fingerprint); err != nil {
		return err
	}
	h.mu.Lock()
	s := h.conns[host]
	h.mu.Unlock()
	if s != nil {
		s.once.Do(func() { close(s.approved) })
	}
	return nil
}

// Send sends e to the Host's Agent: never to one that isn't approved.
func (h *Hub) Send(ctx context.Context, host string, e *agentv1.Envelope) error {
	h.mu.Lock()
	s := h.conns[host]
	h.mu.Unlock()
	if s == nil {
		return fmt.Errorf("host %s: %w", host, ErrOffline)
	}
	select {
	case <-s.approved:
	default:
		return fmt.Errorf("host %s: %w", host, ErrNotApproved)
	}
	return s.conn.Send(ctx, e)
}
