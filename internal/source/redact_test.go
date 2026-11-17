package source

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/logging"
)

// Q62: a token in a repository URL never reaches an error or a log line.
func TestURLCredentialsStayOutOfErrorsAndLogs(t *testing.T) {
	g := New(t.TempDir(), func(context.Context) ([]domain.RepoCredential, error) { return nil, nil })
	var log logging.Buffer
	g.Log, _ = logging.New(&log, "debug", "text")
	for _, u := range []string{"http://me:s3cret-pw@127.0.0.1:1/r.git", "http://ghp_t0ken@127.0.0.1:1/r.git"} {
		_, err := g.Resolve(context.Background(), u, "main")
		if err == nil {
			t.Fatal("resolved an unreachable repository")
		}
		for _, s := range []string{err.Error(), log.String()} {
			if strings.Contains(s, "s3cret-pw") || strings.Contains(s, "ghp_t0ken") {
				t.Errorf("credential in %q", s)
			}
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			t.Errorf("%v: the cause is lost with the credential", err)
		}
	}
	if a, err := g.CheckAccess(context.Background(), "http://me:s3cret-pw@127.0.0.1:1/r.git"); err != nil || a.Access != "error" ||
		strings.Contains(a.Message, "s3cret-pw") {
		t.Errorf("the access check: %+v, %v", a, err)
	}
	if got := redact("https://me:pw@git.example.com/r.git"); got != "https://***@git.example.com/r.git" {
		t.Errorf("redact = %q", got)
	}
}
