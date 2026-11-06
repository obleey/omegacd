package source

import (
	"cmp"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

var ErrSignature = errors.New("invalid webhook signature")

type hookRepo struct {
	HTMLURL    string `json:"html_url"`
	CloneURL   string `json:"clone_url"`
	SSHURL     string `json:"ssh_url"`
	GitURL     string `json:"git_url"`
	URL        string `json:"url"`
	WebURL     string `json:"web_url"`
	GitHTTPURL string `json:"git_http_url"`
	GitSSHURL  string `json:"git_ssh_url"`
	Homepage   string `json:"homepage"`
}

// ParseWebhook verifies a push webhook from provider ("github", "gitlab", "gitea") and returns every
// clone URL of the pushed Repository. Non-push events return nil, nil. An empty secret skips verification.
func ParseWebhook(provider string, header http.Header, body []byte, secret string) ([]string, error) {
	switch provider {
	case "github":
		if secret != "" && !validHMAC(body, secret, strings.TrimPrefix(header.Get("X-Hub-Signature-256"), "sha256=")) {
			return nil, ErrSignature
		}
		if header.Get("X-GitHub-Event") != "push" {
			return nil, nil
		}
	case "gitlab":
		if secret != "" && subtle.ConstantTimeCompare([]byte(header.Get("X-Gitlab-Token")), []byte(secret)) != 1 {
			return nil, ErrSignature
		}
		if ev := header.Get("X-Gitlab-Event"); ev != "Push Hook" && ev != "Tag Push Hook" {
			return nil, nil
		}
	case "gitea":
		sig := cmp.Or(header.Get("X-Gitea-Signature"), header.Get("X-Forgejo-Signature"))
		if secret != "" && !validHMAC(body, secret, sig) {
			return nil, ErrSignature
		}
		if ev := cmp.Or(header.Get("X-Gitea-Event"), header.Get("X-Forgejo-Event")); ev != "" && ev != "push" {
			return nil, nil
		}
	default:
		return nil, fmt.Errorf("unknown webhook provider %q", provider)
	}
	var p struct {
		Repository hookRepo `json:"repository"`
		Project    hookRepo `json:"project"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("parse %s webhook: %w", provider, err)
	}
	var urls []string
	for _, r := range []hookRepo{p.Repository, p.Project} {
		for _, u := range []string{r.CloneURL, r.SSHURL, r.GitURL, r.HTMLURL, r.URL, r.WebURL, r.GitHTTPURL, r.GitSSHURL, r.Homepage} {
			if u != "" && !slices.Contains(urls, u) {
				urls = append(urls, u)
			}
		}
	}
	return urls, nil
}

func validHMAC(body []byte, secret, sigHex string) bool {
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hmac.Equal(sig, m.Sum(nil))
}
