// Package crypto seals at-rest secrets with the omegacd age identity.
package crypto

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

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
	return nil, fmt.Errorf("no X25519 identity in %s: a post-quantum key (age-keygen -pq) isn't supported; make one with age-keygen", path)
}

// LoadOrCreateIdentity reads the age key at path, creating it (owner-only, its folder too) when there is none; created
// says it was.
func LoadOrCreateIdentity(path string) (id *age.X25519Identity, created bool, err error) {
	id, err = LoadIdentity(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return id, false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	id, err = age.GenerateX25519Identity()
	if err != nil { // coverage: unreachable, generating reads the system's randomness, which doesn't fail
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, false, err
	}
	_, err = fmt.Fprintf(f, "# public key: %s\n%s\n", id.Recipient(), id)
	if err = errors.Join(err, f.Close()); err != nil { // coverage: unreachable, writing a few bytes to a file just created
		return nil, false, err
	}
	return id, true, nil
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
	if err != nil { // coverage: unreachable, encrypting in memory to an X25519 recipient
		return "", err
	}
	if _, err := io.WriteString(w, plaintext); err != nil { // coverage: unreachable, writing into a bytes.Buffer
		return "", err
	}
	if err := w.Close(); err != nil { // coverage: unreachable, flushing into a bytes.Buffer
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
