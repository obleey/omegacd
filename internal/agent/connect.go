package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"filippo.io/age"

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
	// a rotation the Agent didn't finish: the Control Plane takes the old key back at the next connection
	if err := os.Remove(path + ".next"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		key, err := newSigningKey(path)
		return key, err
	}
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 { // Q77: owner-only
		return nil, fmt.Errorf("%s can be read by others (mode %v): chmod 600 it, and rotate the key if anyone may have copied it", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
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

// newSigningKey writes a new key to path, owner-only; never over an existing file.
func newSigningKey(path string) (ed25519.PrivateKey, error) {
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	pem := fmt.Sprintf("# created: %s\n# fingerprint: %s\n%s\n", time.Now().Format(time.RFC3339),
		domain.Fingerprint(key.Public().(ed25519.PublicKey)), base64.StdEncoding.EncodeToString(key.Seed()))
	return key, writeNew(path, []byte(pem))
}

// rotate writes a new signing key to path, owner-only, replacing one a refused rotation left.
func rotate(path string) (ed25519.PrivateKey, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return newSigningKey(path)
}

// TrustCA is an HTTP client trusting the PEM certificates in file besides the system's (OMEGACD_AGENT_CA_FILE).
func TrustCA(file string) (*http.Client, error) {
	pem, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("OMEGACD_AGENT_CA_FILE: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("OMEGACD_AGENT_CA_FILE: no PEM certificate in %s", file)
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: t}, nil
}

// Connection is how the Agent reaches its Control Plane.
type Connection struct {
	URL          string              // the Control Plane's, e.g. https://omegacd.example.com
	Invite       string              // sent until the Agent is enrolled; the Control Plane ignores it after
	Key          ed25519.PrivateKey  // a rotation replaces it
	KeyPath      string              // where Key is kept: a rotation writes the new one next to it, then replaces it
	AgeRecipient string              // of the Host Key, registered at enrollment
	Engine       EngineInfo          // reported in Hello; empty when none was reachable at start
	HTTPClient   *http.Client        // trusts OMEGACD_AGENT_CA_FILE's CAs; nil: the system's (Q95)
	HostKey      *age.X25519Identity // decrypts the registry credentials the Control Plane sends (Q86)
}

// Connect holds the connection to the Control Plane until ctx ends, reconnecting with backoff (Q20, Q51).
func (ag *Agent) Connect(ctx context.Context, c Connection) {
	url := strings.TrimSuffix(c.URL, "/") + "/api/agents/v1/connect"
	wire.Keep(ctx, func(ctx context.Context) (*wire.Conn, error) { return wire.DialWith(ctx, url, nil, c.HTTPClient) },
		func(ctx context.Context, conn *wire.Conn) error { return ag.serve(ctx, conn, &c) },
		func(err error) {
			ag.log.Warn("no connection to the Control Plane; trying again", "url", c.URL, "err", err)
		})
}

// serve answers the challenge, enrolling with the invite if there is one, then reads until the connection ends.
func (ag *Agent) serve(ctx context.Context, conn *wire.Conn, c *Connection) error {
	var rotation uint64 // the KeyRotation waiting for its answer
	var next ed25519.PrivateKey
	b := newBlobs(conn)
	q := newRequests(conn)
	var logsMu sync.Mutex
	logs := map[uint64]context.CancelFunc{} // requests streaming logs, by id: a LogsStop ends one
	defer func() {
		ag.reports.connected(nil)
		logsMu.Lock()
		for _, stop := range logs {
			stop()
		}
		logsMu.Unlock()
	}()
	for {
		e, err := conn.Recv(ctx)
		if err != nil {
			return err
		}
		switch {
		case e.GetChallenge() != nil:
			host, _ := os.Hostname()
			out := []*agentv1.Envelope{{Body: &agentv1.Envelope_Hello{Hello: &agentv1.Hello{AgentVersion: version.String(),
				ProtocolVersion: ProtocolVersion, Os: runtime.GOOS, Arch: runtime.GOARCH, Hostname: host,
				Engine: c.Engine.Name, EngineVersion: c.Engine.Version, Rootless: c.Engine.Rootless}}}}
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
		case e.GetRotateKey() != nil:
			if rotation != 0 {
				continue
			}
			var err error
			if next, err = rotate(c.KeyPath + ".next"); err != nil {
				ag.log.Error("key rotation: no new key", "err", err)
				continue
			}
			pub := next.Public().(ed25519.PublicKey)
			r := &agentv1.Envelope{Body: &agentv1.Envelope_KeyRotation{KeyRotation: &agentv1.KeyRotation{PublicKey: pub,
				Signature: ed25519.Sign(c.Key, append([]byte(agents.RotatePrefix), pub...))}}}
			if err := conn.Send(ctx, r); err != nil {
				return err
			}
			rotation = r.Id
		case rotation != 0 && e.ReplyTo == rotation:
			rotation = 0
			if e.GetError() != nil {
				ag.log.Error("the Control Plane refused the new signing key; the old one stays", "err", e.GetError().Message)
				os.Remove(c.KeyPath + ".next")
				continue
			}
			// the Control Plane has the new key, and the old one until the Agent authenticates with the new
			if err := os.Rename(c.KeyPath+".next", c.KeyPath); err != nil {
				ag.log.Error("key rotation: the new key couldn't replace the old one, which the Control Plane takes back "+
					"at the next connection", "err", err)
				continue
			}
			c.Key = next
			ag.log.Info("signing key rotated", "fingerprint", e.GetEnrollmentStatus().GetFingerprint())
		case e.GetOutboxAck() != nil:
			if err := ag.db.Ack(ctx, e.GetOutboxAck().Seq); err != nil {
				ag.log.Error("outbox ack", "err", err)
			}
		case e.GetEnrollmentStatus() != nil:
			s := e.GetEnrollmentStatus()
			if s.EnrolledNow && ag.db != nil {
				ag.dropOutbox(ctx)
			}
			if s.Approved {
				ag.log.Info("connected to the Control Plane, approved", "host", s.Host)
				if ag.db != nil { // an Agent without its state (tests of the connection alone) has nothing to sync
					ag.reports.connected(conn)
					go ag.keepInSync(ctx, conn)
				}
			} else {
				ag.log.Warn("enrolled, waiting for an admin to approve this Agent: the fingerprint the Control Plane shows "+
					"must be this one", "host", s.Host, "fingerprint", s.Fingerprint)
			}
		case e.ReplyTo != 0 && e.ReplyTo != rotation && q.answer(e):
		case e.GetDesiredState() != nil:
			go ag.desired(ctx, conn, b, q, c.HostKey, e.GetDesiredState())
		case e.GetRemoveApp() != nil:
			go ag.removeApp(ctx, conn, e.Id, e.GetRemoveApp())
		case e.GetDiffRequest() != nil:
			go ag.answerDiff(ctx, conn, b, e.Id, e.GetDiffRequest())
		case e.GetLogsRequest() != nil:
			lctx, stop := context.WithCancel(ctx)
			logsMu.Lock()
			logs[e.Id] = stop
			logsMu.Unlock()
			go func(id uint64) {
				ag.streamLogs(lctx, conn, id, e.GetLogsRequest())
				logsMu.Lock()
				delete(logs, id)
				logsMu.Unlock()
				stop()
			}(e.Id)
		case e.GetLogsStop() != nil:
			logsMu.Lock()
			if stop := logs[e.ReplyTo]; stop != nil {
				stop()
			}
			logsMu.Unlock()
		case e.GetBlobChunk() != nil:
			b.chunk(e.GetBlobChunk())
		case e.GetError() != nil && e.ReplyTo != 0 && b.refused(e.ReplyTo, e.GetError().Message):
		case e.GetError() != nil:
			return fmt.Errorf("the Control Plane refused this Agent: %s", e.GetError().Message)
		}
	}
}
