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
	"slices"
	"strings"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/obleey/omegacd/internal/domain"
)

// helperImage runs the short-lived containers that write Application files on a remote Host.
const helperImage = "busybox:1.38"

// filesLib starts the file helper's scripts. Their arguments: the Host's work dir (mounted at /work, for messages),
// the app's folder in it (Application.Dir, "" when removing the app), the app's name, its App of Apps (or ""), 1 if
// the engine found the app's folder to be the app's (ownedDir), its App of Apps' children (space-separated), and the
// folders the store says the app wrote to (relative to /work).
const filesLib = `set -eu
root=$1 dst="/work/$2" name=$3 parent=$4 owned=$5 kids=" $6 "
shift 6
formers=" $* "
# claim DIR APP, the controller's claimDir: refuse DIR unless it carries APP's marker from before (deleted now), or
# carries no marker and is missing, holds nothing but directories, is the app's (owned), or is APP's App of Apps'
# folder holding nothing but its children's folders (re-checked as entries go: siblings sync concurrently)
claim() {
  m=$(cat "$1/.omegacd" 2>/dev/null || true)
  if [ "$m" = "$2" ] || { [ -z "$m" ] && { [ "$owned" = 1 ] || [ ! -e "$1" ] ||
    [ -z "$(find "$1" ! -type d 2>&1 | head -n 1)" ] || { [ "$2" = "$parent" ] && kidsonly "$1"; }; }; }; then
    mkdir -p "$1"
    [ -z "$m" ] || rm -f "$1/.omegacd"
    return
  fi
  echo "refusing to sync into $root${1#/work}: the folder exists and OmegaCD did not create it for Application \"$2\"; rename the Application, pick another work dir, or move the folder away" >&2
  exit 1
}
# kidsonly DIR: DIR holds nothing but folders named after the App of Apps' children (or a marker removed meanwhile)
kidsonly() {
  [ "$kids" != "  " ] || return 1
  for e in "$1"/* "$1"/.[!.]* "$1"/..?*; do
    [ -e "$e" ] || [ -L "$e" ] || continue
    case "$kids" in *" ${e##*/} "*) [ -d "$e" ] && [ ! -L "$e" ] || return 1 ;; *) return 1 ;; esac
  done
}
# stale DIR: DIR is a folder of this app other than dst: one it wrote to (formers), one carrying its marker from
# before, or its folder from the old layout
stale() {
  [ "$1" != "$dst" ] && [ -d "$1" ] && [ ! -L "$1" ] || return 1
  case "$formers" in *" ${1#/work/} "*) return ;; esac
  [ "$1" = "/work/apps/$name" ] || [ "$(cat "$1/.omegacd" 2>/dev/null)" = "$name" ]
}
`

// syncScript claims the app's folder (and its App of Apps' folder) and mirrors /tmp/stage into it. Unchanged files
// are left alone and changed regular files are rewritten in place, so single-file bind mounts keep their inode;
// files not in the stage are deleted. It prints "stale" when the app has another folder (old layout, former App of
// Apps) for the engine to remove once the containers no longer use it.
// shortcut: file names containing newlines are not supported; the whole dir is sent each Sync, fine for compose-sized dirs.
const syncScript = filesLib + `[ -z "$parent" ] || claim "/work/$parent" "$parent"
claim "$dst" "$name"
src=/tmp/stage
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
for d in /work/"$name" /work/*/"$name"; do
  if stale "$d"; then echo stale; fi
done
`

// cleanScript removes the app's folders other than dst, and the old layout's apps folder once it is empty.
// Only folders stale proves the app's are touched.
const cleanScript = filesLib + `for d in /work/"$name" /work/*/"$name"; do
  if stale "$d"; then
    rm -rf "$d"
    [ "$d" != "/work/apps/$name" ] || rmdir /work/apps 2>/dev/null || true
  fi
done
`

// pushFiles copies dir's contents to the app dir on hc's Host through the Docker API, so it works over ssh and tls
// alike. children is its App of Apps' children. stale reports that the app has another folder there, for
// removeFiles once the containers moved.
func (e *Engine) pushFiles(ctx context.Context, hc *hostConn, app domain.Application, dir string, children []string) (stale bool, err error) {
	if err := checkAppName(app); err != nil {
		return false, err
	}
	owned, err := ownedDir(ctx, hc, app)
	if err != nil {
		return false, err
	}
	var buf bytes.Buffer
	if err := tarDir(&buf, dir, "stage"); err != nil {
		return false, fmt.Errorf("pack %s: %w", dir, err)
	}
	args := append([]string{"sh", "-c", syncScript, "sh", remoteWorkDir(hc.host), app.Dir(), app.Name, app.Parent, owned,
		strings.Join(children, " ")}, formerDirs(hc, app)...)
	out, err := runHelper(ctx, hc, args, &buf)
	return strings.Contains(out, "stale"), err
}

// ownedDir is "1" if the app's folder on hc's Host is provably the app's: the store says a Sync wrote to it, or a
// container OmegaCD created for the app runs from it. Containers of other tools never count.
func ownedDir(ctx context.Context, hc *hostConn, app domain.Application) (string, error) {
	dst := remoteAppDir(hc.host, app)
	if slices.Contains(app.Dirs, domain.HostDir(hc.host.Name, dst)) {
		return "1", nil
	}
	wds, err := workingDirs(ctx, hc.cli.Client(), app.Name)
	if err != nil || !slices.Contains(wds, dst) {
		return "", err
	}
	return "1", nil
}

// formerDirs is the app's Dirs on hc's Host, relative to its work dir.
func formerDirs(hc *hostConn, app domain.Application) []string {
	var dirs []string
	for _, d := range app.Dirs {
		if p, ok := strings.CutPrefix(d, domain.HostDir(hc.host.Name, remoteWorkDir(hc.host)+"/")); ok {
			dirs = append(dirs, p)
		}
	}
	return dirs
}

// removeFiles deletes the app's folders on hc's Host, except keep (an Application.Dir; "" removes them all).
func (e *Engine) removeFiles(ctx context.Context, hc *hostConn, app domain.Application, keep string) error {
	if err := checkAppName(app); err != nil {
		return err
	}
	args := append([]string{"sh", "-c", cleanScript, "sh", remoteWorkDir(hc.host), keep, app.Name, app.Parent, "", ""}, formerDirs(hc, app)...)
	_, err := runHelper(ctx, hc, args, nil)
	return err
}

// checkAppName keeps the app dir inside the Host's work dir.
func checkAppName(app domain.Application) error {
	bad := func(n string) bool { return n == "" || n == "." || n == ".." || strings.ContainsAny(n, "/\\") }
	if bad(app.Name) || app.Parent != "" && bad(app.Parent) {
		return fmt.Errorf("invalid application name %q", app.Dir())
	}
	return nil
}

// runHelper runs cmd in a helper container with the Host's work dir mounted at /work, after copying the tar
// stage (if any) into its /tmp, and returns its output. The legacy bind creates the work dir when missing.
func runHelper(ctx context.Context, hc *hostConn, cmd []string, stage io.Reader) (string, error) {
	c := hc.cli.Client()
	if _, err := c.ImageInspect(ctx, helperImage); err != nil {
		res, err := c.ImagePull(ctx, helperImage, client.ImagePullOptions{})
		if err != nil {
			return "", fmt.Errorf("pull %s: %w", helperImage, err)
		}
		if err := res.Wait(ctx); err != nil {
			return "", fmt.Errorf("pull %s: %w", helperImage, err)
		}
	}
	created, err := c.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{Image: helperImage, Cmd: cmd, Labels: map[string]string{"dev.omegacd.helper": "files"}},
		HostConfig: &container.HostConfig{
			Binds:       []string{remoteWorkDir(hc.host) + ":/work"},
			NetworkMode: "none",
		},
	})
	if err != nil {
		return "", fmt.Errorf("create file helper: %w", err)
	}
	defer c.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})
	if stage != nil {
		if _, err := c.CopyToContainer(ctx, created.ID, client.CopyToContainerOptions{DestinationPath: "/tmp", Content: stage}); err != nil {
			return "", fmt.Errorf("copy files to host: %w", err)
		}
	}
	if _, err := c.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("start file helper: %w", err)
	}
	wait := c.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case err := <-wait.Error:
		return "", fmt.Errorf("file helper: %w", err)
	case res := <-wait.Result:
		var out bytes.Buffer
		if logs, err := c.ContainerLogs(ctx, created.ID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true}); err == nil {
			stdcopy.StdCopy(&out, &out, logs)
			logs.Close()
		}
		if res.StatusCode != 0 {
			return "", fmt.Errorf("file helper exited %d: %s", res.StatusCode, strings.TrimSpace(out.String()))
		}
		return out.String(), nil
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
