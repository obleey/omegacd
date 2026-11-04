// Command dockercd is GitOps continuous delivery for Docker Compose.
package main

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/obleey/dockercd/internal/api"
	"github.com/obleey/dockercd/internal/cli"
	"github.com/obleey/dockercd/internal/controller"
	"github.com/obleey/dockercd/internal/crypto"
	"github.com/obleey/dockercd/internal/engine"
	"github.com/obleey/dockercd/internal/gitsrc"
	"github.com/obleey/dockercd/internal/secrets"
	"github.com/obleey/dockercd/internal/store"
	"github.com/obleey/dockercd/web"
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
		addr       = cmp.Or(os.Getenv("DOCKERCD_ADDR"), ":8080")
		dataDir    = cmp.Or(os.Getenv("DOCKERCD_DATA"), "/data")
		workDir    = cmp.Or(os.Getenv("DOCKERCD_WORKDIR"), "/run/dockercd")
		ageKeyFile = cmp.Or(os.Getenv("DOCKERCD_AGE_KEY_FILE"), "/etc/dockercd/age.key")
	)
	poll, err := time.ParseDuration(cmp.Or(os.Getenv("DOCKERCD_POLL_INTERVAL"), "3m"))
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
	st, err := store.Open(filepath.Join(dataDir, "dockercd.db"), crypto.NewSealer(id))
	if err != nil {
		return err
	}
	defer st.Close()
	sec, err := secrets.New(ageKeyFile)
	if err != nil {
		return err
	}
	knownHosts := cmp.Or(os.Getenv("DOCKERCD_SSH_KNOWN_HOSTS"), "/etc/dockercd/known_hosts")
	eng, err := engine.New(engine.Config{
		Hosts:          st.GetHost,
		ListHosts:      st.ListHosts,
		KnownHostsFile: knownHosts,
		KeyDir:         filepath.Join(dataDir, "keys"),
	})
	if err != nil {
		return err
	}
	defer eng.Close()
	git := gitsrc.New(filepath.Join(dataDir, "repos"), st.ListRepoCredentials)
	git.KnownHostsFile = knownHosts

	selfID, _ := os.Hostname() // inside a container this is the container ID prefix
	ctl := controller.New(controller.Config{
		Store: st, Git: git, Secrets: sec, Engine: eng,
		WorkDir: workDir, PollInterval: poll,
		NormalizeURL: gitsrc.NormalizeURL,
		ObserveSync:  api.ObserveSync,
		SelfID:       selfID,
		Log:          log,
	})

	if err := api.EnsureAdmin(ctx, st, os.Getenv("DOCKERCD_ADMIN_PASSWORD"), log); err != nil {
		return err
	}
	proxies, err := api.ParseTrustedProxies(os.Getenv("DOCKERCD_TRUSTED_PROXIES"))
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
			WebhookSecret:  os.Getenv("DOCKERCD_WEBHOOK_SECRET"),
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
	log.Info("dockercd listening", "addr", addr, "data", dataDir, "workdir", workDir, "poll", poll)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
