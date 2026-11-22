package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

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
	fs.Var(&recipients, "recipient", "age public key to encrypt secrets for, instead of the Host Key and the people's keys covering the Host (looked up on your OmegaCD); repeatable")
	hostOnly := fs.Bool("host-only", false, "encrypt for the Host alone when no person's key covers it: nobody can edit the files again")
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
		return errors.New("import needs --host NAME: the name this machine has (or will have) in OmegaCD, on its Hosts page")
	}

	root, err := checkoutRoot(ctx, "import")
	if err != nil {
		return err
	}
	if *repoURL, err = repoURLOf(ctx, *repoURL); err != nil {
		return err
	}
	revision, _ := git(ctx, "branch", "--show-current") // a detached HEAD prints nothing, and that is all that goes wrong here
	if revision == "" {
		revision = "main"
		fmt.Fprintln(c.Stderr, "no current branch (a detached HEAD?): the Applications follow main; check targetRevision")
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
			return c.hostRecipients(ctx, o, *hostName, *hostOnly)
		},
	})
}

// hostRecipients are the Host Key and the people's keys covering the Host (Q194), on the OmegaCD the CLI is logged in to.
func (c *CLI) hostRecipients(ctx context.Context, o opts, host string, hostOnly bool) ([]string, error) {
	cl, err := c.client(o)
	if err != nil {
		return nil, fmt.Errorf("secrets found: omegacd login so their keys can be looked up, or pass --recipient age1…")
	}
	k, err := c.keyring(ctx, cl)
	if err != nil {
		return nil, fmt.Errorf("look up the keys of %s: %w (or pass --recipient)", host, err)
	}
	r, err := k.recipients(host, hostOnly)
	if err != nil {
		return nil, fmt.Errorf("%w (or pass --recipient)", err)
	}
	return r, nil
}

func git(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	return strings.TrimSpace(string(out)), err
}
