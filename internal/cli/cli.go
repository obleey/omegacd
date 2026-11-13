// Package cli is the omegacd command-line client. It talks to a omegacd server over its HTTP API
// with an API token.
package cli

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/obleey/omegacd/internal/importer"
	"github.com/obleey/omegacd/internal/logging"
	"github.com/obleey/omegacd/internal/source"
	"github.com/obleey/omegacd/internal/version"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"filippo.io/age"
)

// CLI holds the process environment, so tests can fake it.
type CLI struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	StdoutTTY      bool          // colors, colorized diffs
	StdinTTY       bool          // confirmation prompts
	Poll           time.Duration // --wait polling interval
	// ImportHost connects omegacd import to this machine's Docker engine; nil uses importer.Dial.
	ImportHost func() (importer.Host, error)
}

// Main runs the CLI with the real process environment and returns the exit code.
func Main(args []string) int {
	c := &CLI{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv,
		StdoutTTY: isTerminal(os.Stdout), StdinTTY: isTerminal(os.Stdin), Poll: 2 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return c.Run(ctx, args)
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// exitError carries a non-1 exit code, or a silent exit (msg "").
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

var errUnauthorized = errors.New("not logged in or token invalid — run omegacd login")

type command struct {
	usage string
	run   func(c *CLI, ctx context.Context, args []string) error
}

var commands map[string]command

func init() { // init breaks the cycle commands → run funcs → flags → commands
	commands = map[string]command{
		"login":         {"login --server URL --token TOKEN", (*CLI).login},
		"logout":        {"logout", (*CLI).logout},
		"version":       {"version", (*CLI).version},
		"keygen":        {"keygen [--out FILE]   (a new age key like age-keygen; the public key also goes to stderr)", (*CLI).keygen},
		"source-certs":  {"source-certs [--out DIR] [--host NAME]...   (mTLS certificates for a standalone Source Server; --host: the names the Control Plane dials it by)", (*CLI).sourceCerts},
		"import":        {"import --host NAME [STACK]... [--recipient age1…] [--apps-dir apps] [--definitions platform/apps.yaml] [--dry-run] [--force]\n    (run on the Host, in your repository checkout: writes the Compose stacks running on this machine into it as Applications for Host NAME)", (*CLI).importStacks},
		"app list":      {"app list", (*CLI).appList},
		"app get":       {"app get NAME", (*CLI).appGet},
		"app create":    {"app create NAME --repo URL [--path P] [--revision R] [--file F]... [--profile P]... [--host H] [--kind apps] [--auto-sync] [--self-heal] [--prune [--prune-volumes]] [--adopt]\n  app create -f app.yaml", (*CLI).appCreate},
		"app delete":    {"app delete NAME [--cascade=false | --volumes | --forget] [--yes]", (*CLI).appDelete},
		"app sync":      {"app sync NAME [--prune] [--wait] [--no-health] [--timeout 10m]", (*CLI).appSync},
		"app refresh":   {"app refresh NAME", (*CLI).appRefresh},
		"app diff":      {"app diff NAME", (*CLI).appDiff},
		"app history":   {"app history NAME", (*CLI).appHistory},
		"app rollback":  {"app rollback NAME OP_ID [--wait] [--no-health] [--timeout 10m]", (*CLI).appRollback},
		"app wait":      {"app wait NAME [--sync] [--health] [--timeout 10m]", (*CLI).appWait},
		"app logs":      {"app logs NAME [CONTAINER_OR_SERVICE] [-f] [--tail N]", (*CLI).appLogs},
		"app export":    {"app export NAME", (*CLI).appExport},
		"host list":     {"host list", (*CLI).hostList},
		"host add":      {"host add NAME   (a Host reached through an Agent: prints the command that enrolls it, with a single-use invite valid an hour; again: a new invite)", (*CLI).hostAdd},
		"host approve":  {"host approve NAME FINGERPRINT   (after comparing it with the fingerprint the Agent printed)", (*CLI).hostApprove},
		"host rotate":   {"host rotate NAME   (the Host's connected Agent makes a new signing key; prints its fingerprint)", (*CLI).hostRotate},
		"host allow":    {"host allow NAME [CIDR]...   (the addresses its Agent may connect from, e.g. 203.0.113.7 or 10.0.0.0/8; none: anywhere)", (*CLI).hostAllow},
		"host unlock":   {"host unlock NAME   (a Host locked by two connections with its key: when the second had a harmless cause)", (*CLI).hostUnlock},
		"host delete":   {"host delete NAME   (revokes its Agent's key and closes its connection; a key that may be stolen: delete, then add the Host again)", (*CLI).hostDelete},
		"host key show": {"host key show HOST", (*CLI).hostKeyShow},
	}
}

var order = []string{"login", "logout", "version", "keygen", "source-certs", "import", "app list", "app get", "app create", "app delete", "app sync", "app refresh",
	"app diff", "app history", "app rollback", "app wait", "app logs", "app export", "host list", "host add", "host approve", "host rotate", "host allow", "host unlock", "host delete",
	"host key show"}

func (c *CLI) usage() {
	fmt.Fprintln(c.Stderr, "Usage: omegacd [server]        run the server\n       omegacd source-server  run a standalone Source Server\n       omegacd agent          run the Agent on this Host (omegacd agent --help: its settings)\n       omegacd COMMAND        talk to a server\n\nCommands:")
	for _, k := range order {
		fmt.Fprintln(c.Stderr, "  "+commands[k].usage)
	}
	fmt.Fprintln(c.Stderr, "\nEvery command takes --server URL, --token TOKEN (or OMEGACD_SERVER, OMEGACD_TOKEN) and -o table|json.")
}

// Run runs one CLI command and returns the exit code.
func (c *CLI) Run(ctx context.Context, args []string) int {
	name, rest := "", args
	if len(args) > 0 {
		name, rest = args[0], args[1:]
		if (name == "app" || name == "host") && len(rest) > 0 {
			name, rest = name+" "+rest[0], rest[1:]
		}
		if name == "host key" && len(rest) > 0 {
			name, rest = name+" "+rest[0], rest[1:]
		}
	}
	cmd, ok := commands[name]
	if !ok {
		c.usage()
		if name == "help" || name == "-h" || name == "--help" {
			return 0
		}
		if name != "" {
			fmt.Fprintf(c.Stderr, "\nunknown command %q\n", strings.Join(args[:min(len(args), 2)], " "))
		}
		return 2
	}
	err := cmd.run(c, ctx, rest)
	var ee *exitError
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 2
	case errors.As(err, &ee):
		if ee.msg != "" {
			fmt.Fprintln(c.Stderr, "error: "+ee.msg)
		}
		return ee.code
	}
	fmt.Fprintln(c.Stderr, "error: "+err.Error())
	return 1
}

// ---- flags ----

type opts struct {
	server, token, output string
}

func (c *CLI) flags(name string, o *opts) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(c.Stderr)
	fs.Usage = func() { fmt.Fprintln(c.Stderr, "Usage: omegacd "+commands[name].usage); fs.PrintDefaults() }
	fs.StringVar(&o.server, "server", "", "omegacd server URL")
	fs.StringVar(&o.token, "token", "", "API token")
	fs.StringVar(&o.output, "output", "table", "output format: table or json")
	fs.StringVar(&o.output, "o", "table", "shorthand for --output")
	return fs
}

// parse parses flags anywhere among the positional args and checks the positional count.
func parse(fs *flag.FlagSet, args []string, minPos, maxPos int) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if args = fs.Args(); len(args) == 0 {
			break
		}
		pos, args = append(pos, args[0]), args[1:]
	}
	if len(pos) < minPos || len(pos) > maxPos {
		fs.Usage()
		return nil, flag.ErrHelp
	}
	if o := fs.Lookup("output").Value.String(); o != "table" && o != "json" {
		return nil, fmt.Errorf("unknown output format %q (want table or json)", o)
	}
	return pos, nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// ---- config ----

type config struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

func (c *CLI) configPath() (string, error) {
	dir := c.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home := c.Getenv("HOME")
		if home == "" {
			var err error
			if home, err = os.UserHomeDir(); err != nil {
				return "", err
			}
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "omegacd", "config.json"), nil
}

// client resolves server and token: flag, then environment, then the config file.
func (c *CLI) client(o opts) (*client, error) {
	var cfg config
	if p, err := c.configPath(); err == nil {
		if b, err := os.ReadFile(p); err == nil {
			if err := json.Unmarshal(b, &cfg); err != nil {
				return nil, fmt.Errorf("%s: %v", p, err)
			}
		}
	}
	server := first(o.server, c.Getenv("OMEGACD_SERVER"), cfg.Server)
	token := first(o.token, c.Getenv("OMEGACD_TOKEN"), cfg.Token)
	if server == "" || token == "" {
		return nil, errUnauthorized
	}
	// warn unless asked: the CLI talks to people, its output is what they read
	log, err := logging.New(c.Stderr, cmp.Or(c.Getenv("OMEGACD_LOG_LEVEL"), "warn"), c.Getenv("OMEGACD_LOG_FORMAT"))
	if err != nil {
		return nil, err
	}
	return &client{server: strings.TrimRight(server, "/"), token: token, log: log.With("component", "cli")}, nil
}

func first(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func (c *CLI) login(ctx context.Context, args []string) error {
	var o opts
	fs := c.flags("login", &o)
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	cl, err := c.client(o)
	if err != nil {
		return errors.New("--server and --token are required")
	}
	var me struct {
		Username string `json:"username"`
	}
	if err := cl.do(ctx, "GET", "/api/auth/me", nil, &me); err != nil {
		return err
	}
	p, err := c.configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(config{cl.server, cl.token}, "", "  ")
	if err := os.WriteFile(p, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(p, 0o600); err != nil { // WriteFile keeps the mode of an existing file
		return err
	}
	fmt.Fprintf(c.Stdout, "Logged in to %s as %s; saved to %s\n", cl.server, me.Username, p)
	return nil
}

func (c *CLI) logout(ctx context.Context, args []string) error {
	var o opts
	if _, err := parse(c.flags("logout", &o), args, 0, 0); err != nil {
		return err
	}
	p, err := c.configPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Fprintln(c.Stdout, "Logged out")
	return nil
}

func (c *CLI) version(ctx context.Context, args []string) error {
	fmt.Fprintln(c.Stdout, "omegacd "+version.String())
	return nil
}

// keygen writes a new age identity in age-keygen's format, so installing OmegaCD needs no age tools.
// It works offline and never overwrites a file: losing the Master Key loses every sealed credential.
func (c *CLI) keygen(ctx context.Context, args []string) error {
	var o opts
	fs := c.flags("keygen", &o)
	out := fs.String("out", "", "write the key to FILE (mode 0600) instead of stdout")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return err
	}
	key := fmt.Sprintf("# created: %s\n# public key: %s\n%s\n", time.Now().Format(time.RFC3339), id.Recipient(), id)
	if *out == "" {
		_, err = io.WriteString(c.Stdout, key)
	} else {
		var f *os.File
		if f, err = os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
			return err
		}
		if _, err = f.WriteString(key); err == nil {
			err = f.Close()
		} else {
			f.Close()
		}
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(c.Stderr, "Public key: "+id.Recipient().String())
	return nil
}

// sourceCerts writes the CA, server and client certificates for omegacd source-server and the Control Plane.
func (c *CLI) sourceCerts(ctx context.Context, args []string) error {
	var o opts
	var hosts multiFlag
	fs := c.flags("source-certs", &o)
	out := fs.String("out", ".", "directory to write ca.pem, server.pem/.key and client.pem/.key into")
	fs.Var(&hosts, "host", "a name or address the Source Server is reached by (repeatable; default localhost and 127.0.0.1)")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	if len(hosts) == 0 {
		hosts = multiFlag{"localhost", "127.0.0.1"}
	}
	if err := source.GenerateCerts(*out, hosts); err != nil {
		return err
	}
	fmt.Fprintf(c.Stderr, "Wrote %s: give the Source Server ca.pem, server.pem and server.key; the Control Plane ca.pem, client.pem and client.key (OMEGACD_SOURCE_TLS_DIR on both)\n", *out)
	return nil
}

// ---- HTTP client ----

type client struct {
	server, token string
	log           *slog.Logger
}

func (cl *client) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, cl.server+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cl.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	cl.log.DebugContext(ctx, "api", "method", method, "path", path, "status", resp.StatusCode, "duration", time.Since(start))
	if resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errUnauthorized
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return nil, errors.New(e.Error)
	}
	return nil, fmt.Errorf("%s %s: %s", method, path, resp.Status)
}

// do sends body as JSON and decodes the response into out (if not nil).
func (cl *client) do(ctx context.Context, method, path string, body, out any) error {
	resp, err := cl.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func appPath(name string, rest ...string) string {
	return "/api/apps/" + strings.Join(append([]string{url.PathEscape(name)}, rest...), "/")
}

// sse calls fn for each event of the stream until it ends or fn returns false.
func sse(r io.Reader, fn func(event, data string) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 2<<20)
	var event string
	var data []string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if event != "" || data != nil {
				if !fn(cmp.Or(event, "message"), strings.Join(data, "\n")) {
					return nil
				}
			}
			event, data = "", nil
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			d := strings.TrimPrefix(line, "data:")
			data = append(data, strings.TrimPrefix(d, " "))
		}
	}
	return sc.Err()
}

// ---- output ----

const (
	red    = "31"
	green  = "32"
	yellow = "33"
	cyan   = "36"
	grey   = "90"
)

func (c *CLI) color() bool { return c.StdoutTTY && c.Getenv("NO_COLOR") == "" }

func (c *CLI) paint(code, s string) string {
	if !c.color() || code == "" || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func statusColor(s string) string {
	switch s {
	case "Synced", "Healthy", "Succeeded", "true":
		return green
	case "OutOfSync", "Progressing", "Running":
		return yellow
	case "Degraded", "Failed", "Missing", "false":
		return red
	}
	return ""
}

// table prints rows with aligned columns; cells are colored after padding so escapes don't break alignment.
func (c *CLI) table(header []string, rows [][]string, colorCols ...int) {
	widths := make([]int, len(header))
	for _, r := range append([][]string{header}, rows...) {
		for i, cell := range r {
			widths[i] = max(widths[i], utf8.RuneCountInString(cell))
		}
	}
	colored := map[int]bool{}
	for _, i := range colorCols {
		colored[i] = true
	}
	for n, r := range append([][]string{header}, rows...) {
		var b strings.Builder
		for i, cell := range r {
			pad := ""
			if i < len(r)-1 {
				pad = strings.Repeat(" ", widths[i]-utf8.RuneCountInString(cell)+2)
			}
			if n > 0 && colored[i] {
				cell = c.paint(statusColor(cell), cell)
			}
			b.WriteString(cell + pad)
		}
		fmt.Fprintln(c.Stdout, strings.TrimRight(b.String(), " "))
	}
}

func (c *CLI) printJSON(v any) error {
	enc := json.NewEncoder(c.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func shortSHA(s string) string { return s[:min(len(s), 7)] }

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
