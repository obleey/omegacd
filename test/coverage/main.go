// Command coverage merges Go coverage profiles from every kind of test and checks them (Q96, Q150): unit and
// integration (go test -coverprofile) and e2e (the binary built with go build -cover, its counters turned into a
// profile with go tool covdata textfmt). Generated protobuf code is left out. The totals are over reachable code: a
// block marked in the code as unreachable, with a comment holding "coverage: unreachable" and the reason, is left out
// of them. With -all a block no test reaches and not marked fails the check: every reachable block covered is 100%.
//
//	go run ./test/coverage [-all] PROFILE...
package main

import (
	"bufio"
	"cmp"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
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

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run is the command; it returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("coverage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("all", false, "fail when any block is uncovered and not marked")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: coverage [-all] PROFILE...")
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
	r := check(blocks, readLines)
	r.print(stdout)
	if *all && len(r.uncovered) > 0 {
		fmt.Fprintf(stdout, "\nFAIL: %d uncovered block(s) without a %q marker:\n", len(r.uncovered), marker)
		for _, b := range r.uncovered {
			fmt.Fprintf(stdout, "  %s:%d-%d\n", b.file, b.start, b.end)
		}
		return 1
	}
	return 0
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

type report struct {
	pkgs      map[string]count // reachable statements per package, and those covered
	uncovered []block          // reachable, and no test reaches them
	marked    int              // blocks marked unreachable
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
	fmt.Fprintf(w, "%6.1f%%  total of reachable code (unit + integration + e2e); %d uncovered block(s), %d marked unreachable\n",
		r.total(), len(r.uncovered), r.marked)
}

// check sums the reachable statements per package (hits: statements of covered blocks) and finds the uncovered
// blocks; lines reads a file's lines, for the markers.
func check(blocks map[block]count, lines func(string) []string) report {
	r := report{pkgs: map[string]count{}}
	for _, b := range slices.SortedFunc(maps.Keys(blocks), func(a, b block) int {
		return cmp.Or(strings.Compare(a.file, b.file), a.start-b.start, a.startCol-b.startCol)
	}) {
		c := blocks[b]
		if c.hits == 0 && c.stmts > 0 && markedUnreachable(lines(b.file), b) {
			r.marked++
			continue
		}
		pc := r.pkgs[path.Dir(b.file)]
		pc.stmts += c.stmts
		if c.hits > 0 {
			pc.hits += c.stmts
		} else if c.stmts > 0 {
			r.uncovered = append(r.uncovered, b)
		}
		r.pkgs[path.Dir(b.file)] = pc
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
