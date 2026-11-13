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
	"slices"
	"strconv"
	"strings"

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

// DockerHost is the Docker engine of the machine import runs on (DOCKER_HOST, the socket). There is no SSH path (Q47):
// M6-05 reads another Host's stacks through its Agent.
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

// Info stats paths under dir with du and stat in a helper container that mounts dir read-only.
func (h *DockerHost) Info(ctx context.Context, dir string, paths []string) (map[string]Info, error) {
	if len(paths) == 0 {
		return map[string]Info{}, nil
	}
	const script = `cd /src && for p in "$@"; do
  if [ -d "$p" ]; then echo "d $(du -sk "$p" | cut -f1) $p"
  elif [ -f "$p" ]; then echo "f $(stat -c %s "$p") $p"
  else echo "- 0 $p"; fi
done > /tmp/out`
	b, err := h.helper(ctx, dir, script, paths)
	if err != nil {
		return nil, err
	}
	return parseInfo(b), nil
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
func (h *DockerHost) Read(ctx context.Context, dir string, paths []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	if len(paths) == 0 {
		return out, nil
	}
	// shortcut: names with newlines are not supported (one name per line in the list)
	const script = `cd /src && for p in "$@"; do [ -e "$p" ] && printf '%s\n' "./$p"; done > /tmp/list
[ -s /tmp/list ] && tar -cf /tmp/out -T /tmp/list || : > /tmp/out`
	b, err := h.helper(ctx, dir, script, paths)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read files from %s: %w", dir, err)
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Size > 50<<20 {
			continue
		}
		c, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out[path.Clean(strings.TrimPrefix(hdr.Name, "./"))] = c
	}
}

// helper runs script (with paths as $@) in a container mounting dir read-only at /src, and returns /tmp/out.
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
			Labels: map[string]string{"dev.omegacd.helper": "import"}},
		HostConfig: &container.HostConfig{Binds: []string{dir + ":/src:ro"}, NetworkMode: "none"},
	})
	if err != nil {
		return nil, fmt.Errorf("create helper container: %w", err)
	}
	defer h.c.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})
	if _, err := h.c.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return nil, fmt.Errorf("start helper container: %w", err)
	}
	wait := h.c.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case err := <-wait.Error:
		return nil, fmt.Errorf("helper container: %w", err)
	case res := <-wait.Result:
		if res.StatusCode != 0 {
			return nil, fmt.Errorf("helper container exited %d", res.StatusCode)
		}
	}
	cp, err := h.c.CopyFromContainer(ctx, created.ID, client.CopyFromContainerOptions{SourcePath: "/tmp/out"})
	if err != nil {
		return nil, fmt.Errorf("copy from helper container: %w", err)
	}
	defer cp.Content.Close()
	tr := tar.NewReader(cp.Content) // the API wraps the copied file in a tar of its own
	if _, err := tr.Next(); err != nil {
		return nil, fmt.Errorf("copy from helper container: %w", err)
	}
	return io.ReadAll(tr)
}
