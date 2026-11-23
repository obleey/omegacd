package importer

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

const (
	projectLabel = "com.docker.compose.project"
	dirLabel     = "com.docker.compose.project.working_dir"
	filesLabel   = "com.docker.compose.project.config_files"
	ownLabel     = "dev.omegacd.application"
	helperImage  = "busybox:1.38" // reads files on the host from a short-lived container, like the engine writes them
)

// DockerHost is the engine of the Agent's Host (DOCKER_HOST, as the Agent set it: rootless, Podman); the CLI reaches it
// only through the Agent (Q47, Q321).
type DockerHost struct{ c client.APIClient }

// Dial connects to the engine the way the docker CLI does.
func Dial() (*DockerHost, error) {
	c, err := client.New(client.FromEnv)
	return &DockerHost{c}, err
}

func (h *DockerHost) Close() error { return h.c.Close() }

// Stacks lists the Compose projects with containers on the host, from the labels Compose puts on them.
func (h *DockerHost) Stacks(ctx context.Context) ([]Stack, error) {
	res, err := h.c.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: make(client.Filters).Add("label", projectLabel)})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	byName := map[string]*Stack{}
	for _, c := range res.Items {
		name := c.Labels[projectLabel]
		s := byName[name]
		if s == nil {
			s = &Stack{Name: name, Dir: c.Labels[dirLabel]}
			for _, f := range strings.Split(c.Labels[filesLabel], ",") {
				if rel, ok := strings.CutPrefix(path.Clean(f), s.Dir+"/"); ok && s.Dir != "" {
					s.Files = append(s.Files, rel)
				}
			}
			byName[name] = s
		}
		s.Managed = s.Managed || c.Labels[ownLabel] != ""
	}
	var out []Stack
	for _, n := range slices.Sorted(maps.Keys(byName)) {
		out = append(out, *byName[n])
	}
	return out, nil
}

// Storage is every volume's mountpoint, by its name.
func (h *DockerHost) Storage(ctx context.Context) (map[string]string, error) {
	res, err := h.c.VolumeList(ctx, client.VolumeListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list volumes: %w", err)
	}
	out := map[string]string{}
	for _, v := range res.Items {
		if v.Mountpoint != "" {
			out[v.Mountpoint] = v.Name
		}
	}
	return out, nil
}

// within is the folder the helper mounts to reach every path (relative to dir, some beside it with ../), and the
// paths relative to that folder.
func within(dir string, paths []string) (string, []string) {
	base := dir
	for _, p := range paths {
		for abs := path.Join(dir, p); base != "/" && abs != base && !strings.HasPrefix(abs, base+"/"); {
			base = path.Dir(base)
		}
	}
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = rel(base, path.Join(dir, p))
	}
	return base, out
}

// back is a path relative to base as the importer names it: relative to dir, with ../ when beside it.
func back(base, dir, p string) string { return rel(dir, path.Join(base, p)) }

func rel(from, to string) string {
	r, _ := filepath.Rel(from, to) // both absolute: it never fails
	return filepath.ToSlash(r)
}

// Info stats paths under dir with du and stat in a helper container that mounts dir (or the folder above it that
// holds them all) read-only.
func (h *DockerHost) Info(ctx context.Context, dir string, paths []string) (map[string]Info, error) {
	if len(paths) == 0 {
		return map[string]Info{}, nil
	}
	base, inBase := within(dir, paths)
	const script = `cd /src && for p in "$@"; do
  if [ -d "$p" ]; then echo "d $(du -sk "$p" | cut -f1) $p"
  elif [ -f "$p" ]; then echo "f $(stat -c %s "$p") $p"
  else echo "- 0 $p"; fi
done`
	b, err := h.helper(ctx, base, script, inBase)
	if err != nil {
		return nil, err
	}
	out := map[string]Info{}
	for p, in := range parseInfo(b) {
		out[back(base, dir, p)] = in
	}
	return out, nil
}

// parseInfo reads the helper's "KIND SIZE PATH" lines. A size du or stat couldn't tell (no permission) is unknown:
// as large as can be, so it is never taken for a small config file to copy (Q134).
func parseInfo(b []byte) map[string]Info {
	out := map[string]Info{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.SplitN(sc.Text(), " ", 3)
		if len(f) != 3 {
			continue
		}
		n, err := strconv.ParseInt(f[1], 10, 64)
		switch {
		case err != nil:
			n = math.MaxInt64
		case f[0] == "d":
			n *= 1024 // du -k
		}
		out[f[2]] = Info{f[0][0], n}
	}
	return out
}

// Read copies the files at or under paths from the host: tar in a helper container, then out of it.
func (h *DockerHost) Read(ctx context.Context, dir string, paths []string) (map[string][]byte, []string, error) {
	out := map[string][]byte{}
	if len(paths) == 0 {
		return out, nil, nil
	}
	// shortcut: names with newlines are not supported (one name per line in the list)
	const script = `cd /src && for p in "$@"; do [ -e "$p" ] && printf '%s\n' "./$p"; done > /tmp/list
if [ -s /tmp/list ]; then tar -cf - -T /tmp/list; fi`
	base, inBase := within(dir, paths)
	b, err := h.helper(ctx, base, script, inBase)
	if err != nil {
		return nil, nil, err
	}
	var skipped []string
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, skipped, nil
		}
		if err != nil { // coverage: unreachable, the helper's tar, copied out whole by the engine
			return nil, nil, fmt.Errorf("read files from %s: %w", dir, err)
		}
		name := back(base, dir, path.Clean(strings.TrimPrefix(hdr.Name, "./")))
		if hdr.Typeflag == tar.TypeDir {
			continue // its files follow
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Size > 50<<20 {
			skipped = append(skipped, name)
			continue
		}
		c, err := io.ReadAll(tr)
		if err != nil { // coverage: unreachable, a file of a tar held in memory
			return nil, nil, err
		}
		out[name] = c
	}
}

// helper runs script (with paths as $@) in a container mounting dir read-only at /src, and returns what it writes to
// stdout, read through an attach: copying a file out of the stopped container instead needs the engine to mount its
// binds again, which rootless Docker 26 can't for a folder on a nosuid tmpfs (Debian's /tmp), and races Podman's
// cleanup of the container.
func (h *DockerHost) helper(ctx context.Context, dir, script string, paths []string) ([]byte, error) {
	if _, err := h.c.ImageInspect(ctx, helperImage); err != nil {
		res, err := h.c.ImagePull(ctx, helperImage, client.ImagePullOptions{})
		if err != nil {
			return nil, fmt.Errorf("pull %s: %w", helperImage, err)
		}
		if err := res.Wait(ctx); err != nil {
			return nil, fmt.Errorf("pull %s: %w", helperImage, err)
		}
	}
	created, err := h.c.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{Image: helperImage, Cmd: append([]string{"sh", "-c", script, "sh"}, paths...),
			AttachStdout: true, AttachStderr: true, Labels: map[string]string{"dev.omegacd.helper": "import"}},
		HostConfig: &container.HostConfig{Binds: []string{dir + ":/src:ro"}, NetworkMode: "none"},
	})
	if err != nil {
		return nil, fmt.Errorf("create helper container for %s: %w", dir, err)
	}
	defer h.c.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})
	att, err := h.c.ContainerAttach(ctx, created.ID, client.ContainerAttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return nil, fmt.Errorf("attach to helper container for %s: %w", dir, err)
	}
	defer att.Close()
	if _, err := h.c.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return nil, fmt.Errorf("start helper container for %s: %w", dir, err)
	}
	var out, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &stderr, att.Reader); err != nil {
		return nil, fmt.Errorf("read helper container for %s: %w", dir, err)
	}
	wait := h.c.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case err := <-wait.Error:
		return nil, fmt.Errorf("helper container for %s: %w", dir, err)
	case res := <-wait.Result:
		if res.StatusCode != 0 {
			return nil, fmt.Errorf("helper container for %s exited %d: %s", dir, res.StatusCode, strings.TrimSpace(stderr.String()))
		}
	}
	return out.Bytes(), nil
}
