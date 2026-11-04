// Package secrets decrypts SOPS Encrypted Secrets with the omegacd age key.
package secrets

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/getsops/sops/v3/cmd/sops/formats"
	"github.com/getsops/sops/v3/decrypt"

	"github.com/obleey/omegacd/internal/domain"
)

type Decryptor struct{}

var _ domain.Secrets = (*Decryptor)(nil)

// New points SOPS at the age key via SOPS_AGE_KEY_FILE: the sops library only reads keys from the
// process environment, so there is one key per process.
func New(ageKeyFile string) (*Decryptor, error) {
	if _, err := os.Stat(ageKeyFile); err != nil {
		return nil, fmt.Errorf("age key file: %w", err)
	}
	if err := os.Setenv("SOPS_AGE_KEY_FILE", ageKeyFile); err != nil {
		return nil, err
	}
	return &Decryptor{}, nil
}

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

func (d *Decryptor) DecryptDir(dir string) ([]string, error) {
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
		plain, err := decrypt.DataWithFormat(data, formatFor(out))
		if err != nil {
			return fmt.Errorf("decrypt %s: %w", path, err)
		}
		if err := writeInPlace(out, plain); err != nil {
			return err
		}
		written = append(written, out)
		return nil
	})
	return written, err
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
