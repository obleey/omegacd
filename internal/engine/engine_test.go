package engine

import (
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/distribution/reference"

	"github.com/obleey/omegacd/internal/domain"
)

func TestContainerHealth(t *testing.T) {
	tests := []struct {
		state, health      string
		exitCode, restarts int
		longRunning        bool
		want               domain.HealthStatus
	}{
		{"running", "", 0, 0, false, domain.Healthy},
		{"running", "healthy", 0, 0, false, domain.Healthy},
		{"running", "starting", 0, 0, false, domain.Progressing},
		{"running", "unhealthy", 0, 0, false, domain.Degraded},
		{"created", "", 0, 0, false, domain.Progressing},
		{"restarting", "", 1, 0, false, domain.Progressing},
		{"restarting", "", 1, 3, false, domain.Degraded},
		{"exited", "", 0, 0, false, domain.Healthy},
		{"exited", "", 0, 0, true, domain.Degraded},
		{"exited", "", 137, 0, false, domain.Degraded},
		{"dead", "", 0, 0, false, domain.Degraded},
		{"paused", "", 0, 0, false, domain.Degraded},
	}
	for _, tt := range tests {
		if got := containerHealth(tt.state, tt.health, tt.exitCode, tt.restarts, tt.longRunning); got != tt.want {
			t.Errorf("containerHealth(%q, %q, %d, %d, %v) = %s, want %s", tt.state, tt.health, tt.exitCode, tt.restarts, tt.longRunning, got, tt.want)
		}
	}
}

func TestWorst(t *testing.T) {
	h := domain.Healthy
	for _, s := range []domain.HealthStatus{domain.Progressing, domain.Degraded, domain.Missing, domain.Healthy} {
		h = worst(h, s)
	}
	if h != domain.Degraded {
		t.Fatalf("worst = %s, want Degraded", h)
	}
	if got := worst(domain.Progressing, domain.Missing); got != domain.Missing {
		t.Fatalf("worst(Progressing, Missing) = %s", got)
	}
}

func TestRegistryConfig(t *testing.T) {
	cf := registryConfig([]domain.RegistryCredential{
		{Registry: "index.docker.io", Username: "hub"},
		{Registry: "https://ghcr.io/", Username: "gh"},
		{Registry: "registry.example.com:5000", Username: "priv"},
	})
	tests := map[string]string{
		"nginx":                              "hub",
		"docker.io/library/nginx:1.27":       "hub",
		"ghcr.io/org/app:v1":                 "gh",
		"registry.example.com:5000/team/app": "priv",
		"quay.io/other/app":                  "",
	}
	for image, want := range tests {
		ref, err := reference.ParseNormalizedNamed(image)
		if err != nil {
			t.Fatal(err)
		}
		// compose looks credentials up by reference.Domain, see compose internal/registry.EncodedAuth
		auth, err := cf.GetAuthConfig(reference.Domain(ref))
		if err != nil {
			t.Fatal(err)
		}
		if auth.Username != want {
			t.Errorf("%s: username %q, want %q", image, auth.Username, want)
		}
	}
	if hub := registryConfig([]domain.RegistryCredential{{Registry: "docker.io", Username: "u"}}); hub.AuthConfigs["https://index.docker.io/v1/"].Username != "u" {
		t.Errorf("docker.io not keyed as Docker Hub index: %v", hub.AuthConfigs)
	}
}

// Golden value copied from compose v5.7.0 internal/confighash/hash_test.go.
func TestServiceHashGolden(t *testing.T) {
	replicas := 3
	var svc types.ServiceConfig
	svc.Name = "web"
	svc.Image = "nginx:latest"
	svc.Command = types.ShellCommand{"nginx", "-g", "daemon off;"}
	svc.User = "nobody"
	svc.Tty = true
	svc.StdinOpen = true
	svc.Restart = types.RestartPolicyAlways
	s1 := "1"
	svc.Environment = types.MappingWithEquals{"A": &s1, "B": nil}
	svc.Labels = types.Labels{"com.example": "v"}
	svc.Annotations = types.Mapping{"note": "x"}
	svc.CapAdd = []string{"NET_ADMIN"}
	svc.ExtraHosts = types.HostsList{"alpha": []string{"10.0.0.1"}}
	svc.Deploy = &types.DeployConfig{Replicas: &replicas}
	svc.Ports = []types.ServicePortConfig{{Target: 80, Published: "8080", Protocol: "tcp"}}
	svc.HealthCheck = &types.HealthCheckConfig{Test: types.HealthCheckTest{"CMD", "true"}}
	svc.Volumes = []types.ServiceVolumeConfig{{Type: types.VolumeTypeVolume, Source: "data", Target: "/data"}}
	svc.Networks = map[string]*types.ServiceNetworkConfig{"default": nil}

	got, err := serviceHash(svc)
	if err != nil {
		t.Fatal(err)
	}
	if want := "75bc312132c71fac4971d123202631b6978ac9d796336621af744d46611b42a2"; got != want {
		t.Fatalf("serviceHash = %s, want %s", got, want)
	}
}

func TestResolveRefsMissingContainer(t *testing.T) {
	var s types.ServiceConfig
	s.NetworkMode = "service:db"
	if _, ok := resolveRefs(s, nil); ok {
		t.Fatal("resolveRefs with no db container = ok; want not ok")
	}
}
