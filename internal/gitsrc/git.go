// Package gitsrc implements domain.Git with go-git, keeping one bare clone per Repository.
package gitsrc

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	neturl "net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/obleey/omegacd/internal/domain"
)

var _ domain.Git = (*Git)(nil)

type Git struct {
	// FetchCooldown: Resolve skips fetching a Repository fetched successfully less than this long ago.
	FetchCooldown time.Duration
	// KnownHostsFile verifies SSH host keys (OpenSSH known_hosts format). SSH is refused without it.
	KnownHostsFile string

	cacheDir string
	creds    func(ctx context.Context) ([]domain.RepoCredential, error)
	mu       sync.Mutex
	repos    map[string]*repo
}

type repo struct {
	mu        sync.Mutex
	r         *git.Repository
	lastFetch time.Time
}

func New(cacheDir string, creds func(ctx context.Context) ([]domain.RepoCredential, error)) *Git {
	return &Git{FetchCooldown: 5 * time.Second, cacheDir: cacheDir, creds: creds, repos: map[string]*repo{}}
}

// open returns the locked cache entry for url, creating the bare clone if needed. Call unlock when done.
func (g *Git) open(url string) (*repo, func(), error) {
	sum := sha256.Sum256([]byte(NormalizeURL(url)))
	key := hex.EncodeToString(sum[:])
	g.mu.Lock()
	e, ok := g.repos[key]
	if !ok {
		e = &repo{}
		g.repos[key] = e
	}
	g.mu.Unlock()
	e.mu.Lock()
	if e.r == nil {
		dir := filepath.Join(g.cacheDir, key)
		r, err := git.PlainOpen(dir)
		if errors.Is(err, git.ErrRepositoryNotExists) {
			r, err = git.PlainInit(dir, true)
			if err == nil {
				_, err = r.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{url}})
			}
		}
		if err != nil {
			e.mu.Unlock()
			return nil, nil, fmt.Errorf("open repo cache for %s: %w", url, err)
		}
		e.r = r
	}
	return e, e.mu.Unlock, nil
}

func (g *Git) fetch(ctx context.Context, e *repo, url string) error {
	auth, err := g.auth(ctx, url)
	if err != nil {
		return err
	}
	err = e.r.FetchContext(ctx, &git.FetchOptions{
		RemoteURL: url,
		RefSpecs:  []config.RefSpec{"+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*"},
		Tags:      git.NoTags,
		Force:     true,
		Prune:     true,
		Auth:      auth,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("fetch %s: %w", url, err)
	}
	e.lastFetch = time.Now()
	return nil
}

// MatchCredential returns the RepoCredential with the longest URLPrefix that url starts with, or nil.
func MatchCredential(creds []domain.RepoCredential, url string) *domain.RepoCredential {
	var best *domain.RepoCredential
	for i, c := range creds {
		if strings.HasPrefix(url, c.URLPrefix) && (best == nil || len(c.URLPrefix) > len(best.URLPrefix)) {
			best = &creds[i]
		}
	}
	return best
}

func (g *Git) credential(ctx context.Context, url string) (*domain.RepoCredential, error) {
	if g.creds == nil {
		return nil, nil
	}
	creds, err := g.creds(ctx)
	if err != nil {
		return nil, fmt.Errorf("load repo credentials: %w", err)
	}
	return MatchCredential(creds, url), nil
}

func (g *Git) auth(ctx context.Context, url string) (transport.AuthMethod, error) {
	best, err := g.credential(ctx, url)
	if err != nil || best == nil {
		return nil, err
	}
	return g.authFor(best, url)
}

// authFor is nil when best holds no secret usable for url's scheme.
func (g *Git) authFor(best *domain.RepoCredential, url string) (transport.AuthMethod, error) {
	user := cmp.Or(best.Username, "git")
	switch {
	case strings.HasPrefix(url, "http://"), strings.HasPrefix(url, "https://"):
		if best.Password == "" {
			return nil, nil
		}
		return &githttp.BasicAuth{Username: user, Password: best.Password}, nil
	case best.SSHPrivateKey != "":
		keys, err := gitssh.NewPublicKeys(user, []byte(best.SSHPrivateKey), "")
		if err != nil {
			return nil, fmt.Errorf("ssh key for %s: %w", best.URLPrefix, err)
		}
		if g.KnownHostsFile == "" {
			return nil, fmt.Errorf("ssh to %s: no known_hosts file configured (OMEGACD_SSH_KNOWN_HOSTS)", url)
		}
		if keys.HostKeyCallback, err = gitssh.NewKnownHostsCallback(g.KnownHostsFile); err != nil {
			return nil, fmt.Errorf("load known_hosts: %w", err)
		}
		return keys, nil
	}
	return nil, nil
}

// CheckAccess lists the remote's refs (no clone, no cache) to tell whether, and how, repoURL can be read.
func (g *Git) CheckAccess(ctx context.Context, repoURL string) (domain.RepoAccess, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cred, err := g.credential(ctx, repoURL)
	if err != nil {
		return domain.RepoAccess{}, err
	}
	clean := func(err error) string {
		var ue *neturl.Error
		if errors.As(err, &ue) { // drop the `Get "<url>":` wrapper
			err = ue.Err
		}
		msg := err.Error()
		if u, perr := neturl.Parse(repoURL); perr == nil {
			if pw, ok := u.User.Password(); ok && pw != "" {
				msg = strings.ReplaceAll(msg, pw, "***")
			}
		}
		if cred != nil && cred.Password != "" {
			msg = strings.ReplaceAll(msg, cred.Password, "***")
		}
		return msg
	}
	ep, err := transport.NewEndpoint(repoURL)
	if err != nil {
		return domain.RepoAccess{Access: "error", Message: clean(err)}, nil
	}
	if ep.Protocol != "ssh" { // ssh always needs a key, so an anonymous attempt is pointless
		branch, err := listHead(ctx, repoURL, nil)
		switch {
		case err == nil:
			return domain.RepoAccess{Access: "public", DefaultBranch: branch}, nil
		case ep.Protocol == "file" || !isAuthErr(err):
			return domain.RepoAccess{Access: "error", Message: clean(err)}, nil
		}
		if cred == nil {
			return domain.RepoAccess{Access: "denied", Message: deniedReason(err) + "; no repository credential matches this URL"}, nil
		}
	} else if cred == nil {
		return domain.RepoAccess{Access: "denied", Message: "SSH needs a repository credential with a private key; none matches this URL"}, nil
	}
	auth, err := g.authFor(cred, repoURL)
	if err != nil {
		return domain.RepoAccess{Access: "error", Message: clean(err)}, nil
	}
	if auth == nil {
		return domain.RepoAccess{Access: "denied", Message: fmt.Sprintf("credential %s has no password/token or SSH key for this URL", cred.URLPrefix)}, nil
	}
	branch, err := listHead(ctx, repoURL, auth)
	switch {
	case err == nil:
		return domain.RepoAccess{Access: "credential", CredentialPrefix: cred.URLPrefix, DefaultBranch: branch}, nil
	case isAuthErr(err):
		return domain.RepoAccess{Access: "denied", Message: fmt.Sprintf("%s with credential %s — check the token and its permissions", deniedReason(err), cred.URLPrefix)}, nil
	}
	return domain.RepoAccess{Access: "error", Message: clean(err)}, nil
}

// listHead lists the remote's refs and returns the branch HEAD points to ("" if unknown or the repository is empty).
func listHead(ctx context.Context, url string, auth transport.AuthMethod) (string, error) {
	rem := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{Name: "origin", URLs: []string{url}})
	refs, err := rem.ListContext(ctx, &git.ListOptions{Auth: auth})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, r := range refs {
		if r.Name() == plumbing.HEAD && r.Type() == plumbing.SymbolicReference {
			return r.Target().Short(), nil
		}
	}
	return "", nil
}

func isAuthErr(err error) bool {
	return errors.Is(err, transport.ErrAuthenticationRequired) || errors.Is(err, transport.ErrAuthorizationFailed) || errors.Is(err, transport.ErrRepositoryNotFound)
}

func deniedReason(err error) string {
	switch {
	case errors.Is(err, transport.ErrRepositoryNotFound), strings.Contains(strings.ToLower(err.Error()), "not found"):
		// GitHub answers "not found" for private repositories it will not show you
		return "Repository not found or private"
	case errors.Is(err, transport.ErrAuthorizationFailed):
		return "Authorization failed"
	}
	return "Authentication required"
}

func (g *Git) Invalidate(repoURL string) {
	sum := sha256.Sum256([]byte(NormalizeURL(repoURL)))
	g.mu.Lock()
	e, ok := g.repos[hex.EncodeToString(sum[:])]
	g.mu.Unlock()
	if ok {
		e.mu.Lock()
		e.lastFetch = time.Time{}
		e.mu.Unlock()
	}
}

func (g *Git) Resolve(ctx context.Context, repoURL, rev string) (string, error) {
	e, unlock, err := g.open(repoURL)
	if err != nil {
		return "", err
	}
	defer unlock()
	if time.Since(e.lastFetch) >= g.FetchCooldown {
		if err := g.fetch(ctx, e, repoURL); err != nil {
			return "", err
		}
	}
	// Branches first (origin/<rev>), then ResolveRevision covers tags (peeled) and full/short SHAs.
	for _, candidate := range []string{"origin/" + rev, rev} {
		if h, err := e.r.ResolveRevision(plumbing.Revision(candidate)); err == nil {
			return h.String(), nil
		}
	}
	return "", fmt.Errorf("revision %q in %s: %w", rev, repoURL, domain.ErrNotFound)
}

// commit returns the commit, fetching once if it is not in the cache yet.
func (g *Git) commit(ctx context.Context, e *repo, url, sha string) (*object.Commit, error) {
	if sha == "" {
		return nil, fmt.Errorf("empty commit sha: %w", domain.ErrInvalid)
	}
	c, err := e.r.CommitObject(plumbing.NewHash(sha))
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		if err := g.fetch(ctx, e, url); err != nil {
			return nil, err
		}
		c, err = e.r.CommitObject(plumbing.NewHash(sha))
	}
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return nil, fmt.Errorf("commit %s: %w", sha, domain.ErrNotFound)
	}
	return c, err
}

func cleanPath(p string) string {
	return path.Clean("/" + p)[1:]
}

// subtree returns the tree at p, or nil if it does not exist.
func subtree(c *object.Commit, p string) (*object.Tree, error) {
	t, err := c.Tree()
	if err != nil || p == "" {
		return t, err
	}
	t, err = t.Tree(p)
	if errors.Is(err, object.ErrDirectoryNotFound) || errors.Is(err, object.ErrEntryNotFound) {
		return nil, nil
	}
	return t, err
}

func (g *Git) TreeHash(ctx context.Context, repoURL, sha, p string) (string, error) {
	e, unlock, err := g.open(repoURL)
	if err != nil {
		return "", err
	}
	defer unlock()
	c, err := g.commit(ctx, e, repoURL, sha)
	if err != nil {
		return "", err
	}
	t, err := subtree(c, cleanPath(p))
	if err != nil || t == nil {
		return "", err
	}
	return t.Hash.String(), nil
}

func (g *Git) Export(ctx context.Context, repoURL, sha, p, dest string) error {
	e, unlock, err := g.open(repoURL)
	if err != nil {
		return err
	}
	defer unlock()
	c, err := g.commit(ctx, e, repoURL, sha)
	if err != nil {
		return err
	}
	t, err := subtree(c, cleanPath(p))
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("path %q at %s: %w", p, sha, domain.ErrNotFound)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	keep := map[string]bool{}
	err = t.Files().ForEach(func(f *object.File) error {
		keep[f.Name] = true
		if base := path.Base(f.Name); strings.Contains(base, ".sops.") {
			keep[path.Join(path.Dir(f.Name), strings.Replace(base, ".sops.", ".", 1))] = true
		}
		content, err := f.Contents()
		if err != nil {
			return err
		}
		return writeEntry(filepath.Join(dest, filepath.FromSlash(f.Name)), f.Name, f.Mode, []byte(content))
	})
	if err != nil {
		return fmt.Errorf("export %s@%s:%s: %w", repoURL, sha, p, err)
	}
	return prune(dest, keep)
}

func writeEntry(target, name string, mode filemode.FileMode, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	fi, statErr := os.Lstat(target)
	if mode == filemode.Symlink {
		link := string(content)
		if path.IsAbs(link) || strings.HasPrefix(path.Join(path.Dir(name), link), "..") {
			return fmt.Errorf("symlink %s -> %s points outside the app path: %w", name, link, domain.ErrInvalid)
		}
		if statErr == nil {
			if cur, _ := os.Readlink(target); fi.Mode()&fs.ModeSymlink != 0 && cur == link {
				return nil
			}
			if err := os.RemoveAll(target); err != nil {
				return err
			}
		}
		return os.Symlink(link, target)
	}
	perm := fs.FileMode(0o644)
	if mode == filemode.Executable {
		perm = 0o755
	}
	if statErr == nil && !fi.Mode().IsRegular() {
		if err := os.RemoveAll(target); err != nil {
			return err
		}
	} else if statErr == nil {
		if cur, err := os.ReadFile(target); err == nil && bytes.Equal(cur, content) {
			if fi.Mode().Perm() == perm {
				return nil
			}
			return os.Chmod(target, perm)
		}
	}
	// Truncate in place instead of rename so the inode stays the same for Docker bind mounts.
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Chmod(target, perm)
}

// prune deletes files under dest not in keep, then empty directories.
func prune(dest string, keep map[string]bool) error {
	var dirs []string
	err := filepath.WalkDir(dest, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dest, p)
		if d.IsDir() {
			if rel != "." {
				dirs = append(dirs, p)
			}
			return nil
		}
		if !keep[filepath.ToSlash(rel)] {
			return os.Remove(p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	slices.Reverse(dirs)
	for _, d := range dirs {
		_ = os.Remove(d) // fails while non-empty, which is what we want
	}
	return nil
}

func (g *Git) Diff(ctx context.Context, repoURL, fromSHA, toSHA, p string) (string, error) {
	e, unlock, err := g.open(repoURL)
	if err != nil {
		return "", err
	}
	defer unlock()
	var from *object.Tree
	if fromSHA != "" {
		c, err := g.commit(ctx, e, repoURL, fromSHA)
		if err != nil {
			return "", err
		}
		if from, err = c.Tree(); err != nil {
			return "", err
		}
	}
	c, err := g.commit(ctx, e, repoURL, toSHA)
	if err != nil {
		return "", err
	}
	to, err := c.Tree()
	if err != nil {
		return "", err
	}
	changes, err := object.DiffTreeWithOptions(ctx, from, to, nil)
	if err != nil {
		return "", err
	}
	prefix := cleanPath(p)
	under := func(name string) bool {
		return name != "" && (prefix == "" || name == prefix || strings.HasPrefix(name, prefix+"/"))
	}
	changes = slices.DeleteFunc(changes, func(ch *object.Change) bool {
		return !under(ch.From.Name) && !under(ch.To.Name)
	})
	patch, err := changes.PatchContext(ctx)
	if err != nil {
		return "", err
	}
	return patch.String(), nil
}

// NormalizeURL reduces a clone URL to "host/org/repo" so different spellings of one Repository compare equal.
func NormalizeURL(u string) string {
	u = strings.TrimSpace(u)
	scheme := false
	if i := strings.Index(u, "://"); i >= 0 {
		u, scheme = u[i+3:], true
	}
	slash := strings.Index(u+"/", "/")
	if i := strings.LastIndex(u[:slash], "@"); i >= 0 {
		u = u[i+1:]
		slash -= i + 1
	}
	if !scheme {
		if i := strings.Index(u[:slash], ":"); i >= 0 {
			u = u[:i] + "/" + strings.TrimPrefix(u[i+1:], "/")
			slash = i
		}
	}
	u = strings.ToLower(u[:slash]) + u[slash:]
	u = strings.TrimSuffix(u, "/")
	u = strings.TrimSuffix(u, ".git")
	return strings.TrimSuffix(u, "/")
}
