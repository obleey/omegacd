package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/importer"
)

// listFlag collects a repeatable flag.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// importStacks writes the Compose stacks running on a host into this Git checkout as Applications.
func (c *CLI) importStacks(ctx context.Context, args []string) error {
	var o opts
	fs := c.flags("import", &o)
	hostName := fs.String("host", "", "the Host's name in OmegaCD, whose Agent runs these stacks (required)")
	var recipients listFlag
	fs.Var(&recipients, "recipient", "age public key to encrypt secrets for (the Host Key from Settings → Hosts); repeatable; default: looked up on your OmegaCD")
	appsDir := fs.String("apps-dir", "apps", "folder for the stacks, relative to the repository root")
	defs := fs.String("definitions", "platform/apps.yaml", "App of Apps file to add the definitions to, relative to the repository root")
	repoURL := fs.String("repo-url", "", "repository URL for the definitions (default: the origin remote)")
	force := fs.Bool("force", false, "overwrite an existing apps/<name>")
	dryRun := fs.Bool("dry-run", false, "only report what would be imported")
	only, err := parse(fs, args, 0, 1<<20)
	if err != nil {
		return err
	}
	if *hostName == "" { // every Application names its Host (Q104)
		return errors.New("import needs --host NAME: the name this machine has (or will have) in OmegaCD under Settings → Hosts")
	}

	root, err := git(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return errors.New("run omegacd import inside your Git repository's checkout (git rev-parse found none)")
	}
	if *repoURL == "" {
		if *repoURL, err = git(ctx, "remote", "get-url", "origin"); err != nil {
			return errors.New("the repository has no origin remote: add one, or pass --repo-url")
		}
	}
	revision, _ := git(ctx, "branch", "--show-current")
	if revision == "" {
		revision = "main"
	}

	dial := c.ImportHost
	if dial == nil {
		dial = func() (importer.Host, error) { return importer.Dial() }
	}
	h, err := dial()
	if err != nil {
		return fmt.Errorf("connect to Docker: %w", err)
	}
	if cl, ok := h.(interface{ Close() error }); ok {
		defer cl.Close()
	}
	return importer.Run(ctx, h, importer.Options{
		Repo: root, AppsDir: *appsDir, Definitions: *defs, RepoURL: *repoURL, Revision: revision,
		HostName: *hostName, Only: only, Force: *force, DryRun: *dryRun, Out: c.Stdout,
		Recipients: func() ([]string, error) {
			if len(recipients) > 0 {
				return recipients, nil
			}
			return c.hostRecipient(ctx, o, *hostName)
		},
	})
}

// hostRecipient looks up the Host Key's public key, from its Agent (Q108), on the OmegaCD the CLI is logged in to.
func (c *CLI) hostRecipient(ctx context.Context, o opts, host string) ([]string, error) {
	cl, err := c.client(o)
	if err != nil {
		return nil, fmt.Errorf("secrets found: pass --recipient age1… (the Host Key of %s from Settings → Hosts), or omegacd login so it can be looked up", host)
	}
	var hosts []domain.HostView
	if err := cl.do(ctx, "GET", "/api/settings/hosts", nil, &hosts); err != nil {
		return nil, fmt.Errorf("look up the Host Key of %s: %w (or pass --recipient)", host, err)
	}
	for _, h := range hosts {
		if h.Name == host {
			if h.AgeRecipient == "" {
				return nil, fmt.Errorf("Host %s has no Host Key yet: its Agent makes one when it enrolls; enroll it first, or pass --recipient", host)
			}
			return []string{h.AgeRecipient}, nil
		}
	}
	return nil, fmt.Errorf("Host %s is not in OmegaCD: add it under Settings → Hosts first, or pass --recipient", host)
}

func git(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	return strings.TrimSpace(string(out)), err
}
