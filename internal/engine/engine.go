// Package engine implements domain.Engine with the Docker Compose v5 SDK.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/config/configfile"
	"github.com/docker/cli/cli/config/credentials"
	clitypes "github.com/docker/cli/cli/config/types"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"

	"github.com/obleey/omegacd/internal/domain"
)

var _ domain.Engine = (*Engine)(nil)

type Engine struct {
	cfg Config

	mu      sync.Mutex
	conns   map[string]*hostConn // host name -> connection, created lazily (local in New)
	desired map[string][]string  // app name -> service names of the last loaded project
}

// New connects to the local Docker host the way the docker CLI does (DOCKER_HOST, contexts).
// Remote Hosts are connected on first use.
func New(cfg Config) (*Engine, error) {
	if cfg.NoLocalHost {
		return &Engine{cfg: cfg, conns: map[string]*hostConn{}, desired: map[string][]string{}}, nil
	}
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
	return &Engine{
		cfg:     cfg,
		conns:   map[string]*hostConn{domain.LocalHost: {host: domain.Host{Name: domain.LocalHost}, cli: dcli, compose: svc}},
		desired: map[string][]string{},
	}, nil
}

func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var errs []error
	for name, hc := range e.conns {
		errs = append(errs, hc.cli.Client().Close())
		delete(e.conns, name)
	}
	return errors.Join(errs...)
}

func newCompose(c command.Cli, progress func(domain.ProgressEvent)) (api.Compose, error) {
	return compose.NewComposeService(c,
		compose.WithPrompt(compose.AlwaysOkPrompt()),
		compose.WithEventProcessor(&progressSink{fn: progress}))
}

func (e *Engine) load(ctx context.Context, app domain.Application, dir string) (*types.Project, error) {
	p, err := loadProject(ctx, app, dir, true)
	if err != nil {
		return nil, err
	}
	// Same labels compose's own LoadProject sets, plus ours. CustomLabels are not hashed.
	for name, s := range p.Services {
		s.CustomLabels = map[string]string{
			api.ProjectLabel:      p.Name,
			api.ServiceLabel:      name,
			api.VersionLabel:      api.ComposeVersion,
			api.WorkingDirLabel:   p.WorkingDir,
			api.ConfigFilesLabel:  strings.Join(p.ComposeFiles, ","),
			api.OneoffLabel:       "False",
			domain.ManagedLabel:   app.Name,
			domain.ManagedByLabel: domain.ManagedByValue,
		}
		p.Services[name] = s
	}
	if err := addContentHashes(p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadProject(ctx context.Context, app domain.Application, dir string, resolvePaths bool) (*types.Project, error) {
	files := app.ComposeFilesIn(dir)
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = filepath.Join(dir, f)
	}
	// Interpolation sees dir/.env only, never omegacd's process env (unlike compose's
	// default WithOsEnv): the Desired State must come from Git alone, and our env holds secrets.
	opts, err := cli.NewProjectOptions(paths,
		cli.WithWorkingDirectory(dir),
		cli.WithEnvFiles(),
		cli.WithDotEnv,
		cli.WithProfiles(app.Profiles),
		cli.WithName(app.Name),
		cli.WithResolvedPaths(resolvePaths),
		// unresolved env_file paths would be read relative to omegacd's own cwd; that read only needs paths
		cli.WithLoadOptions(func(o *loader.Options) { o.SkipResolveEnvironment = !resolvePaths }))
	if err != nil {
		return nil, err
	}
	p, err := opts.LoadProject(ctx)
	if err != nil {
		return nil, err
	}
	return p.WithoutUnnecessaryResources(), nil
}

// contentHashLabel carries a hash of the files a service reads but compose does not hash:
// file-based configs and secrets, and bind-mounted regular files inside the project dir.
// Labels are part of the config hash, so a content change recreates the container.
const contentHashLabel = "dev.omegacd.content-hash"

func addContentHashes(p *types.Project) error {
	for name, s := range p.Services {
		h := sha256.New()
		n := 0
		add := func(kind, id, file, content string) error {
			if file != "" {
				b, err := os.ReadFile(file)
				if err != nil {
					return fmt.Errorf("service %q %s %q: %w", name, kind, id, err)
				}
				content = string(b)
			}
			fmt.Fprintf(h, "%s\x00%s\x00%d\x00%s", kind, id, len(content), content)
			n++
			return nil
		}
		for _, c := range s.Configs {
			if obj, ok := p.Configs[c.Source]; ok && (obj.File != "" || obj.Content != "") {
				if err := add("config", c.Source+":"+c.Target, obj.File, obj.Content); err != nil {
					return err
				}
			}
		}
		for _, c := range s.Secrets {
			if obj, ok := p.Secrets[c.Source]; ok && (obj.File != "" || obj.Content != "") {
				if err := add("secret", c.Source+":"+c.Target, obj.File, obj.Content); err != nil {
					return err
				}
			}
		}
		for _, v := range s.Volumes {
			if v.Type != types.VolumeTypeBind || !insideDir(p.WorkingDir, v.Source) {
				continue
			}
			// only regular files: a bind-mounted directory may hold runtime data that changes constantly
			if fi, err := os.Stat(v.Source); err != nil || !fi.Mode().IsRegular() {
				continue
			}
			if err := add("bind", v.Target, v.Source, ""); err != nil {
				return err
			}
		}
		if n == 0 {
			continue // no label at all, so services without such files keep compose's plain hash
		}
		if s.Labels == nil {
			s.Labels = types.Labels{}
		}
		s.Labels[contentHashLabel] = hex.EncodeToString(h.Sum(nil))
		p.Services[name] = s
	}
	return nil
}

func insideDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
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
	if err := checkNoBuild(p); err != nil {
		return err
	}
	return checkBindSources(ctx, app, dir)
}

// rebase rewrites path fields of a project loaded from dir from as if it had been loaded from dir to:
// a staging dir to the app's working dir, or the local working dir to the app dir on a remote Host.
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
	for name, c := range p.Configs {
		if c.File != "" {
			c.File = re(c.File)
			p.Configs[name] = c
		}
	}
	for name, c := range p.Secrets {
		if c.File != "" {
			c.File = re(c.File)
			p.Secrets[name] = c
		}
	}
}

// placeFor rebases p, loaded from dir, to where its files are on the app's Host: asDir (if set) locally,
// the remote app dir on any other Host. Content hashes were computed from the local files already.
func placeFor(p *types.Project, hc *hostConn, app domain.Application, dir, asDir string) {
	switch {
	case hc.host.Name != domain.LocalHost:
		rebase(p, dir, remoteAppDir(hc.host, app))
	case asDir != "":
		rebase(p, dir, asDir)
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

// checkBindSources refuses relative bind mounts that are not in Git: every Sync makes the app dir match Git
// exactly, so files a container writes there would be deleted on the next Sync (and on reboot, with the
// work dir in /run). Only named volumes and absolute paths keep data.
func checkBindSources(ctx context.Context, app domain.Application, dir string) error {
	p, err := loadProject(ctx, app, dir, false) // paths as written, to tell ./data from /data
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrInvalid, err)
	}
	for _, name := range slices.Sorted(maps.Keys(p.Services)) {
		for _, v := range p.Services[name].Volumes {
			if v.Type != types.VolumeTypeBind || filepath.IsAbs(v.Source) || strings.HasPrefix(v.Source, "~") {
				continue
			}
			src := filepath.Join(dir, v.Source)
			if !insideDir(dir, src) {
				return fmt.Errorf("%w: service %q mounts %s, outside the Application's files; "+
					"use a named volume or an absolute path", domain.ErrInvalid, name, v.Source)
			}
			if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("%w: service %q mounts %s, which is not in Git: OmegaCD deletes files that are not in Git on every Sync, "+
					"so data written there would be lost; use a named volume or an absolute path", domain.ErrInvalid, name, v.Source)
			}
		}
	}
	return nil
}

func (e *Engine) Diff(ctx context.Context, app domain.Application, dir, asDir string) ([]domain.ServiceDiff, error) {
	hc, err := e.conn(ctx, app.Host)
	if err != nil {
		return nil, err
	}
	p, err := e.load(ctx, app, dir)
	if err != nil {
		return nil, err
	}
	if asDir == "" {
		e.remember(app, p)
	}
	placeFor(p, hc, app, dir, asDir)
	ctrs, err := containers(ctx, hc.cli.Client(), app.Name)
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
		// shortcut: use_api_socket is hashed after compose adds a socket mount and, given registry credentials Diff does not see, a config; such services always show as changed.
		resolved, ok := resolveRefs(svc, byService)
		if !ok { // referenced service has no container: compose will recreate this one
			diffs = append(diffs, domain.ServiceDiff{Service: name, Change: "changed", Fields: []string{"config"}})
			continue
		}
		hash, err := serviceHash(resolved)
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
			d := domain.ServiceDiff{Service: name, Change: "removed"}
			// already stopped by a Sync without Prune: only Prune has anything left to do
			if !slices.ContainsFunc(byService[name], func(c container.Summary) bool { return c.State == "running" || c.State == "restarting" }) {
				d.Fields = []string{domain.FieldStopped}
			}
			diffs = append(diffs, d)
		}
	}
	nets, vols, err := undeclared(ctx, hc.cli.Client(), p)
	if err != nil {
		return nil, err
	}
	for _, n := range nets {
		diffs = append(diffs, domain.ServiceDiff{Kind: "network", Service: n.Name, Change: "removed"})
	}
	for _, v := range vols {
		diffs = append(diffs, domain.ServiceDiff{Kind: "volume", Service: v.Name, Change: "removed"})
	}
	return diffs, nil
}

// resolveRefs mirrors compose's resolveServiceReferences (pkg/compose/service_containers.go), applied before
// it hashes a service: service:x in volumes_from, network_mode, ipc and pid points at x's first container.
func resolveRefs(s types.ServiceConfig, byService map[string][]container.Summary) (types.ServiceConfig, bool) {
	first := func(name string) (string, bool) {
		if len(byService[name]) == 0 {
			return "", false
		}
		return slices.MinFunc(byService[name], func(a, b container.Summary) int {
			return strings.Compare(canonicalName(a), canonicalName(b))
		}).ID, true
	}
	s.VolumesFrom = slices.Clone(s.VolumesFrom)
	for i, v := range s.VolumesFrom {
		spec := strings.Split(v, ":")
		if spec[0] == "container" && len(spec) > 1 {
			s.VolumesFrom[i] = spec[1]
			continue
		}
		id, ok := first(spec[0])
		if !ok {
			return s, false
		}
		s.VolumesFrom[i] = id
	}
	for _, mode := range []*string{&s.NetworkMode, &s.Ipc, &s.Pid} {
		if name, found := strings.CutPrefix(*mode, types.NetworkModeServicePrefix); found {
			id, ok := first(name)
			if !ok {
				return s, false
			}
			*mode = types.ContainerPrefix + id
		}
	}
	return s, true
}

// canonicalName copies compose's internal naming.CanonicalContainerName: the name without slash or link aliases.
func canonicalName(c container.Summary) string {
	if len(c.Names) == 0 {
		return c.ID[:12]
	}
	for _, n := range c.Names {
		if strings.LastIndex(n, "/") == 0 {
			return n[1:]
		}
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

func (e *Engine) Sync(ctx context.Context, app domain.Application, dir string, opts domain.SyncOptions) error {
	hc, err := e.conn(ctx, app.Host)
	if err != nil {
		return err
	}
	p, err := e.load(ctx, app, dir)
	if err != nil {
		return fmt.Errorf("load compose project: %w", err)
	}
	if err := checkNoBuild(p); err != nil {
		return err
	}
	e.remember(app, p)
	var stale bool
	if hc.host.Name != domain.LocalHost {
		placeFor(p, hc, app, dir, "")
		if stale, err = e.pushFiles(ctx, hc, app, dir); err != nil {
			return fmt.Errorf("copy files to host %s: %w", hc.host.Name, err)
		}
	}
	svc, err := newCompose(authCLI{Cli: hc.cli, cf: registryConfig(opts.Registries)}, opts.Progress)
	if err != nil {
		return err
	}
	// Up pulls by each service's pull_policy (default: only missing images), so a registry outage
	// doesn't fail Syncs and Self-Heals whose images are already present.
	recreate := api.RecreateDiverged
	if opts.ForceRecreate {
		recreate = api.RecreateForce
	}
	err = svc.Up(ctx, p, api.UpOptions{Create: api.CreateOptions{
		RemoveOrphans:        opts.Prune,
		Recreate:             recreate,
		RecreateDependencies: api.RecreateDiverged,
	}})
	if err != nil {
		return fmt.Errorf("compose up: %w", err)
	}
	if stale {
		// only now: until Up recreated the containers, they used the old folder
		if err := e.removeFiles(ctx, hc, app, app.Dir()); err != nil {
			slog.Warn("remove old application files", "app", app.Name, "host", hc.host.Name, "err", err)
		}
	}
	if err := settleUndeclared(ctx, hc.cli.Client(), p, opts.Prune); err != nil {
		return err
	}
	if !opts.Prune {
		return nil
	}
	return pruneUndeclared(ctx, hc.cli.Client(), p, opts.PruneVolumes)
}

// settleUndeclared handles containers of services the project no longer declares (removed from
// the compose files, or their profile switched off): with prune they are deleted, otherwise
// stopped. Compose itself keeps profile-disabled services even with RemoveOrphans.
func settleUndeclared(ctx context.Context, c client.APIClient, p *types.Project, prune bool) error {
	res, err := c.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: projectFilter(p.Name).Add("label", api.OneoffLabel+"=False")})
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}
	var errs []error
	for _, ct := range res.Items {
		svc := ct.Labels[api.ServiceLabel]
		if _, ok := p.Services[svc]; ok {
			continue
		}
		switch {
		case prune:
			if _, err := c.ContainerRemove(ctx, ct.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
				errs = append(errs, fmt.Errorf("prune service %s: %w", svc, err))
			}
		case ct.State == "running" || ct.State == "restarting":
			if _, err := c.ContainerStop(ctx, ct.ID, client.ContainerStopOptions{}); err != nil {
				errs = append(errs, fmt.Errorf("stop undeclared service %s: %w", svc, err))
			}
		}
	}
	return errors.Join(errs...)
}

// undeclared lists the networks and volumes labelled with the project that it no longer declares.
// Volumes of profile-disabled services still count as declared: switching a profile off keeps their data.
func undeclared(ctx context.Context, c client.APIClient, p *types.Project) ([]network.Summary, []volume.Volume, error) {
	nets, err := c.NetworkList(ctx, client.NetworkListOptions{Filters: projectFilter(p.Name)})
	if err != nil {
		return nil, nil, fmt.Errorf("list networks: %w", err)
	}
	vols, err := c.VolumeList(ctx, client.VolumeListOptions{Filters: projectFilter(p.Name)})
	if err != nil {
		return nil, nil, fmt.Errorf("list volumes: %w", err)
	}
	declared := map[string]bool{}
	for k := range p.Volumes {
		declared[k] = true
	}
	for _, s := range p.DisabledServices {
		for _, v := range s.Volumes {
			if v.Type == types.VolumeTypeVolume {
				declared[v.Source] = true
			}
		}
	}
	// a resource without its compose key label was not created by compose for this project: never ours to delete
	var rn []network.Summary
	for _, n := range nets.Items {
		if k := n.Labels[api.NetworkLabel]; k != "" {
			if _, ok := p.Networks[k]; !ok {
				rn = append(rn, n)
			}
		}
	}
	var rv []volume.Volume
	for _, v := range vols.Items {
		if k := v.Labels[api.VolumeLabel]; k != "" && !declared[k] {
			rv = append(rv, v)
		}
	}
	slices.SortFunc(rn, func(a, b network.Summary) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(rv, func(a, b volume.Volume) int { return strings.Compare(a.Name, b.Name) })
	return rn, rv, nil
}

// pruneUndeclared deletes the project's undeclared networks, and with volumes its undeclared volumes,
// that no container of any project (stopped ones included) uses; those in use stay, prune pending.
// Compose up only prunes containers. Bind mounts are not volumes, so no host path is ever touched.
func pruneUndeclared(ctx context.Context, c client.APIClient, p *types.Project, volumes bool) error {
	nets, vols, err := undeclared(ctx, c, p)
	if err != nil {
		return err
	}
	inUse := func(filter, value string) (bool, error) {
		res, err := c.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: make(client.Filters).Add(filter, value)})
		return len(res.Items) > 0, err
	}
	var errs []error
	for _, n := range nets {
		used, err := inUse("network", n.ID)
		if err == nil && !used {
			_, err = c.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{})
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("prune network %s: %w", n.Name, err))
		}
	}
	if !volumes {
		vols = nil
	}
	for _, v := range vols {
		used, err := inUse("volume", v.Name)
		if err == nil && !used {
			// not forced: a container created meanwhile makes this fail instead of losing its data
			_, err = c.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{})
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("prune volume %s: %w", v.Name, err))
		}
	}
	return errors.Join(errs...)
}

// Remove also deletes the app dir on a remote Host. With volumes, compose deletes the volumes labelled
// with the project (it skips ones it did not create) and the containers' anonymous volumes.
func (e *Engine) Remove(ctx context.Context, app domain.Application, volumes bool) error {
	hc, err := e.conn(ctx, app.Host)
	if err != nil {
		return err
	}
	if err := hc.compose.Down(ctx, app.Name, api.DownOptions{RemoveOrphans: true, Volumes: volumes}); err != nil {
		return fmt.Errorf("compose down: %w", err)
	}
	if hc.host.Name != domain.LocalHost {
		if err := e.removeFiles(ctx, hc, app, ""); err != nil {
			return fmt.Errorf("remove files on host %s: %w", hc.host.Name, err)
		}
	}
	e.mu.Lock()
	delete(e.desired, app.Name)
	e.mu.Unlock()
	return nil
}

// Status derives the desired services from the project last loaded for app (Validate/Diff/Sync).
// Until one was loaded in this process, it falls back to the services seen on containers, so Missing cannot show.
func (e *Engine) Status(ctx context.Context, app domain.Application) (domain.LiveState, error) {
	hc, err := e.conn(ctx, app.Host)
	if err != nil {
		return domain.LiveState{}, err
	}
	c := hc.cli.Client()
	ctrs, err := containers(ctx, c, app.Name)
	if err != nil {
		return domain.LiveState{}, err
	}
	ls := domain.LiveState{Containers: []domain.Container{}, Resources: []domain.Resource{}, Health: domain.Healthy}
	seen := map[string]bool{}
	usedBy := map[string][]string{} // "volume/x" or "network/x" -> services
	use := func(key, svc string) {
		if !slices.Contains(usedBy[key], svc) {
			usedBy[key] = append(usedBy[key], svc)
			slices.Sort(usedBy[key])
		}
	}
	for _, ct := range ctrs {
		for _, m := range ct.Mounts {
			if m.Name != "" {
				use("volume/"+m.Name, ct.Labels[api.ServiceLabel])
			}
		}
		if ct.NetworkSettings != nil {
			for n := range ct.NetworkSettings.Networks {
				use("network/"+n, ct.Labels[api.ServiceLabel])
			}
		}
		insp, err := c.ContainerInspect(ctx, ct.ID, client.ContainerInspectOptions{})
		if err != nil {
			return domain.LiveState{}, fmt.Errorf("inspect container %s: %w", ct.ID, err)
		}
		st := insp.Container.State
		health := ""
		if st.Health != nil {
			health = string(st.Health.Status)
		}
		dc := domain.Container{
			ID:       ct.ID,
			Name:     strings.TrimPrefix(insp.Container.Name, "/"),
			Service:  ct.Labels[api.ServiceLabel],
			Image:    ct.Image,
			State:    string(st.Status),
			Status:   ct.Status,
			ExitCode: st.ExitCode,
			Health:   containerHealth(string(st.Status), health, st.ExitCode, insp.Container.RestartCount, keepsRunning(string(insp.Container.HostConfig.RestartPolicy.Name))),
		}
		ls.Containers = append(ls.Containers, dc)
		seen[dc.Service] = true
	}
	e.mu.Lock()
	ls.Services = e.desired[app.Name]
	e.mu.Unlock()
	if ls.Services == nil {
		ls.Services = slices.Sorted(maps.Keys(seen))
	}
	// only declared services count: undeclared ones are stopped on purpose (prune pending), not broken
	for _, c := range ls.Containers {
		if slices.Contains(ls.Services, c.Service) {
			ls.Health = worst(ls.Health, c.Health)
		}
	}
	for _, s := range ls.Services {
		if !seen[s] {
			ls.Health = worst(ls.Health, domain.Missing)
		}
	}
	if len(ctrs) == 0 {
		ls.Health = domain.Missing
	}

	nets, err := c.NetworkList(ctx, client.NetworkListOptions{Filters: projectFilter(app.Name)})
	if err != nil {
		return domain.LiveState{}, fmt.Errorf("list networks: %w", err)
	}
	for _, n := range nets.Items {
		ls.Resources = append(ls.Resources, domain.Resource{Kind: "network", Name: n.Name, Services: usedBy["network/"+n.Name]})
	}
	vols, err := c.VolumeList(ctx, client.VolumeListOptions{Filters: projectFilter(app.Name)})
	if err != nil {
		return domain.LiveState{}, fmt.Errorf("list volumes: %w", err)
	}
	for _, v := range vols.Items {
		ls.Resources = append(ls.Resources, domain.Resource{Kind: "volume", Name: v.Name, Services: usedBy["volume/"+v.Name]})
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

func (e *Engine) ProjectExists(ctx context.Context, host, name string) (exists, managed bool, err error) {
	hc, err := e.conn(ctx, host)
	if err != nil {
		return false, false, err
	}
	ctrs, err := containers(ctx, hc.cli.Client(), name)
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

func (e *Engine) Logs(ctx context.Context, app domain.Application, containerID string, follow bool, tail int, w io.Writer) error {
	hc, err := e.conn(ctx, app.Host)
	if err != nil {
		return err
	}
	c := hc.cli.Client()
	insp, err := c.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) || err == nil && (insp.Container.Config == nil || insp.Container.Config.Labels[api.ProjectLabel] != app.Name) {
		return fmt.Errorf("container %q: %w", containerID, domain.ErrNotFound)
	}
	if err != nil {
		return err
	}
	t := "all"
	if tail > 0 {
		t = strconv.Itoa(tail)
	}
	rc, err := c.ContainerLogs(ctx, containerID, client.ContainerLogsOptions{
		ShowStdout: true, ShowStderr: true, Timestamps: true, Follow: follow, Tail: t,
	})
	if err != nil {
		return err
	}
	defer rc.Close()
	if insp.Container.Config.Tty {
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
func containers(ctx context.Context, c client.APIClient, project string) ([]container.Summary, error) {
	res, err := c.ContainerList(ctx, client.ContainerListOptions{
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

// authCLI hands compose a config file holding only omegacd's registry credentials.
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
