package controller

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/obleey/omegacd/internal/blob"
	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/secrets"
)

// Q208–Q210: a decrypted file reads as its key names, KEY=••••, from its encrypted source in Git; no value, nor its
// ciphertext, is ever in the view. A binary or an unreadable source says so; a file not from SOPS shows no keys.
func TestSecretDetail(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	logs := logTo(r)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	token, pw := rand.Text(), rand.Text()
	enc := func(name, plain string) []byte {
		b, err := secrets.Encrypt([]byte(plain), name, []string{id.Recipient().String()})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	files := map[string][]byte{
		"app.sops.env":    enc("app.sops.env", "# the API\nAPI_TOKEN="+token+"\nMODE=prod\n"),
		"conf.sops.yaml":  enc("conf.sops.yaml", "db:\n  pin: "+pw+"\n  hosts: [a, b]\n"),
		"cert.sops.pem":   enc("cert.sops.pem", "-----BEGIN CERTIFICATE-----\n"),
		"broken.sops.env": []byte("not a SOPS file\n"),
	}
	var rendered domain.Rendered
	r.g.mu.Lock()
	r.g.blobs = map[string][]byte{}
	for name, b := range files {
		h := blob.Hash(b)
		r.g.blobs[h] = b
		rendered.Files = append(rendered.Files, domain.File{Path: name, SHA256: h})
	}
	r.g.mu.Unlock()
	a := domain.Application{Name: "web"}
	for name, want := range map[string]string{
		"app.env":    "API_TOKEN=••••\nMODE=••••\n",
		"conf.yaml":  "db.pin: ••••\ndb.hosts.0: ••••\ndb.hosts.1: ••••\n",
		"cert.pem":   "",
		"broken.env": "",
		"made.txt":   "",
	} {
		d, err := r.c.secretDetail(ctx, a, domain.ResourceDetail{Resource: domain.Resource{Name: name, Secret: true}}, rendered)
		if err != nil || d.Content != want || d.Masked != (want != "") || !strings.Contains(d.Info["secret"], "decrypted only on the Host") {
			t.Errorf("%s: %q %v %v", name, d.Content, d.Info, err)
		}
		for _, f := range files {
			if strings.Contains(d.Content+strings.Join(mapValues(d.Info), " "), token) || strings.Contains(d.Content, pw) ||
				strings.Contains(d.Content, "ENC[") || len(f) > 0 && strings.Contains(d.Content, string(f)) {
				t.Fatalf("%s: a value or its ciphertext shown: %q", name, d.Content)
			}
		}
		switch name {
		case "cert.pem":
			if d.Info["keys"] != "none: an encrypted binary file" {
				t.Errorf("binary: %v", d.Info)
			}
		case "broken.env":
			if !strings.Contains(d.Info["keys"], "can't be read from broken.sops.env") {
				t.Errorf("unreadable: %v", d.Info)
			}
		}
	}
	if !strings.Contains(logs.String(), `level=WARN msg="the key names of a secret file not shown" app=web file=broken.sops.env`) {
		t.Errorf("logs:\n%s", logs.String())
	}
	r.g.mu.Lock()
	r.g.blobErr = errors.New("source server gone")
	r.g.mu.Unlock()
	if _, err := r.c.secretDetail(ctx, a, domain.ResourceDetail{Resource: domain.Resource{Name: "app.env", Secret: true}}, rendered); err == nil {
		t.Error("a blob it can't fetch")
	}
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
