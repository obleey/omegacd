// Package appdef reads and writes Application definitions: the YAML an App of Apps holds in Git
// and the export endpoint produces.
package appdef

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/obleey/omegacd/internal/domain"
)

type def struct {
	Name           string   `yaml:"name"`
	Kind           string   `yaml:"kind,omitempty"`
	RepoURL        string   `yaml:"repoURL"`
	Path           string   `yaml:"path,omitempty"`
	TargetRevision string   `yaml:"targetRevision,omitempty"`
	ComposeFiles   []string `yaml:"composeFiles,omitempty"`
	Profiles       []string `yaml:"profiles,omitempty"`
	Host           string   `yaml:"host,omitempty"`
	AutoSync       bool     `yaml:"autoSync"`
	SelfHeal       bool     `yaml:"selfHeal"`
	Prune          bool     `yaml:"prune"`
	PruneVolumes   bool     `yaml:"pruneVolumes,omitempty"`
	Adopt          bool     `yaml:"adopt,omitempty"`
}

// Marshal writes a's settings as a definition; Adopt and runtime state are left out.
func Marshal(a domain.Application) ([]byte, error) { return marshal(a, false) }

// MarshalAdopting is Marshal with adopt: true, for definitions that take over running stacks (omegacd import).
func MarshalAdopting(a domain.Application) ([]byte, error) { return marshal(a, true) }

func marshal(a domain.Application, adopt bool) ([]byte, error) {
	d := def{a.Name, a.Kind, a.RepoURL, a.Path, a.TargetRevision, a.ComposeFiles, a.Profiles, a.Host, a.AutoSync, a.SelfHeal, a.Prune, a.PruneVolumes, adopt}
	if d.Kind == domain.KindCompose {
		d.Kind = ""
	}
	return yaml.Marshal(d)
}

// Parse reads every *.yaml and *.yml file directly in dir; a file may hold several documents.
// The Applications are not normalized.
func Parse(dir string) ([]domain.Application, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var apps []domain.Application
	seen := map[string]string{}
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if e.IsDir() || (ext != ".yaml" && ext != ".yml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		defs, err := Decode(e.Name(), b)
		if err != nil {
			return nil, err
		}
		for _, d := range defs {
			if prev, ok := seen[d.Name]; ok {
				return nil, fmt.Errorf("%w: %s: application %q is already declared in %s", domain.ErrInvalid, e.Name(), d.Name, prev)
			}
			seen[d.Name] = e.Name()
			if d.Kind == domain.KindApps {
				return nil, fmt.Errorf("%w: %s: application %q: nested App of Apps is not supported", domain.ErrInvalid, e.Name(), d.Name)
			}
			apps = append(apps, d)
		}
	}
	slices.SortFunc(apps, func(a, b domain.Application) int { return strings.Compare(a.Name, b.Name) })
	return apps, nil
}

// Decode reads the definitions in b, one per YAML document; source names b in errors.
// An App of Apps (kind: apps) is allowed here.
func Decode(source string, b []byte) ([]domain.Application, error) {
	var apps []domain.Application
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	for {
		var d def
		if err := dec.Decode(&d); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", domain.ErrInvalid, source, err)
		}
		if reflect.ValueOf(d).IsZero() {
			continue // empty document
		}
		d.Name = strings.TrimSpace(d.Name)
		switch d.Kind {
		case "":
			d.Kind = domain.KindCompose
		case domain.KindCompose, domain.KindApps:
		default:
			return nil, fmt.Errorf("%w: %s: application %q: unknown kind %q", domain.ErrInvalid, source, d.Name, d.Kind)
		}
		apps = append(apps, domain.Application{
			Name: d.Name, Kind: d.Kind, RepoURL: d.RepoURL, Path: d.Path, TargetRevision: d.TargetRevision,
			ComposeFiles: d.ComposeFiles, Profiles: d.Profiles, Host: d.Host,
			AutoSync: d.AutoSync, SelfHeal: d.SelfHeal, Prune: d.Prune, PruneVolumes: d.PruneVolumes, Adopt: d.Adopt,
		})
	}
	return apps, nil
}
