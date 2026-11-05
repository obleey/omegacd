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

	"github.com/getsops/sops/v3/aes"
	sopsage "github.com/getsops/sops/v3/age"
	"github.com/getsops/sops/v3/cmd/sops/common"
	"github.com/getsops/sops/v3/cmd/sops/formats"
	"github.com/getsops/sops/v3/config"
	"github.com/getsops/sops/v3/keyservice"
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
		return nil
	})
	return written, err
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
	return nil, errors.New("encryption is not supported")
}

// writeInPlace truncates and rewrites so the inode survives for existing bind mounts.
func writeInPlace(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
