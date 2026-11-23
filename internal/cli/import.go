package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/obleey/omegacd/internal/importer"
)

// listFlag collects a repeatable flag.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// importStacks writes the Compose stacks running on a Host into this Git checkout as Applications: the Host's Agent
// reads them and returns the files, every secret encrypted there (M6-05, Q230); this writes them.
func (c *CLI) importStacks(ctx context.Context, args []string) error {
	var o opts
	fs := c.flags("import", &o)
	hostName := fs.String("host", "", "the Host in OmegaCD whose Agent runs these stacks (required)")
	var recipients listFlag
	fs.Var(&recipients, "recipient", "age public key to encrypt secrets for, instead of the Host Key and the people's keys covering the Host (looked up on your OmegaCD); repeatable")
	hostOnly := fs.Bool("host-only", false, "encrypt for the Host alone when no person's key covers it: nobody can edit the files again")
	appsDir := fs.String("apps-dir", "apps", "folder for the stacks, relative to the repository root")
	defs := fs.String("definitions", "platform/apps.yaml", "App of Apps file to add the definitions to, relative to the repository root")
	repoURL := fs.String("repo-url", "", "repository URL for the definitions (default: the origin remote)")
	force := fs.Bool("force", false, "overwrite an existing apps/<name>")
	dryRun := fs.Bool("dry-run", false, "only report what would be imported")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for the Agent's answer")
	only, err := parse(fs, args, 0, 1<<20)
	if err != nil {
		return err
	}
	if *hostName == "" { // every Application names its Host (Q104)
		return errors.New("import needs --host NAME: the Host in OmegaCD whose Agent runs the stacks, on its Hosts page")
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
	defined, err := importer.ExistingNames(filepath.Join(root, filepath.FromSlash(*defs)))
	if err != nil {
		return err
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	req := importer.Request{AppsDir: *appsDir, RepoURL: *repoURL, Revision: revision, HostName: *hostName, Only: only,
		Defined: defined, Recipients: recipients}
	if len(recipients) == 0 {
		if req.Recipients, req.Refuse, err = c.hostRecipients(ctx, cl, *hostName, *hostOnly); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	var results []importer.Result
	if err := cl.do(ctx, "POST", "/api/settings/hosts/"+url.PathEscape(*hostName)+"/import", req, &results); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("the Agent on %s didn't answer within %s (--timeout)", *hostName, *timeout)
		}
		return err
	}
	return importer.Write(importer.Options{Repo: root, AppsDir: *appsDir, Definitions: *defs, Force: *force, DryRun: *dryRun,
		Out: c.Stdout}, results)
}

// hostRecipients are the Host Key and the people's keys covering the Host (Q194), on the OmegaCD the CLI is logged in
// to; refuse says why they may not be used, a Host-only set without --host-only (Q201), which only a stack with
// secrets is refused for (Q320).
func (c *CLI) hostRecipients(ctx context.Context, cl *client, host string, hostOnly bool) (recipients []string, refuse string, err error) {
	k, err := c.keyring(ctx, cl)
	if err != nil {
		return nil, "", fmt.Errorf("look up the keys of %s: %w (or pass --recipient)", host, err)
	}
	if recipients, err = k.recipients(host, true); err != nil {
		return nil, "", err
	}
	if _, err := k.recipients(host, hostOnly); err != nil {
		refuse = err.Error() + " (or pass --recipient)"
	}
	return recipients, refuse, nil
}

func git(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	return strings.TrimSpace(string(out)), err
}
