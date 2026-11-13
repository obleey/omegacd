package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Certificates that can't be written or read are the filesystem's error, naming the file: a folder under a file, a
// file already there as a dangling link (never written through), a missing or empty CA, a missing key.
func TestCertificateFailures(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o600)
	if err := GenerateCerts(filepath.Join(file, "certs"), nil); err == nil {
		t.Error("certificates under a file")
	}
	for _, name := range []string{caFile, serverCert, serverKey} {
		dir := t.TempDir()
		os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, name))
		if err := GenerateCerts(dir, nil); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s a dangling link: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "nowhere")); err == nil {
			t.Errorf("%s: written through the link", name)
		}
	}

	if _, err := ServerTLS(t.TempDir()); err == nil {
		t.Error("server TLS without a CA")
	}
	if _, err := ClientTLS(t.TempDir(), "x"); err == nil {
		t.Error("client TLS without a CA")
	}
	empty := t.TempDir()
	os.WriteFile(filepath.Join(empty, caFile), []byte("not PEM"), 0o644)
	if _, err := ServerTLS(empty); err == nil || !strings.Contains(err.Error(), "ca.pem: no certificate") {
		t.Errorf("a CA file without a certificate: %v", err)
	}
	for _, key := range []string{serverKey, clientKey} {
		dir := t.TempDir()
		if err := GenerateCerts(dir, []string{"localhost"}); err != nil {
			t.Fatal(err)
		}
		os.Remove(filepath.Join(dir, key))
		if _, err := ServerTLS(dir); (err != nil) != (key == serverKey) {
			t.Errorf("ServerTLS without %s: %v", key, err)
		}
		if _, err := ClientTLS(dir, "localhost"); (err != nil) != (key == clientKey) {
			t.Errorf("ClientTLS without %s: %v", key, err)
		}
	}
}
