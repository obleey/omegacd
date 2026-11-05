package domain

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestComposeFilesIn(t *testing.T) {
	for _, tc := range []struct {
		named   []string
		present []string
		want    []string
	}{
		{nil, []string{"compose.yaml", "compose.yml"}, []string{"compose.yaml"}},
		{nil, []string{"compose.yml", "docker-compose.yml"}, []string{"compose.yml"}},
		{nil, []string{"docker-compose.yaml", "docker-compose.yml"}, []string{"docker-compose.yaml"}},
		{nil, []string{"docker-compose.yml"}, []string{"docker-compose.yml"}},
		{nil, nil, []string{"compose.yaml"}}, // none there: errors name the first default
		{[]string{"base.yaml", "prod.yaml"}, []string{"compose.yaml"}, []string{"base.yaml", "prod.yaml"}},
	} {
		dir := t.TempDir()
		for _, f := range tc.present {
			if err := os.WriteFile(filepath.Join(dir, f), []byte("services: {}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if got := (Application{ComposeFiles: tc.named}).ComposeFilesIn(dir); !slices.Equal(got, tc.want) {
			t.Errorf("named %v, present %v: got %v, want %v", tc.named, tc.present, got, tc.want)
		}
	}
	// a directory named like a compose file is not one
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "compose.yaml"), 0o700)
	os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services: {}\n"), 0o600)
	if got := (Application{}).ComposeFilesIn(dir); !slices.Equal(got, []string{"compose.yml"}) {
		t.Errorf("dir named compose.yaml: got %v", got)
	}
}
