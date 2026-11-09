// Command coverage merges Go coverage profiles from every kind of test and checks them (Q96): unit and integration
// (go test -coverprofile) and e2e (the binary built with go build -cover, its counters turned into a profile with go
// tool covdata textfmt). Generated protobuf code is left out. A block no test reaches fails the check when a line of
// it changed against the base (when -base is given), or when -all is given, unless it is marked in the code as
// unreachable with a comment holding "coverage: unreachable" and the reason; the total must not drop below -floor.
//
//	go run ./test/coverage [-base REF] [-all] [-floor FILE] PROFILE...
package main

import (
	"bufio"
	"cmp"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	module = "github.com/obleey/omegacd/"
	marker = "coverage: unreachable"
)

// excluded are generated packages: nothing to test in them.
var excluded = []string{module + "internal/proto/"}

// block is a range of statements in a file, as profiles name it.
type block struct {
	file           string // relative to the module, e.g. internal/agent/agent.go
	start, end     int    // lines
	startCol, eCol int
}

type count struct{ stmts, hits int }

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, gitDiff)) }

// run is the command; diff gives the changed lines against a base. It returns the exit code.
func run(args []string, stdout, stderr io.Writer, diff func(base string) (string, error)) int {
	fs := flag.NewFlagSet("coverage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("base", "", "a git ref: fail when a block with a line changed since it is uncovered and not marked")
	all := fs.Bool("all", false, "fail when any block is uncovered and not marked")
	floor := fs.String("floor", "", "a file holding the lowest total percentage allowed")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: coverage [-base REF] [-all] [-floor FILE] PROFILE...")
		return 2
	}
	blocks := map[block]count{}
	for _, p := range fs.Args() {
		f, err := os.Open(p)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		err = merge(blocks, f)
		f.Close()
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", p, err)
			return 2
		}
	}
	var changed map[string][]int
	if *base != "" {
		d, err := diff(*base)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		changed = changedLines(d)
	}
	r := check(blocks, changed, *all, readLines)
	r.print(stdout)
	code := 0
	if len(r.failing) > 0 {
		fmt.Fprintf(stdout, "\nFAIL: %d uncovered block(s) without a %q marker:\n", len(r.failing), marker)
		for _, b := range r.failing {
			fmt.Fprintf(stdout, "  %s:%d-%d\n", b.file, b.start, b.end)
		}
		code = 1
	}
	if *floor != "" {
		b, err := os.ReadFile(*floor)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		min, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", *floor, err)
			return 2
		}
		if r.total() < min {
			fmt.Fprintf(stdout, "\nFAIL: total %.1f%% is below the floor %.1f%% (%s)\n", r.total(), min, *floor)
			code = 1
		}
	}
	return code
}

var line = regexp.MustCompile(`^(\S+):(\d+)\.(\d+),(\d+)\.(\d+) (\d+) (\d+)$`)

// merge adds a profile's blocks to blocks: a block is covered if any profile covers it.
func merge(blocks map[block]count, r io.Reader) error {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		t := sc.Text()
		if t == "" || strings.HasPrefix(t, "mode:") {
			continue
		}
		m := line.FindStringSubmatch(t)
		if m == nil {
			return fmt.Errorf("not a coverage profile line: %q", t)
		}
		if !strings.HasPrefix(m[1], module) || slices.ContainsFunc(excluded, func(p string) bool { return strings.HasPrefix(m[1], p) }) {
			continue
		}
		n := func(i int) int { v, _ := strconv.Atoi(m[i]); return v }
		b := block{file: strings.TrimPrefix(m[1], module), start: n(2), startCol: n(3), end: n(4), eCol: n(5)}
		c := blocks[b]
		c.stmts = n(6)
		c.hits += n(7)
		blocks[b] = c
	}
	return sc.Err()
}

var hunk = regexp.MustCompile(`^@@ -\S+ \+(\d+)(?:,(\d+))? @@`)

// changedLines are the new lines of each non-test Go file in a unified diff (git diff -U0).
func changedLines(diff string) map[string][]int {
	out := map[string][]int{}
	file := ""
	for _, l := range strings.Split(diff, "\n") {
		if f, ok := strings.CutPrefix(l, "+++ b/"); ok {
			file = f
			if !strings.HasSuffix(f, ".go") || strings.HasSuffix(f, "_test.go") {
				file = ""
			}
			continue
		}
		if m := hunk.FindStringSubmatch(l); m != nil && file != "" {
			start, _ := strconv.Atoi(m[1])
			n := 1
			if m[2] != "" {
				n, _ = strconv.Atoi(m[2])
			}
			for i := range n {
				out[file] = append(out[file], start+i)
			}
		}
	}
	return out
}

type report struct {
	pkgs      map[string]count
	uncovered int     // blocks no test reaches and not marked
	marked    int     // blocks marked unreachable
	failing   []block // uncovered, not marked, and checked (changed, or -all)
}

func (r report) total() float64 {
	var all count
	for _, c := range r.pkgs {
		all.stmts += c.stmts
		all.hits += c.hits
	}
	return percent(all)
}

func percent(c count) float64 {
	if c.stmts == 0 {
		return 100
	}
	return 100 * float64(c.hits) / float64(c.stmts)
}

func (r report) print(w io.Writer) {
	for _, p := range slices.Sorted(maps.Keys(r.pkgs)) {
		fmt.Fprintf(w, "%6.1f%%  %s\n", percent(r.pkgs[p]), p)
	}
	fmt.Fprintf(w, "%6.1f%%  total (unit + integration + e2e); %d uncovered block(s), %d marked unreachable\n",
		r.total(), r.uncovered, r.marked)
}

// check sums statements per package (hits: statements of covered blocks) and finds the uncovered blocks; lines reads a
// file's lines, for the markers.
func check(blocks map[block]count, changed map[string][]int, all bool, lines func(string) []string) report {
	r := report{pkgs: map[string]count{}}
	for _, b := range slices.SortedFunc(maps.Keys(blocks), func(a, b block) int {
		return cmp.Or(strings.Compare(a.file, b.file), a.start-b.start, a.startCol-b.startCol)
	}) {
		c := blocks[b]
		pc := r.pkgs[path.Dir(b.file)]
		pc.stmts += c.stmts
		if c.hits > 0 {
			pc.hits += c.stmts
		}
		r.pkgs[path.Dir(b.file)] = pc
		if c.hits > 0 || c.stmts == 0 {
			continue
		}
		if markedUnreachable(lines(b.file), b) {
			r.marked++
			continue
		}
		r.uncovered++
		if all || slices.ContainsFunc(changed[b.file], func(l int) bool { return l >= b.start && l <= b.end }) {
			r.failing = append(r.failing, b)
		}
	}
	return r
}

// markedUnreachable reports whether a line of b carries the marker (the block of an if starts on the if's line).
func markedUnreachable(lines []string, b block) bool {
	for i := b.start; i <= b.end && i <= len(lines); i++ {
		if strings.Contains(lines[i-1], marker) {
			return true
		}
	}
	return false
}

func readLines(file string) []string {
	b, _ := os.ReadFile(file)
	return strings.Split(string(b), "\n")
}

// gitDiff is the diff of the branch since it left base (base...HEAD).
func gitDiff(base string) (string, error) {
	out, err := exec.Command("git", "diff", "-U0", base+"...HEAD", "--", "*.go").Output()
	if err != nil {
		return "", fmt.Errorf("git diff %s...HEAD: %w", base, err)
	}
	return string(out), nil
}
