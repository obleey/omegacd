package main

import (
	"bytes"
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

// A block counts as covered when any profile covers it; generated code is left out; the total and each package show.
func TestMergeAndTotal(t *testing.T) {
	unit := "mode: atomic\n" + m + "internal/a/a.go:3.1,5.2 2 1\n" + m + "internal/a/a.go:7.1,9.2 2 0\n" +
		m + "internal/proto/agent/v1/agent.pb.go:1.1,2.2 50 0\n" + "golang.org/x/other/x.go:1.1,2.2 9 0\n"
	e2e := "mode: set\n" + m + "internal/a/a.go:7.1,9.2 2 1\n" + m + "cmd/omegacd/main.go:1.1,4.2 4 0\n\n"
	var out bytes.Buffer
	if code := run(profiles(t, unit, e2e), &out, &out); code != 0 {
		t.Fatalf("code %d:\n%s", code, out.String())
	}
	for _, want := range []string{"  0.0%  cmd/omegacd", "100.0%  internal/a", " 50.0%  total of reachable code", "1 uncovered block(s), 0 marked"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "proto") || strings.Contains(out.String(), "golang.org") {
		t.Fatalf("generated or foreign code counted:\n%s", out.String())
	}
}

// With -all a block no test reaches fails unless it is marked unreachable (Q150); a marked block is out of the total,
// so covering the rest is 100%.
func TestAllAndMarked(t *testing.T) {
	p := profiles(t, "mode: set\n"+m+"internal/a/a.go:3.1,5.2 2 0\n"+m+"internal/a/a.go:7.1,9.2 1 1\n"+
		m+"internal/a/a.go:11.1,11.2 0 0\n")
	var out bytes.Buffer
	if code := run(p, &out, &out); code != 0 {
		t.Fatalf("without -all a report only: code %d\n%s", code, out.String())
	}
	out.Reset()
	if code := run(append([]string{"-all"}, p...), &out, &out); code != 1 || !strings.Contains(out.String(), "internal/a/a.go:3-5") ||
		strings.Contains(out.String(), "a.go:7-9") {
		t.Fatalf("-all: code %d\n%s", code, out.String())
	}

	lines := func(string) []string {
		return []string{"package a", "", "if err != nil { // coverage: unreachable, a fixed struct always encodes", "\treturn err", "}"}
	}
	r := check(map[block]count{{file: "internal/a/a.go", start: 3, end: 5}: {stmts: 1}, {file: "internal/a/a.go", start: 7, end: 9}: {stmts: 3, hits: 1}}, lines)
	if r.marked != 1 || len(r.uncovered) != 0 || r.total() != 100 {
		t.Fatalf("a marked block: %+v, total %.1f", r, r.total())
	}
	if markedUnreachable(lines(""), block{start: 9, end: 12}) {
		t.Fatal("marked past the end of the file")
	}
}

func TestUsageAndBadInput(t *testing.T) {
	bad := profiles(t, "not a profile\n")
	for name, args := range map[string][]string{
		"no profiles":   nil,
		"bad flag":      {"-nope"},
		"missing file":  {filepath.Join(t.TempDir(), "none")},
		"not a profile": bad,
	} {
		var out bytes.Buffer
		if code := run(args, &out, &out); code != 2 {
			t.Errorf("%s: code %d\n%s", name, code, out.String())
		}
	}
	if (report{}).total() != 100 {
		t.Fatal("no statements count as covered")
	}
}

func TestReadLines(t *testing.T) {
	f := filepath.Join(t.TempDir(), "a.go")
	os.WriteFile(f, []byte("a\nb\n"), 0o644)
	if l := readLines(f); len(l) != 3 || l[1] != "b" {
		t.Fatalf("lines %q", l)
	}
}
