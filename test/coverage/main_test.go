package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const m = "github.com/obleey/omegacd/"

// profiles writes each profile to a file and returns their paths.
func profiles(t *testing.T, ps ...string) []string {
	t.Helper()
	var out []string
	for i, p := range ps {
		f := filepath.Join(t.TempDir(), "p"+string(rune('a'+i))+".out")
		if err := os.WriteFile(f, []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

func noDiff(string) (string, error) { return "", nil }

// A block counts as covered when any profile covers it; generated code is left out; the total and each package show.
func TestMergeAndTotal(t *testing.T) {
	unit := "mode: atomic\n" + m + "internal/a/a.go:3.1,5.2 2 1\n" + m + "internal/a/a.go:7.1,9.2 2 0\n" +
		m + "internal/proto/agent/v1/agent.pb.go:1.1,2.2 50 0\n" + "golang.org/x/other/x.go:1.1,2.2 9 0\n"
	e2e := "mode: set\n" + m + "internal/a/a.go:7.1,9.2 2 1\n" + m + "cmd/omegacd/main.go:1.1,4.2 4 0\n\n"
	var out bytes.Buffer
	if code := run(profiles(t, unit, e2e), &out, &out, noDiff); code != 0 {
		t.Fatalf("code %d:\n%s", code, out.String())
	}
	for _, want := range []string{"  0.0%  cmd/omegacd", "100.0%  internal/a", " 50.0%  total", "1 uncovered block(s), 0 marked"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "proto") || strings.Contains(out.String(), "golang.org") {
		t.Fatalf("generated or foreign code counted:\n%s", out.String())
	}
}

// A block no test reaches fails when a line of it changed (or with -all), unless it is marked unreachable.
func TestChangedAndMarked(t *testing.T) {
	p := profiles(t, "mode: set\n"+m+"internal/a/a.go:3.1,5.2 2 0\n"+m+"internal/a/a.go:7.1,9.2 1 0\n"+
		m+"internal/a/a.go:11.1,11.2 0 0\n")
	diff := func(base string) (string, error) {
		if base != "main" {
			t.Fatalf("base %q", base)
		}
		return "diff --git a/internal/a/a.go b/internal/a/a.go\n+++ b/internal/a/a.go\n@@ -4 +4 @@\n-x\n+y\n@@ -20 +20,2 @@\n" +
			"+++ b/internal/a/a_test.go\n@@ -1 +3,2 @@\n+++ b/README.md\n@@ -1 +7 @@\n", nil
	}
	var out bytes.Buffer
	if code := run(append([]string{"-base", "main"}, p...), &out, &out, diff); code != 1 ||
		!strings.Contains(out.String(), "internal/a/a.go:3-5") || strings.Contains(out.String(), "a.go:7-9") {
		t.Fatalf("a changed uncovered block: code %d\n%s", code, out.String())
	}
	out.Reset()
	if code := run(append([]string{"-all"}, p...), &out, &out, noDiff); code != 1 || !strings.Contains(out.String(), "a.go:7-9") {
		t.Fatalf("-all: code %d\n%s", code, out.String())
	}

	lines := func(string) []string {
		return []string{"package a", "", "if err != nil { // coverage: unreachable, a fixed struct always encodes", "\treturn err", "}"}
	}
	r := check(map[block]count{{file: "internal/a/a.go", start: 3, end: 5}: {stmts: 1}}, nil, true, lines)
	if r.marked != 1 || r.uncovered != 0 || len(r.failing) != 0 {
		t.Fatalf("a marked block: %+v", r)
	}
	if markedUnreachable(lines(""), block{start: 9, end: 12}) {
		t.Fatal("marked past the end of the file")
	}
}

func TestFloor(t *testing.T) {
	p := profiles(t, "mode: set\n"+m+"internal/a/a.go:3.1,5.2 1 1\n"+m+"internal/a/a.go:7.1,9.2 1 0\n")
	floor := filepath.Join(t.TempDir(), "floor")
	var out bytes.Buffer
	os.WriteFile(floor, []byte("60.0\n"), 0o644)
	if code := run(append([]string{"-floor", floor}, p...), &out, &out, noDiff); code != 1 || !strings.Contains(out.String(), "below the floor 60.0%") {
		t.Fatalf("below: code %d\n%s", code, out.String())
	}
	os.WriteFile(floor, []byte("50"), 0o644)
	if code := run(append([]string{"-floor", floor}, p...), &out, &out, noDiff); code != 0 {
		t.Fatalf("at the floor: code %d", code)
	}
}

func TestUsageAndBadInput(t *testing.T) {
	good := profiles(t, "mode: set\n")
	bad := profiles(t, "not a profile\n")
	floor := filepath.Join(t.TempDir(), "floor")
	os.WriteFile(floor, []byte("high"), 0o644)
	for name, tc := range map[string]struct {
		args []string
		diff func(string) (string, error)
	}{
		"no profiles":   {nil, noDiff},
		"bad flag":      {[]string{"-nope"}, noDiff},
		"missing file":  {[]string{filepath.Join(t.TempDir(), "none")}, noDiff},
		"not a profile": {bad, noDiff},
		"diff fails":    {append([]string{"-base", "x"}, good...), func(string) (string, error) { return "", errors.New("no git") }},
		"missing floor": {append([]string{"-floor", filepath.Join(t.TempDir(), "none")}, good...), noDiff},
		"floor not a %": {append([]string{"-floor", floor}, good...), noDiff},
	} {
		var out bytes.Buffer
		if code := run(tc.args, &out, &out, tc.diff); code != 2 {
			t.Errorf("%s: code %d\n%s", name, code, out.String())
		}
	}
	if (report{}).total() != 100 {
		t.Fatal("no statements count as covered")
	}
}

func TestGitAndFiles(t *testing.T) {
	if _, err := gitDiff("HEAD"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitDiff("no-such-ref-at-all"); err == nil {
		t.Fatal("a diff against a ref that doesn't exist")
	}
	f := filepath.Join(t.TempDir(), "a.go")
	os.WriteFile(f, []byte("a\nb\n"), 0o644)
	if l := readLines(f); len(l) != 3 || l[1] != "b" {
		t.Fatalf("lines %q", l)
	}
}
