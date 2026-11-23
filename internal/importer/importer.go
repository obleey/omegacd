// Package importer writes the Compose stacks running on a Host into a Git checkout as OmegaCD Applications (omegacd
// import): config goes to Git, data stays on the host, secrets are encrypted, and each stack gets a definition with
// adopt: true so OmegaCD takes it over. It never changes the running stacks, and never commits. It runs in two halves
// (Q230, Q317): Plan on the Host's Agent reads the stacks and returns finished files, every secret already encrypted;
// Write in the CLI puts them into the checkout.
package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"

	"github.com/obleey/omegacd/internal/appdef"
	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/secretkey"
	"github.com/obleey/omegacd/internal/secrets"
)

// MaxConfig is the size up to which a mounted file, or a read-only mounted folder, counts as config and is
// copied into Git; anything bigger, and every writable folder, is data and stays on the host.
const MaxConfig = 1 << 20

// Stack is a Compose project found on the host.
type Stack struct {
	Name    string
	Dir     string   // working dir on the host
	Files   []string // compose files, relative to Dir
	Managed bool     // already managed by OmegaCD
}

// Info describes a path on the host: Kind 'f' (file), 'd' (folder) or '-' (missing); Size in bytes.
type Info struct {
	Kind byte
	Size int64
}

// Host is the Docker host the stacks run on.
type Host interface {
	Stacks(ctx context.Context) ([]Stack, error)
	// Info stats paths relative to dir.
	Info(ctx context.Context, dir string, paths []string) (map[string]Info, error)
	// Read returns the regular files at or under paths relative to dir, keyed by their path relative to dir, and what
	// it left out there (anything else but a folder: a symlink, a device, a file too big to copy).
	Read(ctx context.Context, dir string, paths []string) (files map[string][]byte, skipped []string, err error)
	// Storage is where container engines keep data of their own, by the volume it is (Q325): a mountpoint → its name.
	Storage(ctx context.Context) (map[string]string, error)
}

// Request is what the CLI asks the Agent for.
type Request struct {
	AppsDir  string   // where each stack goes, relative to the repository's root: apps
	RepoURL  string   // for the definitions
	Revision string   // the branch the definitions follow
	HostName string   // the Host's name in OmegaCD, whose Agent runs the stacks
	Only     []string // stack names; empty imports every stack not managed by OmegaCD yet
	Defined  []string // Application names already in the checkout's definitions: skipped
	// Recipients are the age public keys secrets are encrypted for; Refuse, when set, says why they may not be used
	// (a Host-only set without --host-only, Q201): a stack with secrets fails with it, one without imports (Q320).
	Recipients []string
	Refuse     string
}

// Result is one stack's import, as the Agent returns it.
type Result struct {
	Name       string
	Skip       string // not imported, not a failure: "managed" (by OmegaCD already) or "defined" (in the checkout)
	Error      string // why it can't be imported
	Report     Report
	Definition []byte            `json:",omitempty"`
	Files      map[string][]byte `json:",omitempty"` // relative to the Application's folder; on the Agent's connection beside the JSON (Q319)
}

// MaxStack is the most one stack's files may come to: its answer is one frame on the Agent's connection (Q319).
// ponytail: one frame per stack; chunk it as blobs if real stacks ever need more.
const MaxStack = 6 << 20

// Plan imports the stacks on h, calling each with every stack's result in name order. An error is one that ends the
// whole import (the stacks can't be listed, one asked for isn't there).
func Plan(ctx context.Context, h Host, req Request, each func(Result) error) error {
	stacks, err := h.Stacks(ctx)
	if err != nil {
		return err
	}
	for _, name := range req.Only {
		if !slices.ContainsFunc(stacks, func(s Stack) bool { return s.Name == name }) {
			return fmt.Errorf("no Compose stack %q on the host", name)
		}
	}
	var storage map[string]string
	for _, s := range stacks {
		if len(req.Only) > 0 && !slices.Contains(req.Only, s.Name) {
			continue
		}
		r := Result{Name: s.Name}
		switch {
		case s.Managed:
			r.Skip = "managed"
		case slices.Contains(req.Defined, s.Name):
			r.Skip = "defined"
		default:
			if storage == nil {
				if storage, err = h.Storage(ctx); err != nil {
					return err
				}
			}
			var err error
			r.Report, r.Files, r.Definition, err = planStack(ctx, h, req, s, storage)
			if err != nil {
				r.Error = err.Error()
			}
		}
		if err := each(r); err != nil {
			return err
		}
	}
	return nil
}

// Options is how Write puts the results into the checkout.
type Options struct {
	Repo        string // root of the Git checkout
	AppsDir     string // as in the Request
	Definitions string // the App of Apps file to append to, relative to Repo: platform/apps.yaml
	Force       bool   // overwrite an existing apps/<name>
	DryRun      bool   // report only
	Out         io.Writer
}

// Write puts the stacks' results into the checkout and reports per stack what it did. A stack that can't be imported
// is reported and skipped; the error is non-nil if any was.
func Write(o Options, results []Result) error {
	var failed []string
	done := 0
	for _, r := range results {
		err := write(o, r)
		switch {
		case r.Skip == "managed":
			fmt.Fprintf(o.Out, "· %s: already managed by OmegaCD, skipped\n", r.Name)
		case r.Skip == "defined":
			fmt.Fprintf(o.Out, "· %s: already defined in %s, skipped\n", r.Name, o.Definitions)
		case err != nil:
			fmt.Fprintf(o.Out, "✗ %s: %v\n", r.Name, err)
			failed = append(failed, r.Name)
		default:
			r.Report.print(o.Out, o.DryRun)
			done++
		}
	}
	switch {
	case len(failed) > 0:
		return fmt.Errorf("not imported: %s", strings.Join(failed, ", "))
	case done == 0:
		fmt.Fprintln(o.Out, "Nothing to import.")
	case !o.DryRun:
		fmt.Fprintf(o.Out, "\nReview with git diff, commit and push; then create an App of Apps with path %s in OmegaCD.\n"+
			"Imported Applications have Auto-Sync off: check each one's Diff, then Sync it to take the stack over.\n", path.Dir(o.Definitions))
	}
	return nil
}

// write puts one stack's files into the checkout and appends its definition; skipped stacks are left alone.
func write(o Options, r Result) error {
	if r.Skip != "" {
		return nil
	}
	if r.Error != "" {
		return errors.New(r.Error)
	}
	dir := path.Join(o.AppsDir, r.Name)
	dest := filepath.Join(o.Repo, filepath.FromSlash(dir))
	if _, err := os.Stat(dest); err == nil && !o.Force {
		return fmt.Errorf("%s exists already (--force overwrites it)", dir)
	}
	if o.DryRun {
		return nil
	}
	if o.Force {
		if err := os.RemoveAll(dest); err != nil {
			return err
		}
	}
	for _, rel := range slices.Sorted(maps.Keys(r.Files)) {
		p := filepath.Join(dest, filepath.FromSlash(rel))
		if !strings.HasPrefix(p, dest+string(filepath.Separator)) { // the Agent's answer is input like any other
			return fmt.Errorf("the Agent sent %q, outside %s", rel, dir)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, r.Files[rel], 0o644); err != nil { // coverage: unreachable, into the folder just made above
			return err
		}
	}
	return appendDefinition(filepath.Join(o.Repo, filepath.FromSlash(o.Definitions)), r.Definition)
}

// ExistingNames are the Application names defined in file already, for Request.Defined.
func ExistingNames(file string) ([]string, error) {
	m, err := existingNames(file)
	return slices.Sorted(maps.Keys(m)), err
}

func existingNames(file string) (map[string]bool, error) {
	names := map[string]bool{}
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		return names, nil
	}
	apps, err := appdef.Parse(filepath.Dir(file))
	if err != nil {
		return nil, err
	}
	for _, a := range apps {
		names[a.Name] = true
	}
	return names, nil
}

// Report is what importing one stack did, for the summary.
type Report struct {
	AppDir          string
	Services        int
	Config, Data    []string `json:",omitempty"` // "path" / "path → /abs/path"
	Encrypted       []string `json:",omitempty"` // "file → file.sops.ext"
	Moved           []string `json:",omitempty"` // "service: KEY"
	Warnings        []string `json:",omitempty"`
	Recreate        bool     // the takeover recreates containers (they mount files that now come from Git)
	RecipientsShort string   `json:",omitempty"`
}

// leftOut says what Read left out: the takeover won't have it unless it is copied by hand.
func (r *Report) leftOut(skipped []string) {
	for _, f := range skipped {
		r.Warnings = append(r.Warnings, fmt.Sprintf("%s left out: not a regular file (a symlink?) or too big to copy; put it in the repository by hand if the stack needs it", f))
	}
}

func (r Report) print(w io.Writer, dryRun bool) {
	verb := "→"
	if dryRun {
		verb = "would go to"
	}
	name := path.Base(r.AppDir)
	fmt.Fprintf(w, "✓ %s %s %s (%d service%s)\n", name, verb, r.AppDir, r.Services, map[bool]string{true: "", false: "s"}[r.Services == 1])
	for _, c := range r.Config {
		fmt.Fprintf(w, "    config   %s\n", c)
	}
	for _, d := range r.Data {
		fmt.Fprintf(w, "    data     %s (stays on the host)\n", d)
	}
	for _, e := range r.Encrypted {
		fmt.Fprintf(w, "    secret   %s, encrypted for %s\n", e, r.RecipientsShort)
	}
	for _, m := range r.Moved {
		fmt.Fprintf(w, "    moved    %s from the compose file into the encrypted .sops.env\n", m)
	}
	for _, x := range r.Warnings {
		fmt.Fprintf(w, "    ! %s\n", x)
	}
	if r.Recreate {
		fmt.Fprintln(w, "    takeover recreates its containers once (they mount files that now come from Git)")
	} else {
		fmt.Fprintln(w, "    takeover keeps its containers running")
	}
}

// mount is a reference from the compose files to a path in the stack's folder.
type mount struct {
	rel      string // relative to the stack dir, cleaned
	readOnly bool
	env      bool // an env_file
	secret   bool // a Compose secret's file
	config   bool // a Compose config's file
}

// enginePaths are where engines keep their own data, whatever volumes exist now (Q325).
var enginePaths = []string{"/var/lib/docker/volumes/", "/var/lib/containers/storage/"}

// planStack imports one stack: its files, every secret encrypted, and its definition.
func planStack(ctx context.Context, h Host, req Request, s Stack, storage map[string]string) (Report, map[string][]byte, []byte, error) {
	r := Report{AppDir: path.Join(req.AppsDir, s.Name)}
	if !validName(s.Name) {
		return r, nil, nil, fmt.Errorf("%q is not a valid Application name (lowercase letters, digits, - and _)", s.Name)
	}
	if len(s.Files) == 0 {
		return r, nil, nil, errors.New("its compose files are not in its folder on the host")
	}

	// 1. the compose files and .env, parsed as written
	files, skipped, err := h.Read(ctx, s.Dir, append(slices.Clone(s.Files), ".env"))
	if err != nil {
		return r, nil, nil, err
	}
	r.leftOut(skipped)
	for _, f := range s.Files {
		if _, ok := files[f]; !ok {
			return r, nil, nil, fmt.Errorf("compose file %s not found in %s", f, s.Dir)
		}
	}
	p, err := loadProject(s, files)
	if err != nil {
		return r, nil, nil, fmt.Errorf("read compose files: %w", err)
	}
	r.Services = len(p.Services) + len(p.DisabledServices)

	// 2. what the services mount and read by a relative path, in the stack's folder or beside it (../)
	refs := map[string]*mount{}
	add := func(raw string, m mount) {
		m.rel = path.Clean(strings.TrimPrefix(raw, "./"))
		if cur, ok := refs[m.rel]; ok {
			cur.readOnly = cur.readOnly && m.readOnly
			cur.env, cur.secret, cur.config = cur.env || m.env, cur.secret || m.secret, cur.config || m.config
			return
		}
		refs[m.rel] = &m
	}
	all := maps.Clone(p.Services)
	maps.Copy(all, p.DisabledServices)
	for _, name := range slices.Sorted(maps.Keys(all)) {
		svc := all[name]
		if svc.Build != nil {
			r.Warnings = append(r.Warnings, fmt.Sprintf("%s uses build: — OmegaCD only runs pre-built images; push the image to a registry and use image:", name))
		}
		for _, v := range svc.Volumes {
			if v.Type == types.VolumeTypeBind && relative(v.Source) {
				add(v.Source, mount{readOnly: v.ReadOnly})
			}
		}
		for _, e := range svc.EnvFiles {
			if relative(e.Path) {
				add(e.Path, mount{env: true})
			}
		}
	}
	for _, c := range p.Configs {
		if c.File != "" && relative(c.File) {
			add(c.File, mount{config: true})
		}
	}
	for _, c := range p.Secrets {
		if c.File != "" && relative(c.File) {
			add(c.File, mount{secret: true})
		}
	}
	rels := slices.Sorted(maps.Keys(refs))
	info, err := h.Info(ctx, s.Dir, rels)
	if err != nil {
		return r, nil, nil, err
	}

	// 3. decide: config into Git, data stays (absolute path), secrets encrypted; the same beside the stack's folder as in
	// it, a copy going into the Application without its ../ (Q323, Q324)
	absolute := map[string]string{} // mount source as cleaned -> absolute path on the host
	var toRead []string
	for _, rel := range rels {
		m, in := refs[rel], info[rel]
		switch {
		case m.env || m.secret || m.config:
			if in.Kind != 'f' {
				r.Warnings = append(r.Warnings, fmt.Sprintf("%s is referenced but not on the host", rel))
				continue
			}
			toRead = append(toRead, rel)
		case in.Kind == 'f' && in.Size <= MaxConfig, in.Kind == 'd' && m.readOnly && in.Size <= MaxConfig:
			toRead = append(toRead, rel)
		default: // a writable or big folder, a big file, or missing (Docker creates it): data
			absolute[rel] = path.Join(s.Dir, rel)
		}
	}
	for _, rel := range slices.Sorted(maps.Keys(absolute)) {
		if where := inStorage(absolute[rel], storage); where != "" {
			return r, nil, nil, fmt.Errorf("%s is %s, inside %s: it would go with that tool's data after the takeover; "+
				"move the data out of it (or mount the volume by name), then import again", rel, absolute[rel], where)
		}
		r.Data = append(r.Data, fmt.Sprintf("%s → %s", rel, absolute[rel]))
	}
	read, skipped, err := h.Read(ctx, s.Dir, toRead)
	if err != nil {
		return r, nil, nil, err
	}
	r.leftOut(skipped)
	out := map[string][]byte{}  // relative path in the app dir -> content
	from := map[string]string{} // and where on the host it came from, relative to the stack's folder
	put := func(key, src string, b []byte) error {
		if prev, ok := from[key]; ok && prev != src {
			return fmt.Errorf("%s and %s would both be %s in the Application: rename one, then import again", prev, src, key)
		}
		out[key], from[key] = b, src
		return nil
	}
	refOf := map[string]*mount{}      // a referenced file's path in the app dir -> its reference
	relocated := map[string]string{}  // a ../ reference as cleaned -> its copy, relative to the app dir
	secretConfig := map[string]bool{} // bind-mounted files that hold a key or credential: encrypted, never in Git as is
	for _, rel := range toRead {
		m := refs[rel]
		refOf[local(rel)] = m
		if local(rel) != rel {
			relocated[rel] = "./" + local(rel)
		}
		if !m.env && !m.secret && !m.config {
			r.Recreate = true // a bind mount now points at OmegaCD's copy of the file
			r.Config = append(r.Config, rel)
			for f, b := range read {
				if (f == rel || strings.HasPrefix(f, rel+"/")) && (secretFile(f) || secretContent(b)) {
					secretConfig[local(f)] = true
				}
			}
		} else if m.config {
			r.Recreate = true // its content hash is new
		}
		if m.secret {
			r.Recreate = true
		}
		for _, f := range slices.Sorted(maps.Keys(read)) {
			if f == rel || strings.HasPrefix(f, rel+"/") {
				if err := put(local(f), f, read[f]); err != nil {
					return r, nil, nil, err
				}
			}
		}
	}

	// 4. secrets written in the compose files move into .env (filled in again with ${KEY})
	dotenvPlain := string(files[".env"])
	compose := map[string][]byte{}
	for _, f := range s.Files {
		b, moved, extra, warns, err := rewrite(files[f], absolute, relocated, envKeys(dotenvPlain))
		if err != nil { // coverage: unreachable, compose-go just loaded the same YAML
			return r, nil, nil, fmt.Errorf("%s: %w", f, err)
		}
		compose[f] = b
		r.Moved = append(r.Moved, moved...)
		r.Warnings = append(r.Warnings, warns...)
		dotenvPlain += extra
	}
	if dotenvPlain != "" {
		if err := put(".env", ".env", []byte(dotenvPlain)); err != nil {
			return r, nil, nil, err
		}
	}
	for _, f := range s.Files {
		if err := put(f, f, compose[f]); err != nil {
			return r, nil, nil, err
		}
	}

	// 5. encrypt env files with secrets, Compose secret files, bind-mounted files holding secrets, and .env when it holds any
	var recipients []string
	encrypt := func(rel string) error {
		enc := sopsName(rel)
		if enc == "" {
			r.Warnings = append(r.Warnings, fmt.Sprintf("%s holds secrets but has no file extension, so it can't be encrypted as *.sops.*: left out — rename it (e.g. %s.env) and import again", rel, rel))
			delete(out, rel)
			return nil
		}
		if recipients == nil {
			switch {
			case req.Refuse != "":
				return errors.New(req.Refuse)
			case len(req.Recipients) == 0:
				return errors.New("secrets found, but no key to encrypt them for")
			}
			recipients = req.Recipients
			r.RecipientsShort = short(recipients)
		}
		b, err := secrets.Encrypt(out[rel], enc, recipients)
		if err != nil {
			return err
		}
		delete(out, rel)
		out[enc] = b
		r.Encrypted = append(r.Encrypted, fmt.Sprintf("%s → %s", rel, enc))
		return nil
	}
	for _, rel := range slices.Sorted(maps.Keys(out)) {
		m := refOf[rel]
		needs := secretConfig[rel] || m != nil && m.secret
		if !needs && (rel == ".env" || m != nil && m.env) {
			var err error
			if needs, err = hasSecrets(string(out[rel])); err != nil {
				r.Warnings = append(r.Warnings, fmt.Sprintf("%s can't be read as an env file (%v): encrypted, as it may hold secrets", rel, err))
			}
		}
		if needs {
			if err := encrypt(rel); err != nil {
				return r, nil, nil, err
			}
		}
	}
	size := 0
	for _, b := range out {
		size += len(b)
	}
	if size > MaxStack {
		return r, nil, nil, fmt.Errorf("its files come to %.1f MB, more than the %d MB an import through the Agent carries: "+
			"mount the biggest as data instead, or copy them into the repository by hand", float64(size)/(1<<20), MaxStack>>20)
	}

	// 6. its definition
	composeFiles := s.Files
	if len(composeFiles) == 1 && composeFiles[0] == "compose.yaml" {
		composeFiles = nil
	}
	def, err := appdef.MarshalAdopting(domain.Application{
		Name: s.Name, RepoURL: req.RepoURL, Path: r.AppDir, TargetRevision: req.Revision,
		ComposeFiles: composeFiles, Host: req.HostName,
	})
	if err != nil { // coverage: unreachable, a definition is strings, lists and booleans: it always encodes as YAML
		return r, nil, nil, err
	}
	return r, out, def, nil
}

// local is where a referenced file goes in the Application: its path in the stack's folder, or beside it without the
// leading ../ (Q323).
func local(rel string) string {
	for strings.HasPrefix(rel, "../") {
		rel = rel[3:]
	}
	if rel == ".." { // the folder above the stack's, as a whole
		return "."
	}
	return rel
}

// inStorage names the engine storage p is in, or "".
func inStorage(p string, storage map[string]string) string {
	for mp, name := range storage {
		if p == mp || strings.HasPrefix(p, strings.TrimSuffix(mp, "/")+"/") {
			return "the volume " + name
		}
	}
	for _, e := range enginePaths {
		if strings.HasPrefix(p+"/", e) {
			return "the engine's storage " + strings.TrimSuffix(e, "/")
		}
	}
	return ""
}

func appendDefinition(file string, def []byte) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	cur, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var b strings.Builder
	b.Write(cur)
	if len(strings.TrimSpace(string(cur))) > 0 {
		if !strings.HasSuffix(string(cur), "\n") {
			b.WriteString("\n")
		}
		b.WriteString("---\n")
	}
	b.WriteString("# imported with omegacd import: Auto-Sync is off until you have checked its Diff\n")
	b.Write(def)
	return os.WriteFile(file, []byte(b.String()), 0o644)
}

// loadProject reads the stack's compose files as written: paths unresolved, env_files not read.
func loadProject(s Stack, files map[string][]byte) (*types.Project, error) {
	dir, err := os.MkdirTemp("", "omegacd-import-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	var paths []string
	for f, b := range files {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil { // coverage: unreachable, the host's files by their own distinct paths, in a new temporary folder
			return nil, err
		}
		if err := os.WriteFile(p, b, 0o600); err != nil { // coverage: unreachable, into the folder just made above
			return nil, err
		}
	}
	for _, f := range s.Files {
		paths = append(paths, filepath.Join(dir, filepath.FromSlash(f)))
	}
	opts, err := cli.NewProjectOptions(paths,
		cli.WithWorkingDirectory(dir), cli.WithEnvFiles(), cli.WithDotEnv, cli.WithName(s.Name),
		cli.WithResolvedPaths(false), cli.WithLoadOptions(func(o *loader.Options) { o.SkipResolveEnvironment = true }))
	if err != nil {
		return nil, err
	}
	return opts.LoadProject(context.Background())
}

// rewrite turns relative data mounts into absolute host paths, points ../ references at their copies in the
// Application (relocated), and moves secrets written in environment: into dotenv lines (returned in extra) referenced
// as ${KEY}. Unchanged files come back byte for byte.
func rewrite(src []byte, absolute, relocated map[string]string, known map[string]string) (out []byte, moved []string, extra string, warnings []string, err error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil { // coverage: unreachable, compose-go just loaded the same YAML
		return nil, nil, "", nil, err
	}
	changed := false
	// to points a relative path at its copy; the node is a scalar holding the path alone
	to := func(n *yaml.Node) {
		if n != nil && n.Kind == yaml.ScalarNode && relative(n.Value) {
			if c, ok := relocated[path.Clean(strings.TrimPrefix(n.Value, "./"))]; ok {
				n.Value, changed = c, true
			}
		}
	}
	for _, top := range []string{"secrets", "configs"} {
		if m := mapValue(docRoot(&doc), top); m != nil {
			for i := 1; i < len(m.Content); i += 2 {
				if m.Content[i].Kind == yaml.MappingNode {
					to(mapValue(m.Content[i], "file"))
				}
			}
		}
	}
	services := mapValue(docRoot(&doc), "services")
	for i := 0; services != nil && i+1 < len(services.Content); i += 2 {
		name, svc := services.Content[i].Value, services.Content[i+1]
		switch ef := mapValue(svc, "env_file"); {
		case ef == nil:
		case ef.Kind == yaml.SequenceNode:
			for _, e := range ef.Content {
				if e.Kind == yaml.MappingNode {
					e = mapValue(e, "path")
				}
				to(e)
			}
		default:
			to(ef)
		}
		if vols := mapValue(svc, "volumes"); vols != nil && vols.Kind == yaml.SequenceNode {
			for _, v := range vols.Content {
				src := v
				if v.Kind == yaml.MappingNode {
					src = mapValue(v, "source")
				}
				if src == nil || src.Kind != yaml.ScalarNode {
					continue
				}
				head, tail, _ := strings.Cut(src.Value, ":")
				if v.Kind == yaml.MappingNode {
					head, tail = src.Value, ""
				}
				if strings.Contains(head, "$") {
					if relative(head) || strings.HasPrefix(head, "${") {
						warnings = append(warnings, fmt.Sprintf("%s mounts %s, which uses a variable: check it resolves to an absolute path on the host", name, head))
					}
					continue
				}
				clean := path.Clean(strings.TrimPrefix(head, "./"))
				abs, ok := absolute[clean]
				if c, copied := relocated[clean]; copied {
					abs, ok = c, true
				}
				if !ok || !relative(head) {
					continue
				}
				if v.Kind == yaml.MappingNode {
					src.Value = abs
				} else {
					src.Value = abs + ":" + tail
				}
				changed = true
			}
		}
		env := mapValue(svc, "environment")
		if env == nil {
			continue
		}
		set := func(k, v string, node *yaml.Node, asItem bool) {
			if !secretkey.Plain(k, v) {
				return
			}
			if prev, ok := known[k]; ok && prev != v {
				warnings = append(warnings, fmt.Sprintf("%s: %s is in plain text in the compose file and set differently in .env: left as is — move it by hand", name, k))
				return
			}
			if asItem {
				node.Value = k + "=${" + k + "}"
			} else {
				node.Value, node.Style = "${"+k+"}", 0
			}
			if _, ok := known[k]; !ok {
				extra += dotenvLine(k, v)
				known[k] = v
			}
			moved = append(moved, name+": "+k)
			changed = true
		}
		switch env.Kind {
		case yaml.MappingNode:
			for j := 0; j+1 < len(env.Content); j += 2 {
				set(env.Content[j].Value, env.Content[j+1].Value, env.Content[j+1], false)
			}
		case yaml.SequenceNode:
			for _, item := range env.Content {
				if k, v, ok := strings.Cut(item.Value, "="); ok {
					set(k, v, item, true)
				}
			}
		}
	}
	if !changed {
		return src, moved, extra, warnings, nil
	}
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil { // coverage: unreachable, a document just parsed encodes back
		return nil, nil, "", nil, err
	}
	return []byte(b.String()), moved, extra, warnings, enc.Close()
}

func docRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	return doc // coverage: unreachable, compose-go loaded the file: a document with content
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode { // coverage: unreachable, compose-go accepted the file: its services and their settings are mappings
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func relative(p string) bool {
	return p != "" && !path.IsAbs(p) && !strings.HasPrefix(p, "~")
}

// envKeys is the .env's keys and values, for the secrets moved there from the compose files.
func envKeys(dotenvText string) map[string]string {
	m, err := dotenv.UnmarshalWithLookup(dotenvText, nil)
	if err != nil { // coverage: unreachable, the stack's .env was loaded with its compose files already
		return map[string]string{}
	}
	return m
}

// hasSecrets reports whether an env file holds a secret in plain text; one that can't be read counts as holding one.
func hasSecrets(dotenvText string) (bool, error) {
	m, err := dotenv.UnmarshalWithLookup(dotenvText, nil)
	if err != nil {
		return true, err
	}
	for k, v := range m {
		if secretkey.Plain(k, v) {
			return true, nil
		}
	}
	return false, nil
}

// dotenvLine writes KEY=value, single-quoted (no escapes) when needed, double-quoted if it holds a quote.
func dotenvLine(k, v string) string {
	switch {
	case v != "" && !strings.ContainsAny(v, " \t#'\"\\$`\n"):
		return k + "=" + v + "\n"
	case !strings.ContainsAny(v, "'\n"):
		return k + "='" + v + "'\n"
	default:
		return k + "=\"" + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "$", `\$`).Replace(v) + "\"\n"
	}
}

// sopsName is where an encrypted file goes: db.env → db.sops.env, .env → .sops.env; "" without an extension.
func sopsName(rel string) string {
	dir, base := path.Split(rel)
	if base == ".env" {
		return dir + ".sops.env"
	}
	i := strings.LastIndex(base, ".")
	if i <= 0 {
		return ""
	}
	return dir + base[:i] + ".sops" + base[i:]
}

// secretLine is a `key: value`, `"key": "value",` or KEY=value line, for secretContent.
var secretLine = regexp.MustCompile(`^\s*["']?([A-Za-z0-9_.-]+)["']?\s*[:=]\s*(.*?)\s*,?\s*$`)

// secretContent guesses from a file's content that it holds a key or credential: a private key, an age
// identity, or a setting with a secret-like name and a literal value (password: hunter2, "token": "abc").
func secretContent(b []byte) bool {
	s := string(b)
	if strings.Contains(s, "PRIVATE KEY-----") || strings.Contains(s, "AGE-SECRET-KEY-") {
		return true
	}
	for line := range strings.Lines(s) {
		if m := secretLine.FindStringSubmatch(line); m != nil && secretkey.Plain(m[1], strings.Trim(m[2], `"'`)) {
			return true
		}
	}
	return false
}

// secretFile guesses from a file name that it holds a key or credential.
func secretFile(rel string) bool {
	base := strings.ToLower(path.Base(rel))
	return strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") || strings.HasSuffix(base, ".p12") ||
		strings.Contains(base, "secret") || strings.Contains(base, "password") || strings.Contains(base, "token") || strings.HasPrefix(base, "id_")
}

func short(recipients []string) string {
	var s []string
	for _, r := range recipients {
		if len(r) > 16 {
			r = r[:12] + "…" + r[len(r)-4:]
		}
		s = append(s, r)
	}
	return strings.Join(s, ", ")
}

func validName(n string) bool {
	if n == "" || len(n) > 63 || !(n[0] >= 'a' && n[0] <= 'z' || n[0] >= '0' && n[0] <= '9') {
		return false
	}
	return strings.IndexFunc(n, func(c rune) bool {
		return !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_')
	}) < 0
}
