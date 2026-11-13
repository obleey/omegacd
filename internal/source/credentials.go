package source

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"filippo.io/age"

	"github.com/obleey/omegacd/internal/crypto"
	"github.com/obleey/omegacd/internal/domain"
)

// creds holds the Repository credentials (Q13, Q40): one file, encrypted with the Source Server's own age key
// (born at its first start, never leaving its data dir), rewritten whole on every change.
type creds struct {
	mu   sync.Mutex
	id   *age.X25519Identity
	file string
	list []stored
}

// stored is a RepoCredential with its secrets, as on disk.
type stored struct {
	ID            int64
	URLPrefix     string
	Username      string
	Password      string
	SSHPrivateKey string
}

func openCreds(dir string) (*creds, error) {
	keyFile := filepath.Join(dir, "source.key")
	id, _, err := crypto.LoadOrCreateIdentity(keyFile)
	if err != nil {
		return nil, fmt.Errorf("source server key: %w", err)
	}
	c := &creds{id: id, file: filepath.Join(dir, "credentials.age")}
	b, err := os.ReadFile(c.file)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	r, err := age.Decrypt(bytes.NewReader(b), id)
	if err == nil {
		err = json.NewDecoder(r).Decode(&c.list)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s (is %s its key?): %w", c.file, keyFile, err)
	}
	return c, nil
}

// save writes list encrypted next to the file, then renames it over: a crash keeps the old file whole.
// Caller holds c.mu.
func (c *creds) save(list []stored) error {
	tmp, err := os.CreateTemp(filepath.Dir(c.file), ".credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	w, err := age.Encrypt(tmp, c.id.Recipient())
	if err == nil {
		err = json.NewEncoder(w).Encode(list)
	}
	if err == nil {
		err = w.Close()
	}
	if err == nil {
		err = tmp.Sync()
	}
	if err = errors.Join(err, tmp.Close()); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), c.file); err != nil {
		return err
	}
	c.list = list
	return nil
}

// all is every credential with its secrets, for Git.
func (c *creds) all(context.Context) ([]domain.RepoCredential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]domain.RepoCredential, len(c.list))
	for i, s := range c.list {
		out[i] = domain.RepoCredential{ID: s.ID, URLPrefix: s.URLPrefix, Username: s.Username, Password: s.Password,
			SSHPrivateKey: s.SSHPrivateKey, HasPassword: s.Password != "", HasSSHKey: s.SSHPrivateKey != ""}
	}
	return out, nil
}

// ListCredentials returns the credentials without their secrets: they never leave the Source Server.
func (s *Server) ListCredentials(ctx context.Context) ([]domain.RepoCredential, error) {
	list, _ := s.creds.all(ctx)
	for i := range list {
		list[i].Password, list[i].SSHPrivateKey = "", ""
	}
	return list, nil
}

// SaveCredential creates (ID 0) or updates a credential; empty secrets keep the old ones. It returns it without
// secrets.
func (s *Server) SaveCredential(ctx context.Context, c domain.RepoCredential) (domain.RepoCredential, error) {
	if strings.TrimSpace(c.URLPrefix) == "" {
		return domain.RepoCredential{}, fmt.Errorf("%w: urlPrefix is required", domain.ErrInvalid)
	}
	s.creds.mu.Lock()
	list := slices.Clone(s.creds.list)
	i := slices.IndexFunc(list, func(x stored) bool { return x.ID == c.ID })
	if c.ID != 0 && i < 0 {
		s.creds.mu.Unlock()
		return domain.RepoCredential{}, domain.ErrNotFound
	}
	if slices.ContainsFunc(list, func(x stored) bool { return x.URLPrefix == c.URLPrefix && x.ID != c.ID }) {
		s.creds.mu.Unlock()
		return domain.RepoCredential{}, fmt.Errorf("%w: a credential for %s", domain.ErrAlreadyExists, c.URLPrefix)
	}
	n := stored{ID: c.ID, URLPrefix: c.URLPrefix, Username: c.Username, Password: c.Password, SSHPrivateKey: c.SSHPrivateKey}
	if i < 0 {
		for _, x := range list {
			n.ID = max(n.ID, x.ID)
		}
		n.ID++
		list = append(list, n)
	} else {
		n.Password = cmp.Or(n.Password, list[i].Password)
		n.SSHPrivateKey = cmp.Or(n.SSHPrivateKey, list[i].SSHPrivateKey)
		list[i] = n
	}
	err := s.creds.save(list)
	s.creds.mu.Unlock()
	if err != nil {
		return domain.RepoCredential{}, err
	}
	return domain.RepoCredential{ID: n.ID, URLPrefix: n.URLPrefix, Username: n.Username,
		HasPassword: n.Password != "", HasSSHKey: n.SSHPrivateKey != ""}, nil
}

func (s *Server) DeleteCredential(_ context.Context, id int64) error {
	s.creds.mu.Lock()
	defer s.creds.mu.Unlock()
	i := slices.IndexFunc(s.creds.list, func(x stored) bool { return x.ID == id })
	if i < 0 {
		return domain.ErrNotFound
	}
	return s.creds.save(slices.Delete(slices.Clone(s.creds.list), i, i+1))
}
