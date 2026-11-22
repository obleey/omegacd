package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/secrets"
	"github.com/obleey/omegacd/internal/source"
)

const keysPath = "/api/settings/encryption-keys"

// keyring is what the people's keys and the Host Keys are, on the OmegaCD the CLI is logged in to (Q199).
type keyring struct {
	hosts []domain.HostView
	keys  []domain.EncryptionKey
}

func (c *CLI) keyring(ctx context.Context, cl *client) (keyring, error) {
	var k keyring
	if err := cl.do(ctx, "GET", "/api/settings/hosts", nil, &k.hosts); err != nil {
		return k, err
	}
	return k, cl.do(ctx, "GET", keysPath, nil, &k.keys)
}

// recipients are host's Host Key and the people's keys covering it (Q190, Q194). Without a person's key nobody could
// open the files again: refused unless hostOnly (Q201).
func (k keyring) recipients(host string, hostOnly bool) ([]string, error) {
	i := slices.IndexFunc(k.hosts, func(h domain.HostView) bool { return h.Name == host })
	if i < 0 {
		return nil, fmt.Errorf("Host %s is not in OmegaCD: add it on the Hosts page first", host)
	}
	r := domain.Recipients(k.hosts[i].AgeRecipient, host, k.keys)
	if r == nil {
		return nil, fmt.Errorf("Host %s has no Host Key yet: its Agent makes one when it enrolls; enroll it first", host)
	}
	if len(r) == 1 && !hostOnly {
		return nil, fmt.Errorf("no person's key covers Host %s, so only the Host could decrypt its secrets and nobody could edit "+
			"them again: add yours (omegacd keygen, then omegacd secrets key add NAME age1…), or pass --host-only", host)
	}
	return r, nil
}

// secretSource is the --source named.
func secretSource(name string) (secrets.Source, error) {
	if s, ok := secrets.Sources[name]; ok {
		return s, nil
	}
	return nil, fmt.Errorf("unknown secret source %q (want sops)", name)
}

// checkoutRoot is the root of the Git checkout the command runs in.
func checkoutRoot(ctx context.Context, command string) (string, error) {
	root, err := git(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("run omegacd %s inside your Git repository's checkout (git rev-parse found none)", command)
	}
	return root, nil
}

// repoURLOf is repoURL, else the checkout's origin remote.
func repoURLOf(ctx context.Context, repoURL string) (string, error) {
	if repoURL != "" {
		return repoURL, nil
	}
	url, err := git(ctx, "remote", "get-url", "origin")
	if err != nil {
		return "", errors.New("the repository has no origin remote: add one, or pass --repo-url")
	}
	return url, nil
}

// pathRule is the rule for the secret files under dir, a folder of the repository ("" or ".": all of it).
func pathRule(dir string) string {
	if dir = path.Clean(strings.Trim(dir, "/")); dir == "." {
		return `^.*\.sops\.`
	}
	return "^" + regexp.QuoteMeta(dir) + `/.*\.sops\.`
}

// secretsInit writes a rule into the repository for each Application of the Host in it (Q197, Q286).
func (c *CLI) secretsInit(ctx context.Context, args []string) error {
	var o opts
	fs := c.flags("secrets init", &o)
	host := fs.String("host", "", "the Host whose secrets these are (required)")
	var paths listFlag
	fs.Var(&paths, "path", "another folder of the repository with this Host's secrets; repeatable")
	src := fs.String("source", "sops", "the secret source: sops")
	repoURL := fs.String("repo-url", "", "the repository's URL as OmegaCD's Applications name it (default: the origin remote)")
	hostOnly := fs.Bool("host-only", false, "encrypt for the Host alone when no person's key covers it: nobody can edit the files again")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	if *host == "" {
		return errors.New("secrets init needs --host NAME: the Host whose secrets these are")
	}
	s, err := secretSource(*src)
	if err != nil {
		return err
	}
	root, err := checkoutRoot(ctx, "secrets init")
	if err != nil {
		return err
	}
	repo, err := repoURLOf(ctx, *repoURL)
	if err != nil {
		return err
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	k, err := c.keyring(ctx, cl)
	if err != nil {
		return err
	}
	recipients, err := k.recipients(*host, *hostOnly)
	if err != nil {
		return err
	}
	var apps []domain.AppView
	if err := cl.do(ctx, "GET", "/api/apps", nil, &apps); err != nil {
		return err
	}
	var rules []secrets.Rule
	for _, a := range apps {
		if a.Host == *host && a.Kind != domain.KindApps && source.NormalizeURL(a.RepoURL) == source.NormalizeURL(repo) {
			rules = append(rules, secrets.Rule{Host: *host, Path: pathRule(a.Path), Recipients: recipients})
		}
	}
	for _, p := range paths {
		rules = append(rules, secrets.Rule{Host: *host, Path: pathRule(p), Recipients: recipients})
	}
	if len(rules) == 0 {
		return fmt.Errorf("no Application of Host %s comes from %s: create them first, or name folders with --path", *host, repo)
	}
	if err := s.WriteRules(root, rules); err != nil {
		return err
	}
	for _, r := range rules {
		fmt.Fprintf(c.Stdout, "%s  %s\n", r.Path, k.names(r.Recipients))
	}
	fmt.Fprintf(c.Stderr, "Wrote %d rule(s) for Host %s: commit them.\n", len(rules), *host)
	return nil
}

// names are recipients as people know them: the Host's key, then the people's by name.
func (k keyring) names(recipients []string) string {
	var out []string
	for _, r := range recipients {
		name := r
		if i := slices.IndexFunc(k.keys, func(e domain.EncryptionKey) bool { return e.Recipient == r }); i >= 0 {
			name = k.keys[i].Name
		} else if i := slices.IndexFunc(k.hosts, func(h domain.HostView) bool { return h.AgeRecipient == r }); i >= 0 {
			name = "Host " + k.hosts[i].Name
		}
		out = append(out, name)
	}
	return strings.Join(out, ", ")
}

// secretsRekey refreshes the rules OmegaCD wrote from the current keys and gives every secret file its rule's keys,
// with the key of the person running it (Q193, Q200, Q288).
func (c *CLI) secretsRekey(ctx context.Context, args []string) error {
	var o opts
	fs := c.flags("secrets rekey", &o)
	src := fs.String("source", "sops", "the secret source: sops")
	hostOnly := fs.Bool("host-only", false, "rekey for a Host alone when no person's key covers it: nobody can edit the files again")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	s, err := secretSource(*src)
	if err != nil {
		return err
	}
	root, err := checkoutRoot(ctx, "secrets rekey")
	if err != nil {
		return err
	}
	rules, err := s.Rules(root)
	if err != nil {
		return err
	}
	if len(rules) == 0 {
		return errors.New("this repository has no rules OmegaCD wrote: run omegacd secrets init --host NAME first")
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	k, err := c.keyring(ctx, cl)
	if err != nil {
		return err
	}
	for i, r := range rules {
		if rules[i].Recipients, err = k.recipients(r.Host, *hostOnly); err != nil {
			return fmt.Errorf("the rule for %s: %w", r.Path, err)
		}
	}
	if err := s.WriteRules(root, rules); err != nil {
		return err
	}
	res, err := s.Rekey(root)
	if err != nil {
		return err
	}
	for _, f := range res.Changed {
		fmt.Fprintln(c.Stdout, "rekeyed     "+f)
	}
	for _, f := range res.NoRule {
		fmt.Fprintln(c.Stdout, "no rule     "+f+" (left as it is)")
	}
	for _, f := range res.CantOpen {
		fmt.Fprintln(c.Stdout, "can't open  "+f)
	}
	if len(res.CantOpen) > 0 {
		return &exitError{code: 1, msg: fmt.Sprintf("%d file(s) are encrypted only for keys you don't hold (such as a Host's alone): "+
			"write their values again and encrypt them with sops; the rest is done", len(res.CantOpen))}
	}
	fmt.Fprintf(c.Stderr, "Rekeyed %d file(s): commit them. Keys removed can still read old commits: change those secrets' values.\n", len(res.Changed))
	return nil
}

func (c *CLI) secretsKeyAdd(ctx context.Context, args []string) error {
	var o opts
	fs := c.flags("secrets key add", &o)
	var hosts listFlag
	fs.Var(&hosts, "host", "a Host the key covers; repeatable (default: all Hosts)")
	pos, err := parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	k := domain.EncryptionKey{Name: pos[0], Recipient: pos[1], AllHosts: len(hosts) == 0, Hosts: hosts}
	if err := cl.do(ctx, "PUT", keysPath+"/"+url.PathEscape(k.Name), k, &k); err != nil {
		return err
	}
	fmt.Fprintf(c.Stdout, "Saved %s for %s. Run omegacd secrets rekey in each repository so the files already there are encrypted for it.\n",
		k.Name, coverage(k))
	return nil
}

func coverage(k domain.EncryptionKey) string {
	if k.AllHosts {
		return "all Hosts"
	}
	if len(k.Hosts) == 0 {
		return "no Host" // its Hosts were deleted (Q291)
	}
	return strings.Join(k.Hosts, ", ")
}

func (c *CLI) secretsKeyList(ctx context.Context, args []string) error {
	var o opts
	if _, err := parse(c.flags("secrets key list", &o), args, 0, 0); err != nil {
		return err
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	var keys []domain.EncryptionKey
	if err := cl.do(ctx, "GET", keysPath, nil, &keys); err != nil {
		return err
	}
	if o.output == "json" {
		return c.printJSON(keys)
	}
	if len(keys) == 0 {
		fmt.Fprintln(c.Stderr, "No keys: only the Hosts can decrypt their secrets, so nobody can edit them and a lost Host loses them (Q191). "+
			"Add yours: omegacd keygen, then omegacd secrets key add NAME age1…")
		return nil
	}
	var rows [][]string
	for _, k := range keys {
		rows = append(rows, []string{k.Name, k.Recipient, coverage(k)})
	}
	c.table([]string{"NAME", "KEY", "HOSTS"}, rows)
	return nil
}

func (c *CLI) secretsKeyRemove(ctx context.Context, args []string) error {
	var o opts
	pos, err := parse(c.flags("secrets key remove", &o), args, 1, 1)
	if err != nil {
		return err
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	if err := cl.do(ctx, "DELETE", keysPath+"/"+url.PathEscape(pos[0]), nil, nil); err != nil {
		return err
	}
	fmt.Fprintf(c.Stdout, "Removed %s. Files stay encrypted for it until omegacd secrets rekey in each repository, and old commits stay "+
		"readable to it: change the values of the secrets it could read.\n", pos[0])
	return nil
}
