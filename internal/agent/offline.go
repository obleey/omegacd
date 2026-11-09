package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/logging"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
)

// Triggers of what the Agent does on its own, as reported (the Control Plane's are "manual", "auto", …).
const (
	triggerRestore  = "restore"
	triggerSelfHeal = "self-heal"
)

// WorkDirOnDisk reports whether decrypted secrets in dir would outlive a reboot (Q19, Q59).
func WorkDirOnDisk(dir string) bool { return !onRAM(dir) }

// complete reports whether dir holds r's files: after a reboot with the work dir in RAM it is empty, or holds only
// the empty directories Docker made for missing bind-mount sources.
func complete(dir string, r Revision) bool {
	for _, f := range r.Files {
		p := filepath.Join(dir, filepath.FromSlash(f.Path))
		if base := filepath.Base(p); strings.Contains(base, ".sops.") {
			// decrypting replaces x.sops.ext with x.ext
			p = filepath.Join(filepath.Dir(p), strings.Replace(base, ".sops.", ".", 1))
		}
		fi, err := os.Lstat(p)
		if err != nil || fi.IsDir() {
			return false
		}
	}
	return true
}

// RestoreFiles writes every deployed Application's files back from the cache and decrypts them, without any engine:
// the half of a restore that can run before Docker starts (Q59), so Docker finds the files when it restarts containers.
func (ag *Agent) RestoreFiles(ctx context.Context) error {
	apps, err := ag.db.ListApps(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range apps {
		if a.Revision == "" {
			continue
		}
		r, err := ag.cache.Revision(ctx, a.Name, a.Revision)
		if err == nil {
			done := complete(ag.work.Dir(a), r)
			ag.log.Debug("restore files", "app", a.Name, "revision", a.Revision, "write", !done)
			if !done {
				err = ag.writeFiles(ctx, a, r)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", a.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (ag *Agent) writeFiles(ctx context.Context, a App, r Revision) error {
	if err := ag.work.Write(ctx, a, r); err != nil {
		return placeholders(ag.work.Dir(a), err)
	}
	_, err := ag.secrets.Decrypt(ag.work.Dir(a))
	return err
}

// placeholders turns a permission error from writing into dir into one that says what happened and what to do, when
// Docker made root-owned placeholders there.
func placeholders(dir string, err error) error {
	if !errors.Is(err, fs.ErrPermission) {
		return err
	}
	return fmt.Errorf("%w: Docker restarted a container before the Agent restored %s and made its missing bind-mount "+
		"sources (and the folders above them) as root; run the Agent's files restore before Docker starts (omegacd agent "+
		"restore --files, see the systemd unit), or fix it once: sudo find %s -mindepth 1 -depth -type d -empty -delete && "+
		"sudo chown %d:%d %s %s", err, dir, dir, os.Getuid(), os.Getgid(), filepath.Dir(dir), dir)
}

// Restore brings every deployed Application back as it was after a reboot (Q24): files from the cache where they are
// missing, then Compose for those whose files were missing or whose containers aren't running and healthy. It
// never deploys anything new and never stops or removes anything. Each restore goes to the outbox.
func (ag *Agent) Restore(ctx context.Context) error {
	apps, err := ag.db.ListApps(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range apps {
		if a.Revision == "" {
			continue
		}
		if err := ag.restore(ctx, a); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", a.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (ag *Agent) restore(ctx context.Context, a App) error {
	unlock, err := ag.lock()
	if err != nil {
		return err
	}
	defer unlock()
	ctx = logging.With(ctx, "app", a.Name, "revision", a.Revision)
	r, err := ag.cache.Revision(ctx, a.Name, a.Revision)
	if err != nil {
		return err
	}
	dir := ag.work.Dir(a)
	missing := !complete(dir, r)
	if !missing {
		live, err := ag.engine.Status(ctx, application(a, r))
		if err == nil && running(live) {
			ag.log.DebugContext(ctx, "restore: nothing to do, files present and containers running", "health", live.Health)
			return nil
		}
		ag.log.DebugContext(ctx, "restore: containers not running", "health", live.Health, "err", err)
	}
	ag.log.InfoContext(ctx, "restoring", "filesMissing", missing)
	if missing && inContainer() {
		// a containerised Agent starts after Docker: clear the placeholders through Docker, with its own image
		ag.log.DebugContext(ctx, "reclaim the work dir from Docker's placeholders", "dir", dir)
		if err := reclaim(ctx, ag.work.root(a), dir, ""); err != nil {
			return err
		}
	}
	return ag.report(ctx, a, r, triggerRestore, func() error {
		if missing {
			if err := ag.writeFiles(ctx, a, r); err != nil {
				return err
			}
		}
		return ag.apply(ctx, a, r, missing, domain.SyncOptions{}) // recreate: bind mounts re-attach to the restored files
	})
}

// reclaim undoes what Docker did to a RAM work dir it found empty after a reboot: it deletes the empty directories it
// made under dir (the app folder) for missing bind-mount sources, and gives root and the folders from root down to dir
// back to the Agent's user, all through Docker as root, with image (the Agent's own when empty: it is local, so this
// works offline). It deletes only empty directories and changes the owner of directories only: never a file.
func reclaim(ctx context.Context, root, dir, image string) error {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("%s is not in the work dir %s", dir, root)
	}
	chown := []string{root}
	for p := root; rel != "."; {
		first, rest, _ := strings.Cut(rel, string(filepath.Separator))
		p, rel = filepath.Join(p, first), cmp.Or(rest, ".")
		chown = append(chown, p)
	}
	c, err := client.New(client.FromEnv)
	if err != nil {
		return err
	}
	defer c.Close()
	if image == "" {
		self, _ := os.Hostname()
		insp, err := c.ContainerInspect(ctx, self, client.ContainerInspectOptions{})
		if err != nil {
			return err
		}
		image = insp.Container.Config.Image
	}
	created, err := c.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{Image: image, User: "0", Labels: map[string]string{"dev.omegacd.agent": "reclaim"},
			Entrypoint: append([]string{"sh", "-c", `find "$1" -mindepth 1 -depth -type d -empty -delete && owner=$2 && shift 2 && chown "$owner" "$@"`,
				"reclaim", dir, fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())}, chown...)},
		HostConfig: &container.HostConfig{Binds: []string{root + ":" + root}},
	})
	if err != nil {
		return fmt.Errorf("reclaim %s: %w", dir, err)
	}
	defer c.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	if _, err := c.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("reclaim %s: %w", dir, err)
	}
	wait := c.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{})
	select {
	case res := <-wait.Result:
		if res.StatusCode != 0 {
			return fmt.Errorf("reclaim %s: exit %d", dir, res.StatusCode)
		}
	case err := <-wait.Error:
		return fmt.Errorf("reclaim %s: %w", dir, err)
	}
	return nil
}

// record puts a report in the outbox and has a live connection send it (M3-06).
func (ag *Agent) record(ctx context.Context, rep *agentv1.SyncOperationReport) error {
	return ag.recordEntry(ctx, &agentv1.OutboxEntry{Event: &agentv1.OutboxEntry_SyncOperationReport{SyncOperationReport: rep}})
}

func (ag *Agent) recordEntry(ctx context.Context, e *agentv1.OutboxEntry) error {
	if _, err := ag.db.Record(ctx, e); err != nil {
		return err
	}
	select {
	case ag.recorded <- struct{}{}:
	default:
	}
	return nil
}

// report runs do and records it in the outbox as a Sync Operation the Agent started (Q24).
func (ag *Agent) report(ctx context.Context, a App, r Revision, trigger string, do func() error) error {
	err := do()
	rep := &agentv1.SyncOperationReport{App: a.Name, Revision: r.Revision, Checksum: r.Checksum, Trigger: trigger,
		Phase: string(domain.OpSucceeded)}
	if err != nil {
		rep.Phase, rep.Message = string(domain.OpFailed), err.Error()
	}
	return errors.Join(err, ag.record(ctx, rep))
}

// selfHeal re-applies an Application's deployed Revision from the cache when it has Self-Heal on and its containers
// aren't running and healthy or have drifted.
func (ag *Agent) selfHeal(ctx context.Context, name string) error {
	unlock, err := ag.lock()
	if err != nil {
		return err
	}
	defer unlock()
	ctx = logging.With(ctx, "app", name)
	a, r, err := ag.deployed(ctx, name)
	if err != nil || !a.SelfHeal {
		ag.log.DebugContext(ctx, "self-heal: not this Agent's to heal", "selfHeal", a.SelfHeal, "err", err)
		return nil // not ours, or nothing to heal
	}
	ctx = logging.With(ctx, "revision", a.Revision)
	app := application(a, r)
	live, err := ag.engine.Status(ctx, app)
	if err != nil {
		return err
	}
	why := "containers " + string(live.Health)
	if running(live) {
		d, err := ag.engine.Diff(ctx, app, a.Dir, "")
		if err != nil || len(d) == 0 {
			ag.log.DebugContext(ctx, "self-heal: running as deployed", "health", live.Health, "err", err)
			return err
		}
		why = fmt.Sprintf("drifted: %d services", len(d))
	}
	ag.log.InfoContext(ctx, "self-healing", "why", why)
	return ag.report(ctx, a, r, triggerSelfHeal, func() error { return ag.apply(ctx, a, r, false, domain.SyncOptions{}) })
}

// running reports whether an Application's containers are up: Healthy, or still starting (Progressing). Anything
// else after a reboot or a stop is for the Agent to bring back.
func running(live domain.LiveState) bool {
	return len(live.Containers) > 0 && (live.Health == domain.Healthy || live.Health == domain.Progressing)
}

// selfHealDelay lets a burst of engine events (a restart is several) settle before the Agent looks.
const selfHealDelay = 3 * time.Second

// Run keeps the Host's Applications as deployed until ctx ends (Q24): it restores them, then watches the engine and
// self-heals those that have Self-Heal on. It works the same with or without a Control Plane.
func (ag *Agent) Run(ctx context.Context) error {
	var mu sync.Mutex
	pending := map[string]bool{}
	watching := make(chan error, 1)
	// watching first, then the restore: nothing that changes in between is missed
	go func() {
		watching <- ag.Watch(ctx, func(app string) {
			ag.reports.soon(ctx, app) // the Control Plane sees the change (M4-02)
			mu.Lock()
			defer mu.Unlock()
			if pending[app] {
				return
			}
			pending[app] = true
			time.AfterFunc(selfHealDelay, func() {
				mu.Lock()
				delete(pending, app)
				mu.Unlock()
				if err := ag.selfHeal(ctx, app); err != nil {
					ag.log.Error("self-heal failed; the next change to its containers tries again", "app", app, "err", err)
				}
			})
		})
	}()
	if err := ag.Restore(ctx); err != nil {
		ag.log.Error("restore failed; fix the cause, then restart the Agent to try again", "err", err)
	}
	return <-watching
}
