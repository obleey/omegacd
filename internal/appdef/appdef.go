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

	"github.com/obleey/dockercd/internal/domain"
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
	Adopt          bool     `yaml:"adopt,omitempty"`
}

// Marshal writes a's settings as a definition; Adopt and runtime state are left out.
func Marshal(a domain.Application) ([]byte, error) {
	d := def{a.Name, a.Kind, a.RepoURL, a.Path, a.TargetRevision, a.ComposeFiles, a.Profiles, a.Host, a.AutoSync, a.SelfHeal, a.Prune, false}
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
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		for {
			var d def
			if err := dec.Decode(&d); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return nil, fmt.Errorf("%w: %s: %v", domain.ErrInvalid, e.Name(), err)
			}
			if reflect.ValueOf(d).IsZero() {
				continue // empty document
			}
			d.Name = strings.TrimSpace(d.Name)
			if prev, ok := seen[d.Name]; ok {
				return nil, fmt.Errorf("%w: %s: application %q is already declared in %s", domain.ErrInvalid, e.Name(), d.Name, prev)
			}
			seen[d.Name] = e.Name()
			switch d.Kind {
			case "", domain.KindCompose:
			case domain.KindApps:
				return nil, fmt.Errorf("%w: %s: application %q: nested App of Apps is not supported", domain.ErrInvalid, e.Name(), d.Name)
			default:
				return nil, fmt.Errorf("%w: %s: application %q: unknown kind %q", domain.ErrInvalid, e.Name(), d.Name, d.Kind)
			}
			apps = append(apps, domain.Application{
				Name: d.Name, Kind: domain.KindCompose, RepoURL: d.RepoURL, Path: d.Path, TargetRevision: d.TargetRevision,
				ComposeFiles: d.ComposeFiles, Profiles: d.Profiles, Host: d.Host,
				AutoSync: d.AutoSync, SelfHeal: d.SelfHeal, Prune: d.Prune, Adopt: d.Adopt,
			})
		}
	}
	slices.SortFunc(apps, func(a, b domain.Application) int { return strings.Compare(a.Name, b.Name) })
	return apps, nil
}
