package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/obleey/omegacd/internal/agents"
	"github.com/obleey/omegacd/internal/domain"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
	"github.com/obleey/omegacd/internal/version"
	"github.com/obleey/omegacd/internal/wire"
)

// ProtocolVersion is the Agent ↔ Control Plane protocol this Agent speaks (Q41).
const ProtocolVersion = 1

// SigningKey loads the Agent's ed25519 signing key from stateDir, creating it at the first start (Q22): every
// connection signs a challenge with it. Owner-only, like the Host Key.
func SigningKey(stateDir string) (ed25519.PrivateKey, error) {
	path := filepath.Join(stateDir, "signing.key")
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		_, key, err := ed25519.GenerateKey(nil)
		if err != nil {
			return nil, err
		}
		pem := fmt.Sprintf("# created: %s\n# fingerprint: %s\n%s\n", time.Now().Format(time.RFC3339),
			domain.Fingerprint(key.Public().(ed25519.PublicKey)), base64.StdEncoding.EncodeToString(key.Seed()))
		return key, writeNew(path, []byte(pem))
	}
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		seed, err := base64.StdEncoding.DecodeString(line)
		if err != nil || len(seed) != ed25519.SeedSize {
			break
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	return nil, fmt.Errorf("%s: not an omegacd signing key", path)
}

// Connection is how the Agent reaches its Control Plane.
type Connection struct {
	URL          string // the Control Plane's, e.g. https://omegacd.example.com
	Invite       string // sent until the Agent is enrolled; the Control Plane ignores it after
	Key          ed25519.PrivateKey
	AgeRecipient string // of the Host Key, registered at enrollment
}

// Connect holds the connection to the Control Plane until ctx ends, reconnecting with backoff (Q20, Q51).
func (ag *Agent) Connect(ctx context.Context, c Connection) {
	url := strings.TrimSuffix(c.URL, "/") + "/api/agents/v1/connect"
	wire.Keep(ctx, func(ctx context.Context) (*wire.Conn, error) { return wire.Dial(ctx, url, nil) },
		func(ctx context.Context, conn *wire.Conn) error { return ag.serve(ctx, conn, c) },
		func(err error) {
			ag.log.Warn("no connection to the Control Plane; trying again", "url", c.URL, "err", err)
		})
}

// serve answers the challenge, enrolling with the invite if there is one, then reads until the connection ends.
func (ag *Agent) serve(ctx context.Context, conn *wire.Conn, c Connection) error {
	for {
		e, err := conn.Recv(ctx)
		if err != nil {
			return err
		}
		switch {
		case e.GetChallenge() != nil:
			host, _ := os.Hostname()
			out := []*agentv1.Envelope{{Body: &agentv1.Envelope_Hello{Hello: &agentv1.Hello{AgentVersion: version.String(),
				ProtocolVersion: ProtocolVersion, Os: runtime.GOOS, Arch: runtime.GOARCH, Hostname: host}}}}
			if c.Invite != "" {
				out = append(out, &agentv1.Envelope{Body: &agentv1.Envelope_Enroll{Enroll: &agentv1.Enroll{
					Invite: c.Invite, AgeRecipient: c.AgeRecipient}}})
			}
			sig := ed25519.Sign(c.Key, append([]byte(agents.ChallengePrefix), e.GetChallenge().Nonce...))
			out = append(out, &agentv1.Envelope{Body: &agentv1.Envelope_ChallengeResponse{ChallengeResponse: &agentv1.ChallengeResponse{
				PublicKey: c.Key.Public().(ed25519.PublicKey), Signature: sig}}})
			for _, o := range out {
				if err := conn.Send(ctx, o); err != nil {
					return err
				}
			}
		case e.GetEnrollmentStatus() != nil:
			s := e.GetEnrollmentStatus()
			if s.Approved {
				ag.log.Info("connected to the Control Plane, approved", "host", s.Host)
			} else {
				ag.log.Warn("enrolled, waiting for an admin to approve this Agent: the fingerprint the Control Plane shows "+
					"must be this one", "host", s.Host, "fingerprint", s.Fingerprint)
			}
		case e.GetError() != nil:
			return fmt.Errorf("the Control Plane refused this Agent: %s", e.GetError().Message)
		}
	}
}
