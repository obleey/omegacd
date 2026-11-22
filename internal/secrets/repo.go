package secrets

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/getsops/sops/v3"
	"github.com/getsops/sops/v3/cmd/sops/common"
	"github.com/getsops/sops/v3/config"
	"github.com/getsops/sops/v3/keyservice"
	"gopkg.in/yaml.v3"
)

// Source prepares a repository for one kind of Encrypted Secrets (Q29, Q295): the rules saying which keys a Host's
// secret files are encrypted for, and giving the files already there a rule's keys. SOPS is the one v0.2 ships.
type Source interface {
	// Rules are the rules OmegaCD wrote into the repository at dir, each for one Host.
	Rules(dir string) ([]Rule, error)
	// WriteRules writes rules into the repository at dir: each replaces the rule for the same Path, new ones go
	// before the others; other rules stay as they are (Q287).
	WriteRules(dir string, rules []Rule) error
	// Rekey gives every secret file under dir the keys of its rule, with the keys of the person running it (Q288).
	Rekey(dir string) (Rekeyed, error)
}

// Rule is which keys the secret files whose path (relative to the repository, slash-separated) matches Path, a
// regular expression, are encrypted for: Host's Host Key and the people's keys covering it.
type Rule struct {
	Host       string
	Path       string
	Recipients []string
}

// Rekeyed is what Rekey did, each a path relative to the repository.
type Rekeyed struct {
	Changed  []string
	CantOpen []string // encrypted only for keys the person doesn't hold (Q200)
	NoRule   []string // no rule matches it: left as it is
}

// Sources are the secret sources by name (omegacd secrets --source).
var Sources = map[string]Source{"sops": SOPS{}}

// SOPS keeps its rules in the repository's .sops.yaml, a creation rule each with a comment naming its Host.
type SOPS struct{}

const sopsConfig = ".sops.yaml"

// hostComment marks a rule OmegaCD wrote, so a rekey can find and refresh it (Q288).
const hostComment = "omegacd: Host "

func (SOPS) Rules(dir string) ([]Rule, error) {
	doc, rules, err := readSOPSConfig(dir)
	if err != nil || doc == nil {
		return nil, err
	}
	var out []Rule
	for _, n := range rules.Content {
		var host string
		for line := range strings.Lines(n.HeadComment) {
			if h, ok := strings.CutPrefix(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#")), hostComment); ok {
				host = h
			}
		}
		if host == "" {
			continue
		}
		r := Rule{Host: host, Path: value(n, "path_regex")}
		for _, k := range strings.Split(value(n, "age"), ",") {
			if k = strings.TrimSpace(k); k != "" {
				r.Recipients = append(r.Recipients, k)
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func (SOPS) WriteRules(dir string, rules []Rule) error {
	doc, list, err := readSOPSConfig(dir)
	if err != nil {
		return err
	}
	if doc == nil {
		list = &yaml.Node{Kind: yaml.SequenceNode}
		doc = &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode,
			Content: []*yaml.Node{{Kind: yaml.ScalarNode, Value: "creation_rules"}, list}}}}
	}
	var added []*yaml.Node
	for _, r := range rules {
		n := &yaml.Node{Kind: yaml.MappingNode, HeadComment: "# " + hostComment + r.Host, Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "path_regex"}, {Kind: yaml.ScalarNode, Value: r.Path},
			{Kind: yaml.ScalarNode, Value: "age"}, {Kind: yaml.ScalarNode, Value: strings.Join(r.Recipients, ",")}}}
		if i := slices.IndexFunc(list.Content, func(m *yaml.Node) bool { return value(m, "path_regex") == r.Path }); i >= 0 {
			list.Content[i] = n
		} else {
			added = append(added, n)
		}
	}
	list.Content = append(added, list.Content...)
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil { // coverage: unreachable, encoding nodes just read or built
		return err
	}
	return os.WriteFile(filepath.Join(dir, sopsConfig), b.Bytes(), 0o644)
}

// readSOPSConfig is the repository's .sops.yaml and its creation_rules, or nil when it has none.
func readSOPSConfig(dir string) (doc, rules *yaml.Node, err error) {
	b, err := os.ReadFile(filepath.Join(dir, sopsConfig))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	doc = &yaml.Node{}
	if err := yaml.Unmarshal(b, doc); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", sopsConfig, err)
	}
	if len(doc.Content) == 0 { // an empty file
		return nil, nil, nil
	}
	top := doc.Content[0]
	if top.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("%s: not a mapping", sopsConfig)
	}
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == "creation_rules" {
			if rules = top.Content[i+1]; rules.Kind != yaml.SequenceNode {
				return nil, nil, fmt.Errorf("%s: creation_rules is not a list", sopsConfig)
			}
			return doc, rules, nil
		}
	}
	rules = &yaml.Node{Kind: yaml.SequenceNode}
	top.Content = append(top.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "creation_rules"}, rules)
	return doc, rules, nil
}

// value is the scalar under key in the mapping n, "" if none.
func value(n *yaml.Node, key string) string {
	for i := 0; n.Kind == yaml.MappingNode && i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1].Value
		}
	}
	return ""
}

// Rekey is sops updatekeys on every *.sops.* file (Q288): the same data key and values, its rule's keys. The person's
// key is read where sops reads it (SOPS_AGE_KEY, SOPS_AGE_KEY_FILE, its keys file); a file already encrypted for
// exactly its rule's keys is left as it is.
func (SOPS) Rekey(dir string) (Rekeyed, error) {
	var out Rekeyed
	conf := filepath.Join(dir, sopsConfig)
	absDir, err := filepath.Abs(dir)
	if err != nil { // coverage: unreachable, the working directory exists while the command runs
		return out, err
	}
	svcs := []keyservice.KeyServiceClient{keyservice.NewLocalClient()}
	err = filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() && e.Name() == ".git" {
			return filepath.SkipDir
		}
		if !e.Type().IsRegular() || !Encrypted(e.Name()) {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		rule, err := config.LoadCreationRuleForFile(conf, filepath.Join(absDir, rel), nil)
		if err != nil || rule == nil || len(rule.KeyGroups) == 0 {
			out.NoRule = append(out.NoRule, rel)
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil { // coverage: unreachable, a file the walk just listed
			return err
		}
		store := common.StoreForFormat(formatFor(strings.Replace(e.Name(), ".sops.", ".", 1)), config.NewStoresConfig())
		tree, err := store.LoadEncryptedFile(data)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		if sameKeys(tree.Metadata.KeyGroups, rule.KeyGroups) {
			return nil
		}
		key, err := tree.Metadata.GetDataKeyWithKeyServices(svcs, nil)
		if err != nil {
			out.CantOpen = append(out.CantOpen, rel)
			return nil
		}
		tree.Metadata.KeyGroups = rule.KeyGroups
		if errs := tree.Metadata.UpdateMasterKeysWithKeyServices(key, svcs); len(errs) > 0 { // coverage: unreachable, age keys from a rule sops just parsed always wrap a key
			return fmt.Errorf("%s: %w", rel, errors.Join(errs...))
		}
		b, err := store.EmitEncryptedFile(tree)
		if err != nil { // coverage: unreachable, emitting a tree the same store just loaded
			return fmt.Errorf("%s: %w", rel, err)
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return err
		}
		out.Changed = append(out.Changed, rel)
		return nil
	})
	return out, err
}

// sameKeys reports whether two sets of key groups name the same keys, in any order.
func sameKeys(a, b []sops.KeyGroup) bool {
	return slices.Equal(groupKeys(a), groupKeys(b))
}

func groupKeys(groups []sops.KeyGroup) []string {
	var keys []string
	for _, g := range groups {
		for _, k := range g {
			keys = append(keys, k.ToString())
		}
	}
	slices.Sort(keys)
	return keys
}

// Recipients lists the keys an encrypted SOPS file is encrypted for, from its metadata: nothing is decrypted (Q293).
func Recipients(encrypted []byte, name string) ([]string, error) {
	tree, err := common.StoreForFormat(formatFor(strings.Replace(name, ".sops.", ".", 1)), config.NewStoresConfig()).LoadEncryptedFile(encrypted)
	if err != nil {
		return nil, fmt.Errorf("read the keys %s is encrypted for: %w", name, err)
	}
	return groupKeys(tree.Metadata.KeyGroups), nil
}
