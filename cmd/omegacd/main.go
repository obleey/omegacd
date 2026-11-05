// Command omegacd is GitOps continuous delivery for Docker Compose.
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/obleey/omegacd/internal/api"
	"github.com/obleey/omegacd/internal/cli"
	"github.com/obleey/omegacd/internal/controller"
	"github.com/obleey/omegacd/internal/crypto"
	"github.com/obleey/omegacd/internal/engine"
	"github.com/obleey/omegacd/internal/gitsrc"
	"github.com/obleey/omegacd/internal/secrets"
	"github.com/obleey/omegacd/internal/store"
	"github.com/obleey/omegacd/web"
)

func main() {
	// no args or "server" runs the server; anything else is a CLI command
	if len(os.Args) > 1 && os.Args[1] != "server" {
		os.Exit(cli.Main(os.Args[1:]))
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	var (
		addr       = cmp.Or(os.Getenv("OMEGACD_ADDR"), ":8080")
		dataDir    = cmp.Or(os.Getenv("OMEGACD_DATA"), "/data")
		workDir    = cmp.Or(os.Getenv("OMEGACD_WORKDIR"), "/run/omegacd")
		ageKeyFile = cmp.Or(os.Getenv("OMEGACD_AGE_KEY_FILE"), "/etc/omegacd/age.key")
	)
	poll, err := pollInterval(os.Getenv("OMEGACD_POLL_INTERVAL"))
	if err != nil {
		return err
	}
	for _, d := range []string{dataDir, workDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	id, err := crypto.LoadIdentity(ageKeyFile)
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(dataDir, "omegacd.db"), crypto.NewSealer(id))
	if err != nil {
		return err
	}
	defer st.Close()
	knownHosts := cmp.Or(os.Getenv("OMEGACD_SSH_KNOWN_HOSTS"), "/etc/omegacd/known_hosts")
	noLocal, err := localHostDisabled(os.Getenv("OMEGACD_LOCAL_HOST"))
	if err != nil {
		return err
	}
	eng, err := engine.New(engine.Config{
		Hosts:          st.GetHost,
		ListHosts:      st.ListHosts,
		KnownHostsFile: knownHosts,
		KeyDir:         filepath.Join(dataDir, "keys"),
		NoLocalHost:    noLocal,
	})
	if err != nil {
		return err
	}
	defer eng.Close()
	git := gitsrc.New(filepath.Join(dataDir, "repos"), st.ListRepoCredentials)
	git.KnownHostsFile = knownHosts

	selfID, _ := os.Hostname() // inside a container this is the container ID prefix
	ctl := controller.New(controller.Config{
		Store: st, Git: git, Secrets: secrets.Decryptor{}, MasterKey: id, Engine: eng,
		WorkDir: workDir, PollInterval: poll, NoLocalHost: noLocal,
		NormalizeURL: gitsrc.NormalizeURL,
		ObserveSync:  api.ObserveSync,
		SelfID:       selfID,
		Log:          log,
	})

	if err := api.EnsureAdmin(ctx, st, os.Getenv("OMEGACD_ADMIN_PASSWORD"), log); err != nil {
		return err
	}
	proxies, err := api.ParseTrustedProxies(os.Getenv("OMEGACD_TRUSTED_PROXIES"))
	if err != nil {
		return err
	}
	ui, err := fs.Sub(web.Build, "build")
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr: addr,
		Handler: api.New(api.Config{
			Controller:     ctl,
			Store:          st,
			WebhookSecret:  os.Getenv("OMEGACD_WEBHOOK_SECRET"),
			WebhookParse:   gitsrc.ParseWebhook,
			TrustedProxies: proxies,
			UI:             ui,
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go ctl.Run(ctx)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Info("omegacd listening", "addr", addr, "data", dataDir, "workdir", workDir, "poll", poll)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// localHostDisabled reads OMEGACD_LOCAL_HOST: "false" where no Docker engine runs next to OmegaCD (Kubernetes).
func localHostDisabled(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "true":
		return false, nil
	case "false":
		return true, nil
	}
	return false, fmt.Errorf("OMEGACD_LOCAL_HOST must be true or false, not %q", v)
}

// pollInterval reads OMEGACD_POLL_INTERVAL (default 3m); 0 turns polling off, leaving webhooks and Refresh.
func pollInterval(v string) (time.Duration, error) {
	d, err := time.ParseDuration(cmp.Or(strings.TrimSpace(v), "3m"))
	if err != nil || d < 0 {
		return 0, fmt.Errorf("OMEGACD_POLL_INTERVAL must be a duration like 3m, or 0 to turn polling off, not %q", v)
	}
	return d, nil
}
