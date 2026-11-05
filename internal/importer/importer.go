// Package importer writes the Compose stacks running on a Docker host into a Git checkout as OmegaCD
// Applications (omegacd import): config goes to Git, data stays on the host, secrets are encrypted for the
// Host Key, and each stack gets a definition with adopt: true so OmegaCD takes it over. It never changes the
// running stacks, and never commits.
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
	// Read returns the regular files at or under paths relative to dir, keyed by their path relative to dir.
	Read(ctx context.Context, dir string, paths []string) (map[string][]byte, error)
}

type Options struct {
	Repo        string // root of the Git checkout
	AppsDir     string // where each stack goes, relative to Repo: apps
	Definitions string // the App of Apps file to append to, relative to Repo: platform/apps.yaml
	RepoURL     string
	Revision    string
	HostName    string // the Host's name in OmegaCD; "" for the local Host
	// Recipients returns the age public keys to encrypt secrets for; asked only when a stack has secrets.
	Recipients func() ([]string, error)
	Only       []string // stack names; empty imports every stack not managed by OmegaCD yet
	Force      bool     // overwrite an existing apps/<name>
	DryRun     bool     // report only
	Out        io.Writer
}

// Run imports the stacks and reports per stack what it did. A stack that can't be imported is reported and
// skipped; the error is non-nil if any was.
func Run(ctx context.Context, h Host, o Options) error {
	stacks, err := h.Stacks(ctx)
	if err != nil {
		return err
	}
	for _, name := range o.Only {
		if !slices.ContainsFunc(stacks, func(s Stack) bool { return s.Name == name }) {
			return fmt.Errorf("no Compose stack %q on the host", name)
		}
	}
	defs, err := existingNames(filepath.Join(o.Repo, o.Definitions))
	if err != nil {
		return err
	}
	var failed []string
	done := 0
	for _, s := range stacks {
		if len(o.Only) > 0 && !slices.Contains(o.Only, s.Name) {
			continue
		}
		if s.Managed {
			fmt.Fprintf(o.Out, "· %s: already managed by OmegaCD, skipped\n", s.Name)
			continue
		}
		if defs[s.Name] {
			fmt.Fprintf(o.Out, "· %s: already defined in %s, skipped\n", s.Name, o.Definitions)
			continue
		}
		r, err := importStack(ctx, h, o, s)
		if err != nil {
			fmt.Fprintf(o.Out, "✗ %s: %v\n", s.Name, err)
			failed = append(failed, s.Name)
			continue
		}
		r.print(o.Out, o)
		done++
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

// report is what importing one stack did, for the summary.
type report struct {
	name, appDir    string
	services        int
	config, data    []string // "path" / "path → /abs/path"
	encrypted       []string // "file → file.sops.ext"
	moved           []string // "service: KEY"
	warnings        []string
	recreate        bool // the takeover recreates containers (they mount files that now come from Git)
	recipientsShort string
}

func (r report) print(w io.Writer, o Options) {
	verb := "→"
	if o.DryRun {
		verb = "would go to"
	}
	fmt.Fprintf(w, "✓ %s %s %s (%d service%s)\n", r.name, verb, r.appDir, r.services, map[bool]string{true: "", false: "s"}[r.services == 1])
	for _, c := range r.config {
		fmt.Fprintf(w, "    config   %s\n", c)
	}
	for _, d := range r.data {
		fmt.Fprintf(w, "    data     %s (stays on the host)\n", d)
	}
	for _, e := range r.encrypted {
		fmt.Fprintf(w, "    secret   %s, encrypted for %s\n", e, r.recipientsShort)
	}
	for _, m := range r.moved {
		fmt.Fprintf(w, "    moved    %s from the compose file into the encrypted .sops.env\n", m)
	}
	for _, x := range r.warnings {
		fmt.Fprintf(w, "    ! %s\n", x)
	}
	if r.recreate {
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

func importStack(ctx context.Context, h Host, o Options, s Stack) (report, error) {
	r := report{name: s.Name, appDir: path.Join(o.AppsDir, s.Name)}
	if !validName(s.Name) {
		return r, fmt.Errorf("%q is not a valid Application name (lowercase letters, digits, - and _)", s.Name)
	}
	if len(s.Files) == 0 {
		return r, errors.New("its compose files are not in its folder on the host")
	}
	dest := filepath.Join(o.Repo, filepath.FromSlash(r.appDir))
	if _, err := os.Stat(dest); err == nil && !o.Force {
		return r, fmt.Errorf("%s exists already (--force overwrites it)", r.appDir)
	}

	// 1. the compose files and .env, parsed as written
	files, err := h.Read(ctx, s.Dir, append(slices.Clone(s.Files), ".env"))
	if err != nil {
		return r, err
	}
	for _, f := range s.Files {
		if _, ok := files[f]; !ok {
			return r, fmt.Errorf("compose file %s not found in %s", f, s.Dir)
		}
	}
	p, err := loadProject(s, files)
	if err != nil {
		return r, fmt.Errorf("read compose files: %w", err)
	}
	r.services = len(p.Services) + len(p.DisabledServices)

	// 2. what the services mount and read from the stack's folder
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
			r.warnings = append(r.warnings, fmt.Sprintf("%s uses build: — OmegaCD only runs pre-built images; push the image to a registry and use image:", name))
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
		return r, err
	}

	// 3. decide: config into Git, data stays (absolute path), secrets encrypted
	out := map[string][]byte{}      // relative path in the app dir -> content
	absolute := map[string]string{} // mount source as cleaned -> absolute path on the host
	var toRead []string
	for _, rel := range rels {
		m, in := refs[rel], info[rel]
		switch {
		case strings.HasPrefix(rel, "../"):
			absolute[rel] = path.Join(s.Dir, rel)
		case m.env || m.secret || m.config:
			if in.Kind != 'f' {
				r.warnings = append(r.warnings, fmt.Sprintf("%s is referenced but not on the host", rel))
				continue
			}
			toRead = append(toRead, rel)
		case in.Kind == 'f' && in.Size <= MaxConfig, in.Kind == 'd' && m.readOnly && in.Size <= MaxConfig:
			toRead = append(toRead, rel)
		default: // a writable or big folder, a big file, or missing (Docker creates it): data
			absolute[rel] = path.Join(s.Dir, rel)
		}
	}
	read, err := h.Read(ctx, s.Dir, toRead)
	if err != nil {
		return r, err
	}
	for _, rel := range toRead {
		m := refs[rel]
		if !m.env && !m.secret && !m.config {
			r.recreate = true // a bind mount now points at OmegaCD's copy of the file
			r.config = append(r.config, rel)
			if secretFile(rel) {
				r.warnings = append(r.warnings, fmt.Sprintf("%s goes to Git as is and its name suggests a secret: check it before committing (or encrypt it as *.sops.* by hand)", rel))
			}
		} else if m.config {
			r.recreate = true // its content hash is new
		}
		if m.secret {
			r.recreate = true
		}
		for f, b := range read {
			if f == rel || strings.HasPrefix(f, rel+"/") {
				out[f] = b
			}
		}
	}
	for _, rel := range slices.Sorted(maps.Keys(absolute)) {
		r.data = append(r.data, fmt.Sprintf("%s → %s", rel, absolute[rel]))
	}

	// 4. secrets written in the compose files move into .env (filled in again with ${KEY})
	dotenvPlain := string(files[".env"])
	compose := map[string][]byte{}
	for _, f := range s.Files {
		b, moved, extra, warns, err := rewrite(files[f], absolute, envKeys(dotenvPlain))
		if err != nil {
			return r, fmt.Errorf("%s: %w", f, err)
		}
		compose[f] = b
		r.moved = append(r.moved, moved...)
		r.warnings = append(r.warnings, warns...)
		dotenvPlain += extra
	}
	if dotenvPlain != "" {
		out[".env"] = []byte(dotenvPlain)
	}
	maps.Copy(out, compose)

	// 5. encrypt env files with secrets, Compose secret files, and .env when it holds any
	var recipients []string
	encrypt := func(rel string) error {
		enc := sopsName(rel)
		if enc == "" {
			r.warnings = append(r.warnings, fmt.Sprintf("%s holds secrets but has no file extension, so it can't be encrypted as *.sops.*: left out — rename it (e.g. %s.env) and import again", rel, rel))
			delete(out, rel)
			return nil
		}
		if recipients == nil {
			var err error
			if recipients, err = o.Recipients(); err != nil {
				return err
			}
			r.recipientsShort = short(recipients)
		}
		b, err := secrets.Encrypt(out[rel], enc, recipients)
		if err != nil {
			return err
		}
		delete(out, rel)
		out[enc] = b
		r.encrypted = append(r.encrypted, fmt.Sprintf("%s → %s", rel, enc))
		return nil
	}
	for _, rel := range slices.Sorted(maps.Keys(out)) {
		m := refs[rel]
		needs := rel == ".env" && hasSecrets(string(out[rel])) || m != nil && (m.secret || m.env && hasSecrets(string(out[rel])))
		if needs {
			if err := encrypt(rel); err != nil {
				return r, err
			}
		}
	}

	// 6. write the app folder and append the definition
	composeFiles := s.Files
	if len(composeFiles) == 1 && composeFiles[0] == "compose.yaml" {
		composeFiles = nil
	}
	def, err := appdef.MarshalAdopting(domain.Application{
		Name: s.Name, RepoURL: o.RepoURL, Path: r.appDir, TargetRevision: o.Revision,
		ComposeFiles: composeFiles, Host: o.HostName,
	})
	if err != nil {
		return r, err
	}
	if o.DryRun {
		return r, nil
	}
	if o.Force {
		if err := os.RemoveAll(dest); err != nil {
			return r, err
		}
	}
	for _, rel := range slices.Sorted(maps.Keys(out)) {
		p := filepath.Join(dest, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return r, err
		}
		if err := os.WriteFile(p, out[rel], 0o644); err != nil {
			return r, err
		}
	}
	return r, appendDefinition(filepath.Join(o.Repo, filepath.FromSlash(o.Definitions)), def)
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
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
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

// rewrite turns relative data mounts into absolute host paths and moves secrets written in environment:
// into dotenv lines (returned in extra) referenced as ${KEY}. Unchanged files come back byte for byte.
func rewrite(src []byte, absolute map[string]string, known map[string]string) (out []byte, moved []string, extra string, warnings []string, err error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, nil, "", nil, err
	}
	changed := false
	services := mapValue(docRoot(&doc), "services")
	for i := 0; services != nil && i+1 < len(services.Content); i += 2 {
		name, svc := services.Content[i].Value, services.Content[i+1]
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
				abs, ok := absolute[path.Clean(strings.TrimPrefix(head, "./"))]
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
	if err := enc.Encode(&doc); err != nil {
		return nil, nil, "", nil, err
	}
	return []byte(b.String()), moved, extra, warnings, enc.Close()
}

func docRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	return doc
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
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

func envKeys(dotenvText string) map[string]string {
	m, err := dotenv.UnmarshalWithLookup(dotenvText, nil)
	if err != nil {
		return map[string]string{}
	}
	return m
}

func hasSecrets(dotenvText string) bool {
	for k, v := range envKeys(dotenvText) {
		if secretkey.Plain(k, v) {
			return true
		}
	}
	return false
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
