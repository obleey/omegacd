package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"filippo.io/age"

	"github.com/obleey/omegacd/internal/crypto"
	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/secrets"
)

// SecretSource decrypts an Application's Encrypted Secrets in its folder (Q29). SOPS with age is the one v0.2 ships;
// others (1Password, Vault, …) plug in here, their credentials in the Agent's configuration, never in Git.
type SecretSource interface {
	// Decrypt writes the plaintext of every Encrypted Secret it handles in dir, into dir and nowhere else (Q19),
	// and returns what it wrote.
	Decrypt(dir string) ([]Decrypted, error)
}

// Decrypted is a file a SecretSource wrote: its path relative to the app folder. Nothing of its content is read again:
// the UI shows a file's key names from its encrypted source in Git (Q208, Q218).
type Decrypted struct {
	Path string
}

// Secrets decrypts with every source, then gives the files the mode Q58 asks for.
type Secrets struct {
	sources []SecretSource
	group   int // -1: none, files are 0600; else 0640 with this group
}

func NewSecrets(group int, sources ...SecretSource) *Secrets {
	return &Secrets{sources: sources, group: group}
}

func (s *Secrets) Decrypt(dir string) ([]Decrypted, error) {
	var all []Decrypted
	for _, src := range s.sources {
		ds, err := src.Decrypt(dir)
		all = append(all, ds...)
		if err != nil {
			return all, err
		}
	}
	for _, d := range all {
		p := filepath.Join(dir, filepath.FromSlash(d.Path))
		mode := fs.FileMode(0o600)
		if s.group >= 0 {
			if err := os.Lchown(p, -1, s.group); err != nil {
				return all, fmt.Errorf("give %s to group %d (OMEGACD_AGENT_SECRET_GROUP; the Agent's user must be in it): %w", d.Path, s.group, err)
			}
			mode = 0o640
		}
		if err := os.Chmod(p, mode); err != nil {
			return all, err
		}
	}
	return all, nil
}

// SOPS decrypts *.sops.* files with the Host Key (Q4): "x.sops.env" becomes "x.env" next to it.
type SOPS struct{ HostKey *age.X25519Identity }

func (s SOPS) Decrypt(dir string) ([]Decrypted, error) {
	written, err := secrets.Decryptor{}.DecryptDir(dir, []string{s.HostKey.String()})
	var out []Decrypted
	for _, p := range written {
		rel, _ := filepath.Rel(dir, p)
		out = append(out, Decrypted{Path: filepath.ToSlash(rel)})
	}
	if wk := (*domain.WrongKeyError)(nil); errors.As(err, &wk) {
		wk.HostKey = s.HostKey.Recipient().String() // its message names this Host's key in full, with the fix (Q215)
	}
	return out, err
}

// HostKey loads the Agent's age Host Key from stateDir, creating it at the first start (Q4). It never leaves the
// Host: losing the state dir means re-encrypting this Host's secrets for a new key.
func HostKey(stateDir string) (*age.X25519Identity, error) {
	path := filepath.Join(stateDir, "host.key")
	id, err := crypto.LoadIdentity(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return id, err
	}
	if id, err = age.GenerateX25519Identity(); err != nil { // coverage: unreachable, generating reads the system's randomness, which doesn't fail
		return nil, err
	}
	key := fmt.Sprintf("# created: %s\n# public key: %s\n%s\n", time.Now().Format(time.RFC3339), id.Recipient(), id)
	return id, writeNew(path, []byte(key))
}

func writeNew(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	return errors.Join(err, f.Close())
}
