package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/obleey/omegacd/internal/domain"
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
		if err == nil && !complete(ag.work.Dir(a), r) {
			err = ag.writeFiles(ctx, a, r)
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
		"sources as root-owned directories; run the Agent's files restore before Docker starts (omegacd agent restore "+
		"--files, see the systemd unit), or remove them once: sudo find %s -depth -type d -empty -delete", err, dir, dir)
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
	ag.mu.Lock()
	defer ag.mu.Unlock()
	r, err := ag.cache.Revision(ctx, a.Name, a.Revision)
	if err != nil {
		return err
	}
	dir := ag.work.Dir(a)
	missing := !complete(dir, r)
	if !missing {
		if live, err := ag.engine.Status(ctx, application(a, r)); err == nil && running(live) {
			return nil
		}
	}
	if missing && inContainer() {
		// a containerised Agent starts after Docker: clear the placeholders through Docker, with its own image
		if err := removePlaceholders(ctx, dir, ""); err != nil {
			return err
		}
	}
	return ag.report(ctx, a, r, triggerRestore, func() error {
		if missing {
			if err := ag.writeFiles(ctx, a, r); err != nil {
				return err
			}
		}
		return ag.apply(ctx, a, r, missing, nil) // recreate: bind mounts re-attach to the restored files
	})
}

// removePlaceholders deletes the empty directories under dir through Docker, as root, with image (the Agent's own
// when empty: it is local, so this works offline). Only empty directories: never a file.
func removePlaceholders(ctx context.Context, dir, image string) error {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
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
		Config: &container.Config{Image: image, User: "0", Entrypoint: []string{"find", dir, "-mindepth", "1", "-depth",
			"-type", "d", "-empty", "-delete"}, Labels: map[string]string{"dev.omegacd.agent": "placeholders"}},
		HostConfig: &container.HostConfig{Binds: []string{dir + ":" + dir}, AutoRemove: false},
	})
	if err != nil {
		return fmt.Errorf("remove placeholders in %s: %w", dir, err)
	}
	defer c.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	if _, err := c.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("remove placeholders in %s: %w", dir, err)
	}
	wait := c.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{})
	select {
	case res := <-wait.Result:
		if res.StatusCode != 0 {
			return fmt.Errorf("remove placeholders in %s: exit %d", dir, res.StatusCode)
		}
	case err := <-wait.Error:
		return fmt.Errorf("remove placeholders in %s: %w", dir, err)
	}
	return nil
}

// report runs do and records it in the outbox as a Sync Operation the Agent started (Q24).
func (ag *Agent) report(ctx context.Context, a App, r Revision, trigger string, do func() error) error {
	err := do()
	rep := &agentv1.SyncOperationReport{App: a.Name, Revision: r.Revision, Trigger: trigger, Phase: string(domain.OpSucceeded)}
	if err != nil {
		rep.Phase, rep.Message = string(domain.OpFailed), err.Error()
	}
	if _, rerr := ag.db.Record(ctx, &agentv1.OutboxEntry{Event: &agentv1.OutboxEntry_SyncOperationReport{SyncOperationReport: rep}}); rerr != nil {
		return errors.Join(err, rerr)
	}
	return err
}

// selfHeal re-applies an Application's deployed Revision from the cache when it has Self-Heal on and its containers
// aren't running and healthy or have drifted.
func (ag *Agent) selfHeal(ctx context.Context, name string) error {
	ag.mu.Lock()
	defer ag.mu.Unlock()
	a, r, err := ag.deployed(ctx, name)
	if err != nil || !a.SelfHeal {
		return nil // not ours, or nothing to heal
	}
	app := application(a, r)
	live, err := ag.engine.Status(ctx, app)
	if err != nil {
		return err
	}
	if running(live) {
		if d, err := ag.engine.Diff(ctx, app, a.Dir, ""); err != nil || len(d) == 0 {
			return err
		}
	}
	return ag.report(ctx, a, r, triggerSelfHeal, func() error { return ag.apply(ctx, a, r, false, nil) })
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
func (ag *Agent) Run(ctx context.Context, log *slog.Logger) error {
	var mu sync.Mutex
	pending := map[string]bool{}
	watching := make(chan error, 1)
	// watching first, then the restore: nothing that changes in between is missed
	go func() {
		watching <- ag.Watch(ctx, func(app string) {
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
					log.Error("self-heal", "app", app, "err", err)
				}
			})
		})
	}()
	if err := ag.Restore(ctx); err != nil {
		log.Error("restore", "err", err)
	}
	return <-watching
}
