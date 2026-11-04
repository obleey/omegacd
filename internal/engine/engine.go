// Package engine implements domain.Engine with the Docker Compose v5 SDK.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/config/configfile"
	"github.com/docker/cli/cli/config/credentials"
	clitypes "github.com/docker/cli/cli/config/types"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/obleey/dockercd/internal/domain"
)

var _ domain.Engine = (*Engine)(nil)

type Engine struct {
	cli     command.Cli
	compose api.Compose

	mu      sync.Mutex
	desired map[string][]string // app name -> service names of the last loaded project
}

// New connects to the Docker host the way the docker CLI does (DOCKER_HOST, contexts).
func New() (*Engine, error) {
	dcli, err := command.NewDockerCli()
	if err != nil {
		return nil, fmt.Errorf("docker cli: %w", err)
	}
	if err := dcli.Initialize(&flags.ClientOptions{}); err != nil {
		return nil, fmt.Errorf("docker cli: %w", err)
	}
	svc, err := newCompose(dcli, nil)
	if err != nil {
		return nil, err
	}
	return &Engine{cli: dcli, compose: svc, desired: map[string][]string{}}, nil
}

func (e *Engine) Close() error { return e.cli.Client().Close() }

func newCompose(c command.Cli, progress func(domain.ProgressEvent)) (api.Compose, error) {
	return compose.NewComposeService(c,
		compose.WithPrompt(compose.AlwaysOkPrompt()),
		compose.WithEventProcessor(&progressSink{fn: progress}))
}

func (e *Engine) load(ctx context.Context, app domain.Application, dir string) (*types.Project, error) {
	files := app.ComposeFiles
	if len(files) == 0 {
		files = []string{"compose.yaml"}
	}
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = filepath.Join(dir, f)
	}
	// Interpolation sees dir/.env only, never dockercd's process env (unlike compose's
	// default WithOsEnv): the Desired State must come from Git alone, and our env holds secrets.
	opts, err := cli.NewProjectOptions(paths,
		cli.WithWorkingDirectory(dir),
		cli.WithEnvFiles(),
		cli.WithDotEnv,
		cli.WithProfiles(app.Profiles),
		cli.WithName(app.Name))
	if err != nil {
		return nil, err
	}
	p, err := opts.LoadProject(ctx)
	if err != nil {
		return nil, err
	}
	p = p.WithoutUnnecessaryResources()
	// Same labels compose's own LoadProject sets, plus ours. CustomLabels are not hashed.
	for name, s := range p.Services {
		s.CustomLabels = map[string]string{
			api.ProjectLabel:     p.Name,
			api.ServiceLabel:     name,
			api.VersionLabel:     api.ComposeVersion,
			api.WorkingDirLabel:  p.WorkingDir,
			api.ConfigFilesLabel: strings.Join(p.ComposeFiles, ","),
			api.OneoffLabel:      "False",
			domain.ManagedLabel:  app.Name,
		}
		p.Services[name] = s
	}
	return p, nil
}

// remember caches the deployed project's services so Status can report Missing ones.
func (e *Engine) remember(app domain.Application, p *types.Project) {
	e.mu.Lock()
	e.desired[app.Name] = slices.Sorted(maps.Keys(p.Services))
	e.mu.Unlock()
}

func (e *Engine) Validate(ctx context.Context, app domain.Application, dir string) error {
	p, err := e.load(ctx, app, dir)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrInvalid, err)
	}
	return checkNoBuild(p)
}

// rebase rewrites hashed path fields of a project loaded from a staging dir as if it had been loaded from to.
func rebase(p *types.Project, from, to string) {
	re := func(path string) string {
		if rel, err := filepath.Rel(from, path); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join(to, rel)
		}
		return path
	}
	p.WorkingDir = re(p.WorkingDir)
	for name, s := range p.Services {
		for i := range s.EnvFiles {
			s.EnvFiles[i].Path = re(s.EnvFiles[i].Path)
		}
		for i := range s.Volumes {
			if s.Volumes[i].Type == types.VolumeTypeBind {
				s.Volumes[i].Source = re(s.Volumes[i].Source)
			}
		}
		p.Services[name] = s
	}
}

func checkNoBuild(p *types.Project) error {
	for _, name := range slices.Sorted(maps.Keys(p.Services)) {
		if p.Services[name].Build != nil {
			return fmt.Errorf("%w: service %q uses build:, only pre-built images are supported", domain.ErrInvalid, name)
		}
	}
	return nil
}

func (e *Engine) Diff(ctx context.Context, app domain.Application, dir, asDir string) ([]domain.ServiceDiff, error) {
	p, err := e.load(ctx, app, dir)
	if err != nil {
		return nil, err
	}
	if asDir != "" {
		rebase(p, dir, asDir)
	} else {
		e.remember(app, p)
	}
	ctrs, err := e.containers(ctx, app.Name)
	if err != nil {
		return nil, err
	}
	byService := map[string][]container.Summary{}
	for _, c := range ctrs {
		s := c.Labels[api.ServiceLabel]
		byService[s] = append(byService[s], c)
	}
	var diffs []domain.ServiceDiff
	for _, name := range slices.Sorted(maps.Keys(p.Services)) {
		svc := p.Services[name]
		live, ok := byService[name]
		if !ok {
			diffs = append(diffs, domain.ServiceDiff{Service: name, Change: "added"})
			continue
		}
		// shortcut: network_mode/ipc/pid "service:x" and use_api_socket are hashed by compose after resolution, so such services always show as changed.
		hash, err := serviceHash(svc)
		if err != nil {
			return nil, fmt.Errorf("hash service %q: %w", name, err)
		}
		if keepsRunning(svc.Restart) && slices.ContainsFunc(live, func(c container.Summary) bool { return c.State != "running" }) {
			diffs = append(diffs, domain.ServiceDiff{Service: name, Change: "stopped"})
			continue
		}
		for _, c := range live {
			if c.Labels[api.ConfigHashLabel] == hash {
				continue
			}
			field := "config"
			if c.Image != svc.Image {
				field = "image"
			}
			diffs = append(diffs, domain.ServiceDiff{Service: name, Change: "changed", Fields: []string{field}})
			break
		}
	}
	for _, name := range slices.Sorted(maps.Keys(byService)) {
		if _, ok := p.Services[name]; !ok {
			diffs = append(diffs, domain.ServiceDiff{Service: name, Change: "removed"})
		}
	}
	return diffs, nil
}

func (e *Engine) Sync(ctx context.Context, app domain.Application, dir string, opts domain.SyncOptions) error {
	p, err := e.load(ctx, app, dir)
	if err != nil {
		return fmt.Errorf("load compose project: %w", err)
	}
	if err := checkNoBuild(p); err != nil {
		return err
	}
	e.remember(app, p)
	svc, err := newCompose(authCLI{Cli: e.cli, cf: registryConfig(opts.Registries)}, opts.Progress)
	if err != nil {
		return err
	}
	if err := svc.Pull(ctx, p, api.PullOptions{}); err != nil {
		return fmt.Errorf("pull images: %w", err)
	}
	err = svc.Up(ctx, p, api.UpOptions{Create: api.CreateOptions{
		RemoveOrphans:        opts.Prune,
		Recreate:             api.RecreateDiverged,
		RecreateDependencies: api.RecreateDiverged,
	}})
	if err != nil {
		return fmt.Errorf("compose up: %w", err)
	}
	if opts.Prune {
		return e.pruneNetworks(ctx, p)
	}
	return nil
}

// pruneNetworks removes project networks no longer declared; compose up only prunes containers.
func (e *Engine) pruneNetworks(ctx context.Context, p *types.Project) error {
	nets, err := e.cli.Client().NetworkList(ctx, client.NetworkListOptions{Filters: projectFilter(p.Name)})
	if err != nil {
		return fmt.Errorf("list networks: %w", err)
	}
	var errs []error
	for _, n := range nets.Items {
		if _, ok := p.Networks[n.Labels[api.NetworkLabel]]; ok {
			continue
		}
		if _, err := e.cli.Client().NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{}); err != nil {
			errs = append(errs, fmt.Errorf("prune network %s: %w", n.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) Remove(ctx context.Context, app domain.Application) error {
	if err := e.compose.Down(ctx, app.Name, api.DownOptions{RemoveOrphans: true}); err != nil {
		return fmt.Errorf("compose down: %w", err)
	}
	e.mu.Lock()
	delete(e.desired, app.Name)
	e.mu.Unlock()
	return nil
}

// Status derives the desired services from the project last loaded for app (Validate/Diff/Sync).
// Until one was loaded in this process, it falls back to the services seen on containers, so Missing cannot show.
func (e *Engine) Status(ctx context.Context, app domain.Application) (domain.LiveState, error) {
	ctrs, err := e.containers(ctx, app.Name)
	if err != nil {
		return domain.LiveState{}, err
	}
	ls := domain.LiveState{Containers: []domain.Container{}, Resources: []domain.Resource{}, Health: domain.Healthy}
	seen := map[string]bool{}
	for _, c := range ctrs {
		insp, err := e.cli.Client().ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		if err != nil {
			return domain.LiveState{}, fmt.Errorf("inspect container %s: %w", c.ID, err)
		}
		st := insp.Container.State
		health := ""
		if st.Health != nil {
			health = string(st.Health.Status)
		}
		dc := domain.Container{
			ID:       c.ID,
			Name:     strings.TrimPrefix(insp.Container.Name, "/"),
			Service:  c.Labels[api.ServiceLabel],
			Image:    c.Image,
			State:    string(st.Status),
			Status:   c.Status,
			ExitCode: st.ExitCode,
			Health:   containerHealth(string(st.Status), health, st.ExitCode, insp.Container.RestartCount, keepsRunning(string(insp.Container.HostConfig.RestartPolicy.Name))),
		}
		ls.Containers = append(ls.Containers, dc)
		ls.Health = worst(ls.Health, dc.Health)
		seen[dc.Service] = true
	}
	e.mu.Lock()
	ls.Services = e.desired[app.Name]
	e.mu.Unlock()
	if ls.Services == nil {
		ls.Services = slices.Sorted(maps.Keys(seen))
	}
	for _, s := range ls.Services {
		if !seen[s] {
			ls.Health = worst(ls.Health, domain.Missing)
		}
	}
	if len(ctrs) == 0 {
		ls.Health = domain.Missing
	}

	nets, err := e.cli.Client().NetworkList(ctx, client.NetworkListOptions{Filters: projectFilter(app.Name)})
	if err != nil {
		return domain.LiveState{}, fmt.Errorf("list networks: %w", err)
	}
	for _, n := range nets.Items {
		ls.Resources = append(ls.Resources, domain.Resource{Kind: "network", Name: n.Name})
	}
	vols, err := e.cli.Client().VolumeList(ctx, client.VolumeListOptions{Filters: projectFilter(app.Name)})
	if err != nil {
		return domain.LiveState{}, fmt.Errorf("list volumes: %w", err)
	}
	for _, v := range vols.Items {
		ls.Resources = append(ls.Resources, domain.Resource{Kind: "volume", Name: v.Name})
	}
	return ls, nil
}

// keepsRunning reports whether a restart policy marks the service as long-running rather than a one-shot.
func keepsRunning(restartPolicy string) bool {
	return restartPolicy != "" && restartPolicy != "no"
}

// containerHealth maps docker container state to a Health Status. An exited 0 one-shot is Healthy,
// but a long-running service (one with a restart policy) that is not running is Degraded.
func containerHealth(state, health string, exitCode, restartCount int, longRunning bool) domain.HealthStatus {
	switch state {
	case "running":
		switch health {
		case "starting":
			return domain.Progressing
		case "unhealthy":
			return domain.Degraded
		}
		return domain.Healthy
	case "created", "removing":
		return domain.Progressing
	case "restarting":
		if restartCount == 0 {
			return domain.Progressing
		}
		return domain.Degraded
	case "exited":
		if exitCode == 0 && !longRunning {
			return domain.Healthy
		}
	}
	return domain.Degraded // exited != 0, dead, paused
}

var healthRank = map[domain.HealthStatus]int{domain.Healthy: 0, domain.Progressing: 1, domain.Missing: 2, domain.Degraded: 3}

func worst(a, b domain.HealthStatus) domain.HealthStatus {
	if healthRank[b] > healthRank[a] {
		return b
	}
	return a
}

func (e *Engine) ProjectExists(ctx context.Context, name string) (exists, managed bool, err error) {
	ctrs, err := e.containers(ctx, name)
	if err != nil {
		return false, false, err
	}
	for _, c := range ctrs {
		if _, ok := c.Labels[domain.ManagedLabel]; ok {
			managed = true
		}
	}
	return len(ctrs) > 0, managed, nil
}

func (e *Engine) Logs(ctx context.Context, containerID string, follow bool, tail int, w io.Writer) error {
	insp, err := e.cli.Client().ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return err
	}
	t := "all"
	if tail > 0 {
		t = strconv.Itoa(tail)
	}
	rc, err := e.cli.Client().ContainerLogs(ctx, containerID, client.ContainerLogsOptions{
		ShowStdout: true, ShowStderr: true, Timestamps: true, Follow: follow, Tail: t,
	})
	if err != nil {
		return err
	}
	defer rc.Close()
	if insp.Container.Config != nil && insp.Container.Config.Tty {
		_, err = io.Copy(w, rc)
	} else {
		_, err = stdcopy.StdCopy(w, w, rc)
	}
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// containers lists the project's service containers, stopped ones included, one-off `run` containers excluded.
func (e *Engine) containers(ctx context.Context, project string) ([]container.Summary, error) {
	res, err := e.cli.Client().ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: projectFilter(project).Add("label", api.OneoffLabel+"=False"),
	})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	return res.Items, nil
}

func projectFilter(project string) client.Filters {
	return make(client.Filters).Add("label", api.ProjectLabel+"="+project)
}

// authCLI hands compose a config file holding only dockercd's registry credentials.
type authCLI struct {
	command.Cli
	cf *configfile.ConfigFile
}

func (c authCLI) ConfigFile() *configfile.ConfigFile { return c.cf }

// registryConfig keys credentials the way the docker CLI config looks them up per image domain.
func registryConfig(regs []domain.RegistryCredential) *configfile.ConfigFile {
	cf := configfile.New("")
	for _, r := range regs {
		key := credentials.ConvertToHostname(r.Registry)
		if key == "docker.io" || key == "index.docker.io" || key == "registry-1.docker.io" {
			key = "https://index.docker.io/v1/"
		}
		cf.AuthConfigs[key] = clitypes.AuthConfig{Username: r.Username, Password: r.Password, ServerAddress: key}
	}
	return cf
}

type progressSink struct {
	mu sync.Mutex
	fn func(domain.ProgressEvent)
}

func (p *progressSink) Start(context.Context, string) {}
func (p *progressSink) Done(string, bool)             {}

func (p *progressSink) On(events ...api.Resource) {
	if p.fn == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ev := range events {
		if ev.ParentID != "" { // per-layer pull progress
			continue
		}
		status := ev.Text
		if status == "" {
			status = ev.StatusText()
		}
		p.fn(domain.ProgressEvent{Resource: ev.ID, Status: status, Text: ev.Details})
	}
}
