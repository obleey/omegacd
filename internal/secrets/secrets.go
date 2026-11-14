// Package secrets decrypts SOPS Encrypted Secrets with the age keys of the Application's Host.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/getsops/sops/v3"
	"github.com/getsops/sops/v3/aes"
	sopsage "github.com/getsops/sops/v3/age"
	"github.com/getsops/sops/v3/cmd/sops/common"
	"github.com/getsops/sops/v3/cmd/sops/formats"
	"github.com/getsops/sops/v3/config"
	"github.com/getsops/sops/v3/keyservice"
	"github.com/getsops/sops/v3/version"
	"google.golang.org/grpc"

	"github.com/obleey/omegacd/internal/domain"
)

// Decryptor decrypts with the identities passed to each call. It never reads SOPS_AGE_KEY* from the
// environment: the data key is unwrapped by ageKeys, a key service client holding just those identities,
// so concurrent calls with different keys cannot see each other's keys and no process state is touched.
type Decryptor struct{}

var _ domain.Secrets = Decryptor{}

func formatFor(name string) formats.Format {
	switch strings.TrimPrefix(filepath.Ext(name), ".") {
	case "env", "dotenv":
		return formats.Dotenv
	case "yaml", "yml":
		return formats.Yaml
	case "json":
		return formats.Json
	case "ini":
		return formats.Ini
	}
	return formats.Binary
}

func (Decryptor) DecryptDir(dir string, identities []string) ([]string, error) {
	var ids sopsage.ParsedIdentities
	if err := ids.Import(identities...); err != nil {
		return nil, err
	}
	var written []string
	err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !e.Type().IsRegular() || !strings.Contains(e.Name(), ".sops.") {
			return nil
		}
		out := filepath.Join(filepath.Dir(path), strings.Replace(e.Name(), ".sops.", ".", 1))
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		plain, err := decryptData(data, formatFor(out), ids, rel)
		if err != nil {
			return err
		}
		if err := writeInPlace(out, plain); err != nil {
			return err
		}
		written = append(written, out)
		// the containers read the plaintext; the encrypted copy stays in Git, where it belongs
		return os.Remove(path)
	})
	return written, err
}

// Encrypt encrypts plain for the age recipients as a SOPS file named name ("x.sops.env"): the format comes
// from the decrypted name's extension, as DecryptDir reads it back. Used by omegacd import; the result is a
// regular SOPS file the sops CLI opens too.
func Encrypt(plain []byte, name string, recipients []string) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, errors.New("encrypt: no age recipient")
	}
	keys, err := sopsage.MasterKeysFromRecipients(strings.Join(recipients, ","))
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	group := make(sops.KeyGroup, len(keys))
	for i, k := range keys {
		group[i] = k
	}
	store := common.StoreForFormat(formatFor(strings.Replace(name, ".sops.", ".", 1)), config.NewStoresConfig())
	branches, err := store.LoadPlainFile(plain)
	if err != nil {
		return nil, fmt.Errorf("encrypt %s: %w", name, err)
	}
	tree := sops.Tree{Branches: branches, Metadata: sops.Metadata{KeyGroups: []sops.KeyGroup{group}, Version: version.Version}}
	dataKey, errs := tree.GenerateDataKeyWithKeyServices([]keyservice.KeyServiceClient{keyservice.NewLocalClient()})
	if len(errs) > 0 { // coverage: unreachable, age recipients just parsed always wrap a key
		return nil, fmt.Errorf("encrypt %s: %v", name, errors.Join(errs...))
	}
	if err := common.EncryptTree(common.EncryptTreeOpts{DataKey: dataKey, Tree: &tree, Cipher: aes.NewCipher()}); err != nil { // coverage: unreachable, a tree just loaded and a new key
		return nil, fmt.Errorf("encrypt %s: %w", name, err)
	}
	return store.EmitEncryptedFile(tree)
}

// decryptData is decrypt.DataWithFormat with our key service instead of the environment-reading local one.
func decryptData(data []byte, format formats.Format, ids sopsage.ParsedIdentities, name string) ([]byte, error) {
	store := common.StoreForFormat(format, config.NewStoresConfig())
	tree, err := store.LoadEncryptedFile(data)
	if err != nil {
		return nil, fmt.Errorf("decrypt %s: %w", name, err)
	}
	key, err := tree.Metadata.GetDataKeyWithKeyServices([]keyservice.KeyServiceClient{ageKeys(ids)}, nil)
	if err != nil {
		wk := &domain.WrongKeyError{File: name}
		for _, g := range tree.Metadata.KeyGroups {
			for _, k := range g {
				wk.Recipients = append(wk.Recipients, k.ToString())
			}
		}
		return nil, wk
	}
	cipher := aes.NewCipher()
	mac, err := tree.Decrypt(key, cipher)
	if err != nil {
		return nil, fmt.Errorf("decrypt %s: %w", name, err)
	}
	original, err := cipher.Decrypt(tree.Metadata.MessageAuthenticationCode, key, tree.Metadata.LastModified.Format(time.RFC3339))
	if err != nil || original != mac {
		return nil, fmt.Errorf("decrypt %s: integrity check failed (MAC mismatch)", name)
	}
	return store.EmitPlainFile(tree.Branches)
}

// ageKeys unwraps age-encrypted SOPS data keys with exactly these identities.
type ageKeys sopsage.ParsedIdentities

func (k ageKeys) Decrypt(_ context.Context, req *keyservice.DecryptRequest, _ ...grpc.CallOption) (*keyservice.DecryptResponse, error) {
	ak := req.Key.GetAgeKey()
	if ak == nil || len(k) == 0 { // with no identities, MasterKey.Decrypt would fall back to the environment
		return nil, errors.New("no matching age identity")
	}
	mk := &sopsage.MasterKey{Recipient: ak.Recipient, EncryptedKey: string(req.Ciphertext)}
	sopsage.ParsedIdentities(k).ApplyToMasterKey(mk)
	plain, err := mk.Decrypt()
	if err != nil {
		return nil, err
	}
	return &keyservice.DecryptResponse{Plaintext: plain}, nil
}

func (ageKeys) Encrypt(context.Context, *keyservice.EncryptRequest, ...grpc.CallOption) (*keyservice.EncryptResponse, error) {
	// coverage: unreachable, decrypting never calls it
	return nil, errors.New("encryption is not supported")
}

// writeInPlace truncates and rewrites so the inode survives for existing bind mounts.
func writeInPlace(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil { // coverage: unreachable, a file just opened for writing
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil { // coverage: unreachable, a file just opened for writing; only a full disk fails
		f.Close()
		return err
	}
	return f.Close()
}
