package cli

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/obleey/omegacd/internal/appdef"
	"github.com/obleey/omegacd/internal/domain"
)

func (c *CLI) appList(ctx context.Context, args []string) error {
	var o opts
	if _, err := parse(c.flags("app list", &o), args, 0, 0); err != nil {
		return err
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	var apps []domain.AppView
	if err := cl.do(ctx, "GET", "/api/apps", nil, &apps); err != nil {
		return err
	}
	if o.output == "json" {
		return c.printJSON(apps)
	}
	slices.SortFunc(apps, func(a, b domain.AppView) int { return strings.Compare(a.Name, b.Name) })
	names := map[string]bool{}
	for _, a := range apps {
		names[a.Name] = true
	}
	var rows [][]string
	var add func(a domain.AppView, indent string)
	add = func(a domain.AppView, indent string) {
		lastOp := "-"
		if op := a.LastOperation; op != nil {
			lastOp = string(op.Phase) + " " + ago(cmpTime(op.FinishedAt, op.StartedAt))
		}
		rev := a.TargetRevision
		if a.DeployedSHA != "" {
			rev += " (" + shortSHA(a.DeployedSHA) + ")"
		}
		rows = append(rows, []string{indent + a.Name, a.Kind, hostsOf(a, ","), string(a.SyncStatus), string(a.Health), rev, lastOp})
		for _, ch := range apps {
			if ch.Parent == a.Name {
				add(ch, indent+"  ")
			}
		}
	}
	for _, a := range apps {
		if a.Parent == "" || !names[a.Parent] {
			add(a, "")
		}
	}
	c.table([]string{"NAME", "KIND", "HOST", "SYNC", "HEALTH", "REVISION", "LAST-OP"}, rows, 3, 4)
	return nil
}

func cmpTime(a, b time.Time) time.Time {
	if a.IsZero() {
		return b
	}
	return a
}

func (c *CLI) getApp(ctx context.Context, cl *client, name string) (domain.AppView, error) {
	var v domain.AppView
	err := cl.do(ctx, "GET", appPath(name), nil, &v)
	return v, err
}

func (c *CLI) appGet(ctx context.Context, args []string) error {
	var o opts
	pos, err := parse(c.flags("app get", &o), args, 1, 1)
	if err != nil {
		return err
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	v, err := c.getApp(ctx, cl, pos[0])
	if err != nil {
		return err
	}
	if o.output == "json" {
		return c.printJSON(v)
	}
	c.printApp(v)
	return nil
}

func (c *CLI) printApp(v domain.AppView) {
	var policy []string
	for _, p := range []struct {
		on   bool
		name string
	}{{v.AutoSync, "auto-sync"}, {v.SelfHeal, "self-heal"}, {v.Prune, "prune"}, {v.PruneVolumes, "prune-volumes"}} {
		if p.on {
			policy = append(policy, p.name)
		}
	}
	sync := c.paint(statusColor(string(v.SyncStatus)), string(v.SyncStatus))
	if v.SyncMessage != "" {
		sync += " (" + v.SyncMessage + ")"
	}
	lines := [][2]string{
		{"Name", v.Name}, {"Kind", v.Kind}, {"Parent", v.Parent}, {"Repository", v.RepoURL}, {"Path", v.Path},
		{"Target revision", v.TargetRevision}, {"Compose files", strings.Join(v.ComposeFiles, ", ")},
		{"Profiles", strings.Join(v.Profiles, ", ")}, {hostsLabel(v), hostsOf(v, ", ")}, {"Sync policy", strings.Join(policy, ", ")},
		{"Sync status", sync}, {"Health", c.paint(statusColor(string(v.Health)), string(v.Health))},
		{"Target SHA", shortSHA(v.TargetSHA)}, {"Deployed SHA", shortSHA(v.DeployedSHA)},
	}
	if v.NotInGit {
		lines = append(lines, [2]string{"Note", "no longer declared by " + v.Parent})
	}
	if op := v.LastOperation; op != nil {
		lines = append(lines, [2]string{"Last operation", fmt.Sprintf("#%d %s (%s, %s): %s",
			op.ID, c.paint(statusColor(string(op.Phase)), string(op.Phase)), op.Trigger, ago(cmpTime(op.FinishedAt, op.StartedAt)), op.Message)})
	}
	for _, l := range lines {
		if l[1] != "" {
			fmt.Fprintf(c.Stdout, "%-16s %s\n", l[0]+":", l[1])
		}
	}
}

func (c *CLI) appCreate(ctx context.Context, args []string) error {
	var o opts
	var a domain.Application
	var files, profiles multiFlag
	var file string
	fs := c.flags("app create", &o)
	fs.StringVar(&file, "f", "", "create the Applications defined in this YAML file (- for stdin)")
	fs.StringVar(&a.RepoURL, "repo", "", "Repository URL")
	fs.StringVar(&a.Path, "path", "", "path inside the Repository")
	fs.StringVar(&a.TargetRevision, "revision", "", "branch, tag or commit (default main)")
	fs.Var(&files, "file", "compose file, relative to --path (repeatable)")
	fs.Var(&profiles, "profile", "Compose profile (repeatable)")
	fs.StringVar(&a.Host, "host", "", "Host to deploy to (default local)")
	fs.StringVar(&a.Kind, "kind", "", "compose (default) or apps (an App of Apps)")
	fs.BoolVar(&a.AutoSync, "auto-sync", false, "Sync automatically when the Repository changes")
	fs.BoolVar(&a.SelfHeal, "self-heal", false, "Sync automatically when the Live State drifts")
	fs.BoolVar(&a.Prune, "prune", false, "remove containers and networks no longer declared")
	fs.BoolVar(&a.PruneVolumes, "prune-volumes", false, "with --prune, also delete volumes no longer declared (their data is lost)")
	fs.BoolVar(&a.Adopt, "adopt", false, "take over an existing unmanaged Compose project")
	pos, err := parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	var apps []domain.Application
	switch {
	case file != "" && len(pos) == 0:
		var b []byte
		if file == "-" {
			b, err = io.ReadAll(c.Stdin)
		} else {
			b, err = os.ReadFile(file)
		}
		if err != nil {
			return err
		}
		if apps, err = appdef.Decode(file, b); err != nil {
			return err
		}
		if len(apps) == 0 {
			return fmt.Errorf("%s defines no Applications", file)
		}
	case file == "" && len(pos) == 1:
		if a.RepoURL == "" {
			return errors.New("--repo is required")
		}
		if a.PruneVolumes && !a.Prune {
			return errors.New("--prune-volumes requires --prune")
		}
		a.Name, a.ComposeFiles, a.Profiles = pos[0], files, profiles
		apps = []domain.Application{a}
	default:
		fs.Usage()
		return flag.ErrHelp
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	var created []domain.AppView
	for _, a := range apps {
		var v domain.AppView
		if err := cl.do(ctx, "POST", "/api/apps", a, &v); err != nil {
			return fmt.Errorf("application %s: %w", a.Name, err)
		}
		created = append(created, v)
		if o.output != "json" {
			fmt.Fprintf(c.Stdout, "application %s created\n", v.Name)
		}
	}
	if o.output == "json" {
		return c.printJSON(created)
	}
	return nil
}

func (c *CLI) appDelete(ctx context.Context, args []string) error {
	var o opts
	cascade, volumes, yes := true, false, false
	fs := c.flags("app delete", &o)
	fs.BoolVar(&cascade, "cascade", true, "also remove the Application's containers and networks (volumes are kept)")
	fs.BoolVar(&volumes, "volumes", false, "with --cascade, also delete the Application's volumes and all their data")
	fs.BoolVar(&yes, "yes", false, "do not ask for confirmation")
	fs.BoolVar(&yes, "y", false, "shorthand for --yes")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	name := pos[0]
	if volumes && !cascade {
		return errors.New("--volumes requires --cascade")
	}
	// a script must say --yes explicitly: deleting by accident in CI would tear down running services
	if !yes && !c.StdinTTY {
		return &exitError{1, "refusing to delete without confirmation: pass --yes when not running in a terminal"}
	}
	if !yes {
		what := "Application " + name
		switch {
		case volumes:
			what += " and its containers, networks and volumes (all volume data is permanently lost)"
		case cascade:
			what += " and its containers and networks"
		}
		fmt.Fprintf(c.Stderr, "Delete %s? [y/N] ", what)
		ans, _ := bufio.NewReader(c.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			return &exitError{1, "aborted"}
		}
	}
	q := "?cascade=" + strconv.FormatBool(cascade)
	if volumes {
		q += "&volumes=true"
	}
	if err := cl.do(ctx, "DELETE", appPath(name)+q, nil, nil); err != nil {
		return err
	}
	fmt.Fprintf(c.Stdout, "application %s deleted\n", name)
	return nil
}

type waitOpts struct {
	wait, noHealth bool
	timeout        time.Duration
}

func waitFlags(fs interface {
	BoolVar(*bool, string, bool, string)
	DurationVar(*time.Duration, string, time.Duration, string)
}, w *waitOpts) {
	fs.BoolVar(&w.wait, "wait", false, "wait until the Sync Operation finishes and the Application is Healthy")
	fs.BoolVar(&w.noHealth, "no-health", false, "with --wait, do not wait for Health")
	fs.DurationVar(&w.timeout, "timeout", 10*time.Minute, "with --wait, give up after this long")
}

func (c *CLI) appSync(ctx context.Context, args []string) error {
	var o opts
	var w waitOpts
	fs := c.flags("app sync", &o)
	waitFlags(fs, &w)
	prune := fs.Bool("prune", false, "also delete containers of services no longer declared, for this Sync only (volumes are kept unless the Application prunes volumes)")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	var body any
	if *prune {
		body = domain.SyncRequest{Prune: prune}
	}
	return c.startOp(ctx, cl, pos[0], "sync", body, w)
}

func (c *CLI) appRollback(ctx context.Context, args []string) error {
	var o opts
	var w waitOpts
	fs := c.flags("app rollback", &o)
	waitFlags(fs, &w)
	pos, err := parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	id, err := strconv.ParseInt(pos[1], 10, 64)
	if err != nil {
		return fmt.Errorf("OP_ID %q is not a number (see omegacd app history %s)", pos[1], pos[0])
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	return c.startOp(ctx, cl, pos[0], "rollback", map[string]int64{"opID": id}, w)
}

func (c *CLI) startOp(ctx context.Context, cl *client, name, action string, body any, w waitOpts) error {
	var res struct {
		OpID int64 `json:"opID"`
	}
	if err := cl.do(ctx, "POST", appPath(name, action), body, &res); err != nil {
		return err
	}
	fmt.Fprintf(c.Stdout, "%s: Sync Operation #%d started\n", name, res.OpID)
	if !w.wait {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	return c.timeout(ctx, w.timeout, c.waitOp(ctx, cl, name, res.OpID, !w.noHealth))
}

// timeout turns a deadline error into a readable one.
func (c *CLI) timeout(ctx context.Context, d time.Duration, err error) error {
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("timed out after %s", d)
	}
	return err
}

// waitOp waits until Sync Operation id is Succeeded or Failed, then (if health) until the Application,
// reconciled after the operation, is Healthy or Degraded.
func (c *CLI) waitOp(ctx context.Context, cl *client, name string, id int64, health bool) error {
	var op domain.SyncOperation
	var last string
	for {
		var ops []domain.SyncOperation
		if err := cl.do(ctx, "GET", appPath(name, "history"), nil, &ops); err != nil {
			return err
		}
		if i := slices.IndexFunc(ops, func(o domain.SyncOperation) bool { return o.ID == id }); i >= 0 {
			op = ops[i]
			if p := string(op.Phase); p != last {
				fmt.Fprintf(c.Stdout, "%s: Sync Operation #%d %s\n", name, id, c.paint(statusColor(p), p))
				last = p
			}
			if op.Phase != domain.OpRunning {
				break
			}
		}
		if err := c.sleep(ctx); err != nil {
			return err
		}
	}
	if op.Phase == domain.OpFailed {
		return errors.New(op.Message)
	}
	if !health {
		return nil
	}
	last = ""
	for {
		v, err := c.getApp(ctx, cl, name)
		if err != nil {
			return err
		}
		if !v.ReconciledAt.Before(op.FinishedAt) { // health computed after the sync, not before it
			if h := string(v.Health); h != last {
				fmt.Fprintf(c.Stdout, "%s: %s\n", name, c.paint(statusColor(h), h))
				last = h
			}
			switch v.Health {
			case domain.Healthy:
				return nil
			case domain.Degraded:
				return fmt.Errorf("%s is Degraded after Sync Operation #%d", name, id)
			}
		}
		if err := c.sleep(ctx); err != nil {
			return err
		}
	}
}

func (c *CLI) sleep(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(c.Poll):
		return nil
	}
}

func (c *CLI) appWait(ctx context.Context, args []string) error {
	var o opts
	var sync, health bool
	var timeout time.Duration
	fs := c.flags("app wait", &o)
	fs.BoolVar(&sync, "sync", false, "wait until Synced")
	fs.BoolVar(&health, "health", false, "wait until Healthy")
	fs.DurationVar(&timeout, "timeout", 10*time.Minute, "give up after this long")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	if !sync && !health {
		sync, health = true, true
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	last := ""
	err = func() error {
		for {
			v, err := c.getApp(ctx, cl, pos[0])
			if err != nil {
				return err
			}
			if s := string(v.SyncStatus) + " " + string(v.Health); s != last {
				fmt.Fprintf(c.Stdout, "%s: %s, %s\n", v.Name, c.paint(statusColor(string(v.SyncStatus)), string(v.SyncStatus)),
					c.paint(statusColor(string(v.Health)), string(v.Health)))
				last = s
			}
			if (!sync || v.SyncStatus == domain.Synced) && (!health || v.Health == domain.Healthy) {
				return nil
			}
			if err := c.sleep(ctx); err != nil {
				return err
			}
		}
	}()
	return c.timeout(ctx, timeout, err)
}

func (c *CLI) appRefresh(ctx context.Context, args []string) error {
	var o opts
	pos, err := parse(c.flags("app refresh", &o), args, 1, 1)
	if err != nil {
		return err
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	var v domain.AppView
	if err := cl.do(ctx, "POST", appPath(pos[0], "refresh"), nil, &v); err != nil {
		return err
	}
	if o.output == "json" {
		return c.printJSON(v)
	}
	c.printApp(v)
	return nil
}

func (c *CLI) appDiff(ctx context.Context, args []string) error {
	var o opts
	pos, err := parse(c.flags("app diff", &o), args, 1, 1)
	if err != nil {
		return err
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	var d domain.DiffView
	if err := cl.do(ctx, "GET", appPath(pos[0], "diff"), nil, &d); err != nil {
		return &exitError{2, err.Error()} // like diff(1): 2 means trouble
	}
	// undeclared networks and volumes (Kind set) are shown but, like in Sync Status, are not a difference
	differs := slices.ContainsFunc(d.Services, func(s domain.ServiceDiff) bool { return s.Kind == "" }) || strings.TrimSpace(d.GitDiff) != ""
	if o.output == "json" {
		if err := c.printJSON(d); err != nil {
			return err
		}
	} else {
		for _, s := range d.Services {
			mark, code := "~", yellow
			switch s.Change {
			case "added":
				mark, code = "+", green
			case "removed":
				mark, code = "-", red
			}
			line := mark + " " + strings.TrimLeft(s.Kind+" "+s.Service, " ")
			if len(s.Fields) > 0 {
				line += ": " + strings.Join(s.Fields, ", ")
			}
			fmt.Fprintln(c.Stdout, c.paint(code, line))
		}
		if d.GitDiff != "" {
			if len(d.Services) > 0 {
				fmt.Fprintln(c.Stdout)
			}
			fmt.Fprintln(c.Stdout, c.paint(grey, fmt.Sprintf("git diff %s..%s", cmp.Or(shortSHA(d.FromSHA), "(nothing deployed)"), shortSHA(d.ToSHA))))
			for _, l := range strings.Split(strings.TrimRight(d.GitDiff, "\n"), "\n") {
				code := ""
				switch {
				case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"), strings.HasPrefix(l, "diff "):
				case strings.HasPrefix(l, "+"):
					code = green
				case strings.HasPrefix(l, "-"):
					code = red
				case strings.HasPrefix(l, "@@"):
					code = cyan
				}
				fmt.Fprintln(c.Stdout, c.paint(code, l))
			}
		}
		if !differs {
			fmt.Fprintln(c.Stdout, "no differences")
		}
	}
	if differs {
		return &exitError{code: 1}
	}
	return nil
}

func (c *CLI) appHistory(ctx context.Context, args []string) error {
	var o opts
	pos, err := parse(c.flags("app history", &o), args, 1, 1)
	if err != nil {
		return err
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	var ops []domain.SyncOperation
	if err := cl.do(ctx, "GET", appPath(pos[0], "history"), nil, &ops); err != nil {
		return err
	}
	if o.output == "json" {
		return c.printJSON(ops)
	}
	var rows [][]string
	for _, op := range ops {
		dur := "-"
		if !op.FinishedAt.IsZero() {
			dur = op.FinishedAt.Sub(op.StartedAt).Round(time.Second).String()
		}
		rows = append(rows, []string{strconv.FormatInt(op.ID, 10), shortSHA(op.Revision), string(op.Phase), op.Trigger,
			ago(op.StartedAt), dur, strings.ReplaceAll(op.Message, "\n", " ")})
	}
	c.table([]string{"ID", "REVISION", "PHASE", "TRIGGER", "STARTED", "DURATION", "MESSAGE"}, rows, 2)
	return nil
}

func (c *CLI) appLogs(ctx context.Context, args []string) error {
	var o opts
	var follow bool
	tail := 200
	fs := c.flags("app logs", &o)
	fs.BoolVar(&follow, "f", false, "follow the log")
	fs.BoolVar(&follow, "follow", false, "follow the log")
	fs.IntVar(&tail, "tail", 200, "lines from the end of the log to show")
	pos, err := parse(fs, args, 1, 2)
	if err != nil {
		return err
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	var live domain.LiveState
	if err := cl.do(ctx, "GET", appPath(pos[0], "resources"), nil, &live); err != nil {
		return err
	}
	var names []string
	for _, ct := range live.Containers {
		names = append(names, ct.Name)
	}
	var ct *domain.Container
	switch {
	case len(pos) == 2:
		// exact container name or ID first, then the first container of a service
		for _, match := range []func(domain.Container) bool{
			func(x domain.Container) bool { return x.Name == pos[1] || x.ID == pos[1] },
			func(x domain.Container) bool { return x.Service == pos[1] },
		} {
			if i := slices.IndexFunc(live.Containers, match); i >= 0 && ct == nil {
				ct = &live.Containers[i]
			}
		}
		if ct == nil {
			return fmt.Errorf("no container or service %q in %s (containers: %s)", pos[1], pos[0], strings.Join(names, ", "))
		}
	case len(live.Containers) == 1:
		ct = &live.Containers[0]
	case len(live.Containers) == 0:
		return fmt.Errorf("%s has no containers", pos[0])
	default:
		return fmt.Errorf("%s has several containers; name one: %s", pos[0], strings.Join(names, ", "))
	}
	q := url.Values{"tail": {strconv.Itoa(tail)}, "follow": {strconv.FormatBool(follow)}}
	resp, err := cl.request(ctx, "GET", appPath(pos[0], "logs", url.PathEscape(ct.ID))+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var streamErr error
	err = sse(resp.Body, func(event, data string) bool {
		switch event {
		case "log":
			fmt.Fprintln(c.Stdout, data)
		case "error":
			streamErr = errors.New(data)
		case "end":
			return false
		}
		return true
	})
	if ctx.Err() != nil {
		return nil // interrupted while following
	}
	if streamErr != nil {
		return streamErr
	}
	return err
}

func (c *CLI) appExport(ctx context.Context, args []string) error {
	var o opts
	pos, err := parse(c.flags("app export", &o), args, 1, 1)
	if err != nil {
		return err
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	resp, err := cl.request(ctx, "GET", appPath(pos[0], "export"), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(c.Stdout, resp.Body)
	return err
}

func (c *CLI) hostList(ctx context.Context, args []string) error {
	var o opts
	if _, err := parse(c.flags("host list", &o), args, 0, 0); err != nil {
		return err
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	var hosts []domain.HostView
	if err := cl.do(ctx, "GET", "/api/settings/hosts", nil, &hosts); err != nil {
		return err
	}
	if o.output == "json" {
		return c.printJSON(hosts)
	}
	var rows [][]string
	for _, h := range hosts {
		u := h.URL
		if u == "" {
			u = "(docker socket)"
		}
		rows = append(rows, []string{h.Name, u, strconv.FormatBool(h.Status.Connected), h.Status.ServerVersion,
			strconv.Itoa(h.Status.Containers), cmp.Or(h.KeyRecipient, "(none)"), strings.Join(h.Apps, ", ")})
	}
	c.table([]string{"NAME", "URL", "CONNECTED", "VERSION", "CONTAINERS", "HOST KEY", "APPS"}, rows, 2)
	return nil
}

func (c *CLI) hostTest(ctx context.Context, args []string) error {
	var o opts
	pos, err := parse(c.flags("host test", &o), args, 1, 1)
	if err != nil {
		return err
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	var st domain.HostStatus
	if err := cl.do(ctx, "POST", "/api/settings/hosts/"+url.PathEscape(pos[0])+"/test", nil, &st); err != nil {
		return err
	}
	if o.output == "json" {
		if err := c.printJSON(st); err != nil {
			return err
		}
	} else if st.Connected {
		fmt.Fprintf(c.Stdout, "%s: connected, Docker %s (%s), %d containers\n", st.Name, st.ServerVersion, st.OS, st.Containers)
	}
	if !st.Connected {
		return fmt.Errorf("%s: %s", pos[0], cmp.Or(st.Error, "not connected"))
	}
	return nil
}

func hostKeysPath(host string) string { return "/api/settings/hosts/" + url.PathEscape(host) + "/keys" }

func (c *CLI) hostKeyShow(ctx context.Context, args []string) error {
	var o opts
	pos, err := parse(c.flags("host key show", &o), args, 1, 1)
	if err != nil {
		return err
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	var keys []domain.HostKey
	if err := cl.do(ctx, "GET", hostKeysPath(pos[0]), nil, &keys); err != nil {
		return err
	}
	if o.output == "json" {
		return c.printJSON(keys)
	}
	if len(keys) == 0 {
		return fmt.Errorf("Host %q has no Host Key — run: omegacd host key generate %s", pos[0], pos[0])
	}
	var rows [][]string
	for _, k := range keys {
		status, created := "previous", ""
		switch {
		case k.Master:
			status = "current (Master Key)"
		case k.Current:
			status = "current"
		}
		if !k.CreatedAt.IsZero() {
			created = k.CreatedAt.Local().Format(time.DateTime)
		}
		rows = append(rows, []string{k.Recipient, status, created})
	}
	c.table([]string{"RECIPIENT", "STATUS", "CREATED"}, rows)
	return nil
}

func (c *CLI) hostKeyGenerate(ctx context.Context, args []string) error {
	var o opts
	pos, err := parse(c.flags("host key generate", &o), args, 1, 1)
	if err != nil {
		return err
	}
	return c.addHostKey(ctx, o, pos[0], "generate", nil)
}

func (c *CLI) hostKeyImport(ctx context.Context, args []string) error {
	var o opts
	fs := c.flags("host key import", &o)
	file := fs.String("file", "", "file holding the age private key (AGE-SECRET-KEY-1...), - for stdin")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	if *file == "" {
		return errors.New("--file is required (the private key is never taken from the command line)")
	}
	var b []byte
	if *file == "-" {
		b, err = io.ReadAll(c.Stdin)
	} else {
		b, err = os.ReadFile(*file)
	}
	if err != nil {
		return err
	}
	return c.addHostKey(ctx, o, pos[0], "import", map[string]string{"privateKey": string(b)})
}

func (c *CLI) addHostKey(ctx context.Context, o opts, host, action string, body any) error {
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	var k domain.HostKey
	if err := cl.do(ctx, "POST", hostKeysPath(host)+"/"+action, body, &k); err != nil {
		return err
	}
	if o.output == "json" {
		return c.printJSON(k)
	}
	fmt.Fprintf(c.Stdout, "%s: current Host Key is now %s\nAdd it to .sops.yaml, run sops updatekeys on the Host's files, then remove the previous key.\n", host, k.Recipient)
	return nil
}

func (c *CLI) hostKeyRemove(ctx context.Context, args []string) error {
	var o opts
	pos, err := parse(c.flags("host key remove", &o), args, 2, 2)
	if err != nil {
		return err
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	return cl.do(ctx, "DELETE", hostKeysPath(pos[0])+"/"+url.PathEscape(pos[1]), nil, nil)
}

// hostsLabel names hostsOf for app get.
func hostsLabel(v domain.AppView) string {
	if v.Kind == domain.KindApps {
		return "Hosts"
	}
	return "Host"
}

// hostsOf is where an Application deploys: its Host, or for an App of Apps its children's Hosts ("-" without any).
func hostsOf(v domain.AppView, sep string) string {
	if v.Kind == domain.KindApps {
		return cmp.Or(strings.Join(v.Hosts, sep), "-")
	}
	return v.Host
}
