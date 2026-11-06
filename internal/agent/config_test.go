package agent

import (
	"bytes"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestConfigDefaults(t *testing.T) {
	for name, tc := range map[string]struct {
		env              map[string]string
		euid             int
		state, work, err string
	}{
		"user session": {
			env:   map[string]string{"HOME": "/home/ana", "XDG_RUNTIME_DIR": "/run/user/1000"},
			euid:  1000,
			state: "/home/ana/.local/share/omegacd-agent", work: "/run/user/1000/omegacd",
		},
		"user with XDG_DATA_HOME": {
			env:   map[string]string{"HOME": "/home/ana", "XDG_DATA_HOME": "/data/ana", "XDG_RUNTIME_DIR": "/run/user/1000"},
			euid:  1000,
			state: "/data/ana/omegacd-agent", work: "/run/user/1000/omegacd",
		},
		"system service (systemd StateDirectory, docker-group user)": {
			env:   map[string]string{"HOME": "/home/omegacd", "STATE_DIRECTORY": "/var/lib/omegacd-agent"},
			euid:  998,
			state: "/var/lib/omegacd-agent", work: "/run/omegacd",
		},
		"explicit dirs win": {
			env: map[string]string{"HOME": "/home/ana", "STATE_DIRECTORY": "/var/lib/omegacd-agent", "XDG_RUNTIME_DIR": "/run/user/1000",
				"OMEGACD_AGENT_STATE_DIR": "/srv/agent", "OMEGACD_AGENT_WORKDIR": "/opt/services"},
			euid:  1000,
			state: "/srv/agent", work: "/opt/services",
		},
		"dirs cleaned": {
			env:   map[string]string{"HOME": "/h", "OMEGACD_AGENT_STATE_DIR": "/srv//agent/", "OMEGACD_AGENT_WORKDIR": "/opt/services/"},
			euid:  1000,
			state: "/srv/agent", work: "/opt/services",
		},
		"root refused": {
			env: map[string]string{"HOME": "/root"}, euid: 0, err: "OMEGACD_AGENT_ALLOW_ROOT=true",
		},
		"root allowed": {
			env:   map[string]string{"HOME": "/root", "OMEGACD_AGENT_ALLOW_ROOT": "true"},
			state: "/root/.local/share/omegacd-agent", work: "/run/omegacd",
		},
		"no home": {env: map[string]string{}, euid: 1000, err: "OMEGACD_AGENT_STATE_DIR"},
		"relative work dir": {
			env: map[string]string{"HOME": "/h", "OMEGACD_AGENT_WORKDIR": "work"}, euid: 1000, err: "absolute",
		},
		"bad control plane": {
			env: map[string]string{"HOME": "/h", "OMEGACD_AGENT_CONTROL_PLANE": "omegacd.example.com"}, euid: 1000, err: "https://",
		},
		"bad cache size": {
			env: map[string]string{"HOME": "/h", "OMEGACD_AGENT_CACHE_REVISIONS": "0"}, euid: 1000, err: "at least 1",
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := LoadConfig(env(tc.env), tc.euid)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want one mentioning %q", err, tc.err)
				}
				return
			}
			if err != nil || c.StateDir != tc.state || c.WorkDir != tc.work || c.CacheRevisions != 10 {
				t.Fatalf("got %+v, %v; want state %s, work %s, 10 Revisions", c, err, tc.state, tc.work)
			}
		})
	}
}

func TestConfigSettings(t *testing.T) {
	c, err := LoadConfig(env(map[string]string{"HOME": "/h", "OMEGACD_AGENT_CONTROL_PLANE": "https://cp.example.com",
		"OMEGACD_AGENT_INVITE": " inv-123 ", "DOCKER_HOST": "unix:///run/user/1000/docker.sock", "OMEGACD_AGENT_CACHE_REVISIONS": "25"}), 1000)
	if err != nil || c.ControlPlane != "https://cp.example.com" || c.Invite != "inv-123" ||
		c.DockerHost != "unix:///run/user/1000/docker.sock" || c.CacheRevisions != 25 {
		t.Fatalf("got %+v, %v", c, err)
	}
}

// --help documents every OMEGACD_ and DOCKER_ variable LoadConfig reads.
func TestUsageDocumentsEverySetting(t *testing.T) {
	var buf bytes.Buffer
	Usage(&buf)
	read := map[string]bool{}
	LoadConfig(func(k string) string { read[k] = true; return map[string]string{"HOME": "/h"}[k] }, 1000)
	for k := range read {
		if (strings.HasPrefix(k, "OMEGACD_") || strings.HasPrefix(k, "DOCKER_")) && !strings.Contains(buf.String(), k) {
			t.Errorf("%s is read but not in --help", k)
		}
	}
	if !read["OMEGACD_AGENT_CACHE_REVISIONS"] {
		t.Fatal("the probe missed settings")
	}
}
