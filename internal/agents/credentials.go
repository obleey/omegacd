package agents

import (
	"bytes"
	"encoding/json"
	"fmt"

	"filippo.io/age"

	"github.com/obleey/omegacd/internal/domain"
)

// sealed is a registry credential as it travels to an Agent, inside its age encryption.
type sealed struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// SealCredential encrypts c for the Host whose Host Key is recipient (Q86): ciphertext on the wire whatever the
// transport; only that Agent can open it.
func SealCredential(recipient string, c domain.RegistryCredential) ([]byte, error) {
	r, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return nil, fmt.Errorf("host key %q: %w", recipient, err)
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, r)
	if err != nil {
		return nil, err
	}
	if err := json.NewEncoder(w).Encode(sealed{c.Username, c.Password}); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// OpenCredential decrypts a sealed credential for registry with the Host Key, in memory.
func OpenCredential(hostKey *age.X25519Identity, registry string, b []byte) (domain.RegistryCredential, error) {
	r, err := age.Decrypt(bytes.NewReader(b), hostKey)
	if err != nil {
		return domain.RegistryCredential{}, fmt.Errorf("registry %s: the credentials don't decrypt with this Host's key: %w", registry, err)
	}
	var s sealed
	if err := json.NewDecoder(r).Decode(&s); err != nil {
		return domain.RegistryCredential{}, fmt.Errorf("registry %s: %w", registry, err)
	}
	return domain.RegistryCredential{Registry: registry, Username: s.Username, Password: s.Password}, nil
}
