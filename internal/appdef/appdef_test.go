package appdef

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obleey/omegacd/internal/domain"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  []string
		err   string
	}{
		{"multi-doc and both extensions", map[string]string{
			"a.yaml": "name: web\nrepoURL: x\nprofiles: [prod]\nhost: local\n---\n---\nname: api\nrepoURL: x\nhost: local\n",
			"b.yml":  "name: db\nrepoURL: x\nadopt: true\nhost: local\n",
			"c.txt":  "not: yaml",
		}, []string{"api", "db", "web"}, ""},
		{"empty dir", map[string]string{}, nil, ""},
		{"unknown field", map[string]string{"a.yaml": "name: web\nrepoURL: x\nimage: nginx\n"}, nil, "field image not found"},
		{"no host", map[string]string{"a.yaml": "name: web\nrepoURL: x\n"}, nil, `"web" has no host:`},
		{"duplicate name", map[string]string{"a.yaml": "name: web\nrepoURL: x\nhost: local\n", "b.yaml": "name: web\nrepoURL: y\nhost: local\n"}, nil, `"web" is already declared in a.yaml`},
		{"nested", map[string]string{"a.yaml": "name: web\nkind: apps\nrepoURL: x\n"}, nil, "nested App of Apps"},
		{"unknown kind", map[string]string{"a.yaml": "name: web\nkind: helm\nrepoURL: x\n"}, nil, `unknown kind "helm"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for n, c := range tc.files {
				os.WriteFile(filepath.Join(dir, n), []byte(c), 0o600)
			}
			apps, err := Parse(dir)
			if tc.err != "" {
				if !errors.Is(err, domain.ErrInvalid) || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("want ErrInvalid containing %q, got %v", tc.err, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, a := range apps {
				names = append(names, a.Name)
				if a.Kind != domain.KindCompose {
					t.Errorf("%s: kind %q", a.Name, a.Kind)
				}
			}
			if strings.Join(names, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("got %v want %v", names, tc.want)
			}
		})
	}
}

// A folder or a definition that can't be read is the filesystem's error.
func TestParseUnreadable(t *testing.T) {
	dir := t.TempDir()
	if _, err := Parse(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing folder: %v", err)
	}
	os.Symlink("nowhere", filepath.Join(dir, "a.yaml"))
	if _, err := Parse(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a dangling a.yaml: %v", err)
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	a := domain.Application{Name: "web", Kind: domain.KindCompose, RepoURL: "x", Path: "apps/web", TargetRevision: "v1",
		ComposeFiles: []string{"compose.yaml"}, Profiles: []string{"prod"}, Host: "local", AutoSync: true, Prune: true, PruneVolumes: true, Adopt: true,
		WorkDir: "/srv/apps"}
	b, err := Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "kind") || strings.Contains(string(b), "adopt") {
		t.Fatalf("unexpected fields:\n%s", b)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "web.yaml"), b, 0o600)
	got, err := Parse(dir)
	if err != nil || len(got) != 1 || got[0].Name != a.Name || got[0].TargetRevision != a.TargetRevision || got[0].Profiles[0] != "prod" || got[0].Adopt || !got[0].PruneVolumes ||
		got[0].WorkDir != "/srv/apps" || !strings.Contains(string(b), "workDir: /srv/apps") {
		t.Fatalf("got %+v %v", got, err)
	}
}
