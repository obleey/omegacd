// Package agent is the Agent: the omegacd process on a Host that deploys its Applications with Compose (Q3, Q10).
package agent

import (
	"cmp"
	"fmt"
	"io"
	"net/url"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	ControlPlane   string   // URL; empty while the Agent works on its own
	Invite         string   // single-use, for the first connection
	StateDir       string   // signing key, Host Key, database, cache
	WorkDir        string   // the Applications' folders
	WorkDirAllow   []string // further roots an Application may name as its work dir (Q57)
	DockerHost     string   // empty: the engine's default socket
	CacheRevisions int      // per Application, besides the deployed one
	SecretGroup    int      // gid decrypted files belong to (mode 0640); -1: none, they are 0600
	AllowRoot      bool
}

// settings is every environment variable LoadConfig reads, for --help.
var settings = []struct{ name, def, help string }{
	{"OMEGACD_AGENT_CONTROL_PLANE", "", "URL of the Control Plane, e.g. https://omegacd.example.com"},
	{"OMEGACD_AGENT_INVITE", "", "single-use invite from Add Host, for the first connection"},
	{"OMEGACD_AGENT_STATE_DIR", "$STATE_DIRECTORY (systemd), else $XDG_DATA_HOME/omegacd-agent, else ~/.local/share/omegacd-agent",
		"keys, database and cache; keep it, losing it loses the Host Key"},
	{"OMEGACD_AGENT_WORKDIR", "$XDG_RUNTIME_DIR/omegacd, else /run/omegacd",
		"where Applications' files are written, decrypted secrets included: in RAM (tmpfs, the default) they never touch a " +
			"disk and are restored from the cache after a reboot; on a disk path they stay there across reboots (the Agent warns)"},
	{"OMEGACD_AGENT_WORKDIR_ALLOW", "", "comma-separated absolute paths an Application may name as its own work dir " +
		"(workDir:), besides the work dir above; the Agent refuses any other, so Git can't aim it at /etc"},
	{"DOCKER_HOST", "the engine's default socket", "Docker or Podman socket, e.g. unix:///run/user/1000/docker.sock for rootless Docker"},
	{"OMEGACD_AGENT_CACHE_REVISIONS", "10", "Revisions kept per Application for offline restore and rollback"},
	{"OMEGACD_AGENT_SECRET_GROUP", "", "a group (name or gid) decrypted files belong to, mode 0640, for containers that read them " +
		"as a non-root user and join it with group_add: (the gid; with rootless Docker or Podman \"0\"); without it they are 0600, readable by containers running as root " +
		"(rootful) or as the Agent's user (rootless). (On macOS, Docker Desktop and OrbStack ignore owner and mode of shared folders.)"},
	{"OMEGACD_AGENT_ALLOW_ROOT", "false", "true runs the Agent as root; use a docker-group user or rootless Docker instead"},
	{"OMEGACD_LOG_LEVEL", "info", "debug, info, warn or error; debug says why the Agent did what it did (never a secret)"},
	{"OMEGACD_LOG_FORMAT", "text", "text or json (one object per line)"},
}

// Usage documents every setting.
func Usage(w io.Writer) {
	fmt.Fprintln(w, "Usage: omegacd agent [restore --files | rollback [APP [REVISION]]]\n\n"+
		"Runs the Agent on this Host.\n"+
		"  restore --files              writes the deployed Applications' files back from the cache, before Docker starts\n"+
		"  rollback                     lists every Application's cached Revisions, the deployed one marked\n"+
		"  rollback APP                 lists APP's cached Revisions\n"+
		"  rollback APP REVISION        deploys a cached Revision of APP again (a unique prefix will do), without the\n"+
		"                               Control Plane; it shows APP as rolled back on the Host, its Auto-Sync off\n"+
		"\nSettings are environment variables:")
	for _, s := range settings {
		fmt.Fprintf(w, "\n  %s\n      %s\n      default: %s\n", s.name, s.help, cmp.Or(s.def, "(none)"))
	}
	fmt.Fprintln(w, "\nSecrets: SOPS with age, decrypted with the Host Key the Agent creates in its state dir (host.key).")
}

// LoadConfig reads the settings from getenv for a process running as euid.
func LoadConfig(getenv func(string) string, euid int) (Config, error) {
	c := Config{
		ControlPlane: strings.TrimSpace(getenv("OMEGACD_AGENT_CONTROL_PLANE")),
		Invite:       strings.TrimSpace(getenv("OMEGACD_AGENT_INVITE")),
		DockerHost:   getenv("DOCKER_HOST"),
		AllowRoot:    getenv("OMEGACD_AGENT_ALLOW_ROOT") == "true",
	}
	if euid == 0 && !c.AllowRoot {
		return c, fmt.Errorf("refusing to run as root: run the Agent as a user in the docker group, or with rootless Docker or Podman " +
			"(set OMEGACD_AGENT_ALLOW_ROOT=true to run as root anyway)")
	}
	if c.ControlPlane != "" {
		if u, err := url.Parse(c.ControlPlane); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return c, fmt.Errorf("OMEGACD_AGENT_CONTROL_PLANE=%q: want https://host[:port]", c.ControlPlane)
		}
	}

	// systemd sets STATE_DIRECTORY for StateDirectory=omegacd-agent: /var/lib/omegacd-agent in a system unit
	state, _, _ := strings.Cut(getenv("STATE_DIRECTORY"), ":")
	c.StateDir = cmp.Or(getenv("OMEGACD_AGENT_STATE_DIR"), state)
	if c.StateDir == "" {
		data := getenv("XDG_DATA_HOME")
		if data == "" && getenv("HOME") != "" {
			data = filepath.Join(getenv("HOME"), ".local", "share")
		}
		if data == "" {
			return c, fmt.Errorf("no state dir: set OMEGACD_AGENT_STATE_DIR (or HOME)")
		}
		c.StateDir = filepath.Join(data, "omegacd-agent")
	}
	c.WorkDir = getenv("OMEGACD_AGENT_WORKDIR")
	if c.WorkDir == "" {
		c.WorkDir = "/run/omegacd"
		if rt := getenv("XDG_RUNTIME_DIR"); rt != "" {
			c.WorkDir = filepath.Join(rt, "omegacd")
		}
	}
	for _, d := range []struct{ name, path string }{{"state", c.StateDir}, {"work", c.WorkDir}} {
		if !filepath.IsAbs(d.path) {
			return c, fmt.Errorf("the %s dir %q must be an absolute path", d.name, d.path)
		}
	}
	// cleaned, so they compare with paths Docker reports (path parity) whatever the spelling: /opt/services/, //opt/…
	c.StateDir, c.WorkDir = filepath.Clean(c.StateDir), filepath.Clean(c.WorkDir)
	for _, r := range strings.Split(getenv("OMEGACD_AGENT_WORKDIR_ALLOW"), ",") {
		if r = strings.TrimSpace(r); r == "" {
			continue
		}
		if !filepath.IsAbs(r) || filepath.Clean(r) == "/" {
			return c, fmt.Errorf("OMEGACD_AGENT_WORKDIR_ALLOW: %q must be an absolute path, not /", r)
		}
		c.WorkDirAllow = append(c.WorkDirAllow, filepath.Clean(r))
	}

	c.CacheRevisions = 10
	if v := strings.TrimSpace(getenv("OMEGACD_AGENT_CACHE_REVISIONS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("OMEGACD_AGENT_CACHE_REVISIONS=%q: want a number of Revisions, at least 1", v)
		}
		c.CacheRevisions = n
	}
	c.SecretGroup = -1
	if g := strings.TrimSpace(getenv("OMEGACD_AGENT_SECRET_GROUP")); g != "" {
		gid, err := strconv.Atoi(g)
		if err != nil {
			grp, lerr := user.LookupGroup(g)
			if lerr != nil {
				return c, fmt.Errorf("OMEGACD_AGENT_SECRET_GROUP=%q: no such group", g)
			}
			gid, _ = strconv.Atoi(grp.Gid)
		}
		if gid < 0 {
			return c, fmt.Errorf("OMEGACD_AGENT_SECRET_GROUP=%q: want a group name or gid", g)
		}
		c.SecretGroup = gid
	}
	return c, nil
}
