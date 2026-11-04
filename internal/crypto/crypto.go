// Package crypto seals at-rest secrets with the dockercd age identity.
package crypto

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"os"

	"filippo.io/age"
)

// LoadIdentity reads the first X25519 identity from an age key file.
func LoadIdentity(path string) (*age.X25519Identity, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ids, err := age.ParseIdentities(f)
	if err != nil {
		return nil, fmt.Errorf("parse age key %s: %w", path, err)
	}
	for _, id := range ids {
		if x, ok := id.(*age.X25519Identity); ok {
			return x, nil
		}
	}
	return nil, fmt.Errorf("no X25519 identity in %s", path)
}

type Sealer struct {
	id *age.X25519Identity
}

func NewSealer(id *age.X25519Identity) *Sealer { return &Sealer{id: id} }

func (s *Sealer) Seal(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, s.id.Recipient())
	if err != nil {
		return "", err
	}
	if _, err := io.WriteString(w, plaintext); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

func (s *Sealer) Open(sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", err
	}
	r, err := age.Decrypt(bytes.NewReader(raw), s.id)
	if err != nil {
		return "", err
	}
	b, err := io.ReadAll(r)
	return string(b), err
}
