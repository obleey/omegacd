package agent

import "testing"

// What the three engines of the test matrix answer (recorded from them).
func TestEngineInfo(t *testing.T) {
	for name, tc := range map[string]struct {
		components []component
		security   []string
		want       EngineInfo
	}{
		"rootful Docker": {[]component{{"Engine", "29.8.2"}, {"containerd", "2.1"}}, []string{"name=seccomp,profile=builtin", "name=cgroupns"},
			EngineInfo{Name: "docker", Version: "29.8.2", OS: "linux", Arch: "arm64"}},
		"rootless Docker": {[]component{{"Engine", "29.9.3"}}, []string{"name=seccomp,profile=builtin", "name=rootless", "name=cgroupns"},
			EngineInfo{Name: "docker", Version: "29.8.2", Rootless: true, OS: "linux", Arch: "arm64"}},
		"rootless Podman": {[]component{{"Conmon", "2.1"}, {"Podman Engine", "5.8.9"}}, []string{"name=seccomp,profile=default", "name=rootless"},
			EngineInfo{Name: "podman", Version: "5.8.9", Rootless: true, OS: "linux", Arch: "arm64"}},
	} {
		if got := engineInfo("29.8.2", "linux", "arm64", tc.components, tc.security); got != tc.want {
			t.Errorf("%s: %+v, want %+v", name, got, tc.want)
		}
	}
}
