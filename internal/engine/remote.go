package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// helperImage runs the short-lived containers that write Application files on a remote Host.
const helperImage = "busybox:1.36"

// syncScript mirrors /tmp/stage into /apps/$1. Unchanged files are left alone and changed regular files
// are rewritten in place, so single-file bind mounts keep their inode; files not in the stage are deleted.
// shortcut: file names containing newlines are not supported; the whole dir is sent each Sync, fine for compose-sized dirs.
const syncScript = `set -eu
src=/tmp/stage dst="/apps/$1"
mkdir -p "$dst"
cd "$src"
find . -mindepth 1 -type d | while IFS= read -r f; do
  if [ ! -d "$dst/$f" ] || [ -L "$dst/$f" ]; then rm -rf "$dst/$f"; mkdir "$dst/$f"; fi
  chmod "$(stat -c %a "$f")" "$dst/$f"
done
find . ! -type d | while IFS= read -r f; do
  if [ -f "$f" ] && [ ! -L "$f" ] && [ -f "$dst/$f" ] && [ ! -L "$dst/$f" ]; then
    chmod "$(stat -c %a "$f")" "$dst/$f"
    cmp -s "$f" "$dst/$f" || cat "$f" > "$dst/$f"
  else
    rm -rf "$dst/$f"; cp -a "$f" "$dst/$f"
  fi
done
cd "$dst"
find . -depth -mindepth 1 | while IFS= read -r f; do
  [ -e "$src/$f" ] || [ -L "$src/$f" ] || rm -rf "$f"
done
`

// pushFiles copies dir's contents to the app dir on hc's Host through the Docker API, so it works over ssh and tls alike.
func (e *Engine) pushFiles(ctx context.Context, hc *hostConn, app, dir string) error {
	if err := checkAppName(app); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := tarDir(&buf, dir, "stage"); err != nil {
		return fmt.Errorf("pack %s: %w", dir, err)
	}
	return runHelper(ctx, hc, []string{"sh", "-c", syncScript, "sh", app}, &buf)
}

// removeFiles deletes the app dir on hc's Host.
func (e *Engine) removeFiles(ctx context.Context, hc *hostConn, app string) error {
	if err := checkAppName(app); err != nil {
		return err
	}
	return runHelper(ctx, hc, []string{"rm", "-rf", "/apps/" + app}, nil)
}

// checkAppName keeps the app dir inside the Host's apps dir.
func checkAppName(app string) error {
	if app == "" || app == "." || app == ".." || strings.ContainsAny(app, "/\\") {
		return fmt.Errorf("invalid application name %q", app)
	}
	return nil
}

// runHelper runs cmd in a helper container with the Host's apps dir mounted at /apps, after copying
// the tar stage (if any) into its /tmp. The legacy bind creates the apps dir on the Host when missing.
func runHelper(ctx context.Context, hc *hostConn, cmd []string, stage io.Reader) error {
	c := hc.cli.Client()
	if _, err := c.ImageInspect(ctx, helperImage); err != nil {
		res, err := c.ImagePull(ctx, helperImage, client.ImagePullOptions{})
		if err != nil {
			return fmt.Errorf("pull %s: %w", helperImage, err)
		}
		if err := res.Wait(ctx); err != nil {
			return fmt.Errorf("pull %s: %w", helperImage, err)
		}
	}
	appsDir := path.Dir(remoteAppDir(hc.host, "x"))
	created, err := c.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{Image: helperImage, Cmd: cmd, Labels: map[string]string{"dev.dockercd.helper": "files"}},
		HostConfig: &container.HostConfig{
			Binds:       []string{appsDir + ":/apps"},
			NetworkMode: "none",
		},
	})
	if err != nil {
		return fmt.Errorf("create file helper: %w", err)
	}
	defer c.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})
	if stage != nil {
		if _, err := c.CopyToContainer(ctx, created.ID, client.CopyToContainerOptions{DestinationPath: "/tmp", Content: stage}); err != nil {
			return fmt.Errorf("copy files to host: %w", err)
		}
	}
	if _, err := c.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start file helper: %w", err)
	}
	wait := c.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case err := <-wait.Error:
		return fmt.Errorf("file helper: %w", err)
	case res := <-wait.Result:
		if res.StatusCode == 0 {
			return nil
		}
		var out bytes.Buffer
		if logs, err := c.ContainerLogs(ctx, created.ID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true}); err == nil {
			stdcopy.StdCopy(&out, &out, logs)
			logs.Close()
		}
		return fmt.Errorf("file helper exited %d: %s", res.StatusCode, strings.TrimSpace(out.String()))
	}
}

// tarDir writes dir as a tar under prefix/, keeping modes and symlinks, owned by root.
func tarDir(w io.Writer, dir, prefix string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		hdr.Name = path.Join(prefix, filepath.ToSlash(rel))
		if d.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}
