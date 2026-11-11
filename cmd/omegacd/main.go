// Command omegacd is GitOps continuous delivery for Docker Compose.
package main

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/obleey/omegacd/internal/agent"
	"github.com/obleey/omegacd/internal/agents"
	"github.com/obleey/omegacd/internal/api"
	"github.com/obleey/omegacd/internal/cli"
	"github.com/obleey/omegacd/internal/controller"
	"github.com/obleey/omegacd/internal/crypto"
	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/engine"
	"github.com/obleey/omegacd/internal/logging"
	"github.com/obleey/omegacd/internal/source"
	"github.com/obleey/omegacd/internal/store"
	"github.com/obleey/omegacd/web"
)

func main() {
	// no args or "server" runs the server, "source-server" the standalone Source Server, "agent" the Agent;
	// anything else is a CLI command
	run := run
	switch {
	case len(os.Args) > 1 && os.Args[1] == "source-server":
		run = runSourceServer
	case len(os.Args) > 1 && os.Args[1] == "agent":
		if len(os.Args) > 2 && slices.Contains([]string{"-h", "--help", "help"}, os.Args[2]) {
			agent.Usage(os.Stdout)
			return
		}
		run = runAgent
	case len(os.Args) > 1 && os.Args[1] != "server":
		os.Exit(cli.Main(os.Args[1:]))
	}
	log, err := logging.New(os.Stderr, os.Getenv("OMEGACD_LOG_LEVEL"), os.Getenv("OMEGACD_LOG_FORMAT"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	var (
		addr       = cmp.Or(os.Getenv("OMEGACD_ADDR"), ":8080")
		dataDir    = cmp.Or(os.Getenv("OMEGACD_DATA"), "/data")
		ageKeyFile = cmp.Or(os.Getenv("OMEGACD_AGE_KEY_FILE"), "/etc/omegacd/age.key")
	)
	poll, err := pollInterval(os.Getenv("OMEGACD_POLL_INTERVAL"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return err
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
	knownHosts := cmp.Or(os.Getenv("OMEGACD_SSH_KNOWN_HOSTS"), "/etc/omegacd/known_hosts") // Git over SSH (Q106)
	var src domain.Source
	if u := os.Getenv("OMEGACD_SOURCE_SERVER"); u != "" {
		c, err := source.Dial(u, sourceTLSDir())
		if err != nil {
			return err
		}
		defer c.Close()
		src = c
		log.Info("using the standalone source server", "url", u)
	} else {
		s, err := source.NewServer(dataDir)
		if err != nil {
			return err
		}
		s.KnownHostsFile = knownHosts
		s.Log = log.With("component", "source")
		go s.CollectBlobs(ctx)
		src = s
	}

	if err := moveRepoCredentials(ctx, st, src); err != nil {
		return err
	}

	hub := agents.New(st, log.With("component", "agents"))
	hub.Blob = src.Blob // the files as rendered, secrets still encrypted: only the Agent decrypts (Q19)
	ctl := controller.New(controller.Config{
		Agents: hub, Store: st, Source: src, PollInterval: poll,
		NormalizeURL: source.NormalizeURL,
		ObserveSync:  api.ObserveSync,
		Log:          log.With("component", "controller"),
	})
	hub.Reported, hub.Resynced, hub.Removed, hub.Progress, hub.Status = ctl.AgentReported, ctl.Resynced, ctl.AgentRemoved,
		ctl.AgentProgress, ctl.AgentStatusChanged

	if err := api.EnsureAdmin(ctx, st, os.Getenv("OMEGACD_ADMIN_PASSWORD"), log.With("component", "api")); err != nil {
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
			Agents:         hub,
			Source:         src,
			WebhookSecret:  os.Getenv("OMEGACD_WEBHOOK_SECRET"),
			WebhookParse:   source.ParseWebhook,
			TrustedProxies: proxies,
			UI:             ui,
			Log:            log.With("component", "api"),
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
	log.Info("omegacd listening", "addr", addr, "data", dataDir, "poll", poll)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// moveRepoCredentials hands Repository credentials an older version stored in the Control Plane's database to the
// Source Server, which owns them now (Q40), and deletes them here: one at a time, each deleted only once saved.
func moveRepoCredentials(ctx context.Context, st *store.Store, src domain.Source) error {
	cs, err := st.ListRepoCredentials(ctx)
	if err != nil {
		return err
	}
	for _, c := range cs {
		id := c.ID
		c.ID = 0
		// already there: a move that stopped before deleting here; the Source Server's copy stays
		if _, err := src.SaveCredential(ctx, c); err != nil && !errors.Is(err, domain.ErrAlreadyExists) {
			return fmt.Errorf("move repository credential %s to the source server: %w", c.URLPrefix, err)
		}
		if err := st.DeleteRepoCredential(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func sourceTLSDir() string {
	return cmp.Or(os.Getenv("OMEGACD_SOURCE_TLS_DIR"), "/etc/omegacd/source-tls")
}

// runSourceServer serves the Source Server over gRPC with mutual TLS: only the Control Plane, holding the client
// certificate from omegacd source-certs, gets in (Q15, Q45).
func runSourceServer(log *slog.Logger) error {
	var (
		addr    = cmp.Or(os.Getenv("OMEGACD_SOURCE_ADDR"), ":9090")
		dataDir = cmp.Or(os.Getenv("OMEGACD_DATA"), "/data")
	)
	cfg, err := source.ServerTLS(sourceTLSDir())
	if err != nil {
		return fmt.Errorf("source server certificates (omegacd source-certs makes them): %w", err)
	}
	src, err := source.NewServer(dataDir)
	if err != nil {
		return err
	}
	src.KnownHostsFile = cmp.Or(os.Getenv("OMEGACD_SSH_KNOWN_HOSTS"), "/etc/omegacd/known_hosts")
	src.Log = log.With("component", "source")
	g := grpc.NewServer(grpc.Creds(credentials.NewTLS(cfg)))
	source.Serve(g, src)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go src.CollectBlobs(ctx)
	go func() {
		<-ctx.Done()
		g.GracefulStop()
	}()
	log.Info("omegacd source server listening", "addr", addr, "data", dataDir)
	return g.Serve(l)
}

// runAgent runs the Agent on this Host (Q3, Q10) until it is stopped. "omegacd agent restore --files" only writes
// the deployed Applications' files back from the cache, without any engine: before Docker starts at boot (Q59).
// "omegacd agent rollback [APP [REVISION]]" lists the cached Revisions, or deploys one again from the cache (Q25).
func runAgent(log *slog.Logger) error {
	args := os.Args[2:]
	filesOnly := len(args) == 2 && args[0] == "restore" && args[1] == "--files"
	rollback := len(args) >= 1 && len(args) <= 3 && args[0] == "rollback"
	if len(args) > 0 && !filesOnly && !rollback {
		return fmt.Errorf("usage: omegacd agent [restore --files | rollback [APP [REVISION]]] (omegacd agent --help: its settings)")
	}
	if err := useDockerHost(log, os.Getenv, isSocket); err != nil {
		return err
	}
	cfg, err := agent.LoadConfig(os.Getenv, os.Geteuid())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil { // keys live here: owner-only
		return fmt.Errorf("state dir: %w (set OMEGACD_AGENT_STATE_DIR to a folder the Agent's user owns)", err)
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o750); err != nil {
		return fmt.Errorf("work dir: %w (set OMEGACD_AGENT_WORKDIR to a folder the Agent's user owns, "+
			"or give it this one, e.g. RuntimeDirectory=omegacd in its systemd unit)", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if !filesOnly && !(rollback && len(args) < 3) { // listing touches no container
		for _, root := range append([]string{cfg.WorkDir}, cfg.WorkDirAllow...) { // every root: Docker resolves binds there (Q57)
			if err := agent.CheckPathParity(ctx, root); err != nil {
				return err
			}
		}
	}
	db, err := agent.OpenDB(filepath.Join(cfg.StateDir, "agent.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	cache, err := agent.OpenCache(db, filepath.Join(cfg.StateDir, "cache"), cfg.CacheRevisions)
	if err != nil {
		return err
	}
	hostKey, err := agent.HostKey(cfg.StateDir)
	if err != nil {
		return err
	}
	signingKey, err := agent.SigningKey(cfg.StateDir)
	if err != nil {
		return err
	}
	secrets := agent.NewSecrets(cfg.SecretGroup, agent.SOPS{HostKey: hostKey})
	work := agent.NewWorkDir(cfg.WorkDir, db, cache, log.With("component", "agent"))
	work.Allow = cfg.WorkDirAllow
	if filesOnly {
		return agent.New(db, cache, work, secrets, nil, log.With("component", "agent")).RestoreFiles(ctx)
	}
	if rollback && len(args) < 3 {
		return listCached(ctx, agent.New(db, cache, work, secrets, nil, log.With("component", "agent")), db, args[1:])
	}
	if rollback {
		eng, err := engine.New(engine.Config{Log: log.With("component", "engine")})
		if err != nil {
			return err
		}
		defer eng.Close()
		if err := agent.New(db, cache, work, secrets, eng, log.With("component", "agent")).Rollback(ctx, args[1], args[2]); err != nil {
			return err
		}
		fmt.Printf("%s rolled back to %s on this Host; the Control Plane shows it rolled back, its Auto-Sync off\n", args[1], args[2])
		return nil
	}
	if runtime.GOOS == "darwin" {
		log.Warn("macOS is for development only (Q61): Docker Desktop and OrbStack share folders ignoring owners and " +
			"modes, so any container can read decrypted secrets, and there is no systemd; run Agents on Linux")
	}
	for _, root := range append([]string{cfg.WorkDir}, cfg.WorkDirAllow...) {
		if agent.WorkDirOnDisk(root) {
			log.Warn("a work dir is not in RAM: decrypted secrets written there stay on disk, also across reboots; "+
				"keep work dirs on a tmpfs (such as /run/omegacd) to keep them off disk", "workdir", root)
		}
	}
	apps, err := db.ListApps(ctx)
	if err != nil {
		return err
	}
	detected, err := agent.DetectEngine(ctx)
	if err != nil {
		log.Warn("no container engine reachable (DOCKER_HOST or the default socket)", "err", err)
	} else {
		log.Info("container engine", "engine", detected.Name, "version", detected.Version, "rootless", detected.Rootless,
			"os", detected.OS, "arch", detected.Arch)
	}
	eng, err := engine.New(engine.Config{Log: log.With("component", "engine")})
	if err != nil {
		return err
	}
	defer eng.Close()
	log.Info("omegacd agent", "state", cfg.StateDir, "workdir", cfg.WorkDir, "controlPlane", cfg.ControlPlane,
		"cacheRevisions", cfg.CacheRevisions, "apps", len(apps), "hostKey", hostKey.Recipient().String(),
		"fingerprint", domain.Fingerprint(signingKey.Public().(ed25519.PublicKey)))
	log.Info("omegacd agent running")
	ag := agent.New(db, cache, work, secrets, eng, log.With("component", "agent"))
	if cfg.ControlPlane != "" {
		var client *http.Client
		if cfg.CAFile != "" {
			if client, err = agent.TrustCA(cfg.CAFile); err != nil {
				return err
			}
		}
		go ag.Connect(ctx, agent.Connection{HTTPClient: client, URL: cfg.ControlPlane, Invite: cfg.Invite, Key: signingKey,
			KeyPath: filepath.Join(cfg.StateDir, "signing.key"), Engine: detected,
			AgeRecipient: hostKey.Recipient().String(), HostKey: hostKey})
	}
	return ag.Run(ctx)
}

// pollInterval reads OMEGACD_POLL_INTERVAL (default 3m); 0 turns polling off, leaving webhooks and Refresh.
func pollInterval(v string) (time.Duration, error) {
	d, err := time.ParseDuration(cmp.Or(strings.TrimSpace(v), "3m"))
	if err != nil || d < 0 {
		return 0, fmt.Errorf("OMEGACD_POLL_INTERVAL must be a duration like 3m, or 0 to turn polling off, not %q", v)
	}
	return d, nil
}

// listCached prints the cached Revisions of the Applications named (all when none), newest first, the deployed one
// marked: what omegacd agent rollback can go back to.
func listCached(ctx context.Context, ag *agent.Agent, db *agent.DB, names []string) error {
	if len(names) == 0 {
		apps, err := db.ListApps(ctx)
		if err != nil {
			return err
		}
		for _, a := range apps {
			names = append(names, a.Name)
		}
		if len(names) == 0 {
			fmt.Println("no Applications deployed on this Host")
		}
	}
	for _, name := range names {
		revs, deployed, err := ag.Cached(ctx, name)
		if err != nil {
			return err
		}
		fmt.Println(name)
		for _, r := range revs {
			mark := ""
			if r.Revision == deployed {
				mark = "  (deployed)"
			}
			fmt.Printf("  %s  %s%s\n", r.Revision, r.AddedAt.Local().Format("2006-01-02 15:04"), mark)
		}
	}
	return nil
}

// useDockerHost settles the container engine before anything talks to it (M4-04): DOCKER_HOST checked, or the engine's
// socket found and set, the choice logged.
func useDockerHost(log *slog.Logger, getenv func(string) string, isSocket func(string) bool) error {
	host, others, err := engine.DockerHost(getenv, isSocket)
	switch {
	case err != nil:
		return err
	case getenv("DOCKER_HOST") != "":
		log.Info("container engine", "dockerHost", host, "from", "DOCKER_HOST")
		return nil
	case host == "":
		log.Warn("no container engine socket found (rootful or rootless Docker, Podman): trying the default; set DOCKER_HOST if the engine listens elsewhere")
		return nil
	}
	if len(others) > 0 {
		log.Info("container engine socket found, others too: set DOCKER_HOST to use another", "dockerHost", host, "others", others)
	} else {
		log.Info("container engine socket found", "dockerHost", host)
	}
	return os.Setenv("DOCKER_HOST", host)
}

func isSocket(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&fs.ModeSocket != 0
}
