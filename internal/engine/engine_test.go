package engine

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/distribution/reference"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

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

func TestNoLocalHost(t *testing.T) {
	e, err := New(Config{NoLocalHost: true}) // no Docker CLI set up at all
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"", domain.LocalHost} {
		if _, err := e.conn(context.Background(), host); !errors.Is(err, domain.ErrLocalHostDisabled) {
			t.Fatalf("conn(%q) = %v", host, err)
		}
	}
	if _, err := e.Status(context.Background(), domain.Application{Name: "web"}); !errors.Is(err, domain.ErrLocalHostDisabled) {
		t.Fatalf("Status on local = %v", err)
	}
}

type listClient struct {
	client.APIClient
	items []container.Summary
}

func (c listClient) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	return client.ContainerListResult{Items: c.items}, nil
}

// Only containers OmegaCD created for the app vouch for their folder: not an adopted project's (doco-cd's alloy).
func TestWorkingDirs(t *testing.T) {
	ctr := func(dir string, labels ...string) container.Summary {
		l := map[string]string{api.WorkingDirLabel: dir}
		for i := 0; i < len(labels); i += 2 {
			l[labels[i]] = labels[i+1]
		}
		return container.Summary{Labels: l}
	}
	c := listClient{items: []container.Summary{
		ctr("/opt/services/alloy"),
		ctr("/opt/other", domain.ManagedLabel, "alloy"),
		ctr("/opt/older", domain.ManagedByLabel, domain.ManagedByValue, domain.ManagedLabel, "other"),
		ctr("/opt/omegacd/alloy", domain.ManagedByLabel, domain.ManagedByValue, domain.ManagedLabel, "alloy"),
	}}
	if got, err := workingDirs(t.Context(), c, "alloy"); err != nil || !slices.Equal(got, []string{"/opt/omegacd/alloy"}) {
		t.Fatalf("workingDirs = %v, %v", got, err)
	}
}

// M2-11: the init services are those another waits for to complete; a dependency waited on only to start or be
// healthy isn't one.
func TestInitServices(t *testing.T) {
	svc := func(name string, deps map[string]string) types.ServiceConfig {
		s := types.ServiceConfig{Name: name}
		s.DependsOn = types.DependsOnConfig{}
		for d, cond := range deps {
			s.DependsOn[d] = types.ServiceDependency{Condition: cond}
		}
		return s
	}
	done, healthy := types.ServiceConditionCompletedSuccessfully, types.ServiceConditionHealthy
	p := &types.Project{Services: types.Services{
		"web":     svc("web", map[string]string{"migrate": done, "db": healthy}),
		"worker":  svc("worker", map[string]string{"migrate": done, "seed": done}),
		"migrate": svc("migrate", map[string]string{"db": healthy}),
		"db":      svc("db", nil),
		"seed":    svc("seed", nil),
	}}
	if got := initServices(p); !slices.Equal(got, []string{"migrate", "seed"}) {
		t.Fatalf("got %v", got)
	}
}

func TestRegistries(t *testing.T) {
	p := &types.Project{Services: types.Services{
		"a": {Name: "a", ContainerSpec: types.ContainerSpec{Image: "nginx:1.27"}},
		"b": {Name: "b", ContainerSpec: types.ContainerSpec{Image: "ghcr.io/obleey/omegacd:0.2.0"}},
		"c": {Name: "c", ContainerSpec: types.ContainerSpec{Image: "localhost:5000/private/app:1"}},
		"d": {Name: "d", ContainerSpec: types.ContainerSpec{Image: "docker.io/library/busybox:1.36"}},
		"e": {Name: "e", ContainerSpec: types.ContainerSpec{Image: "ghcr.io/obleey/other:1"}},
		"f": {Name: "f", ContainerSpec: types.ContainerSpec{Image: "Not A Valid Reference"}}, // skipped: the engine says why
	}}
	if got := registries(p); !slices.Equal(got, []string{"docker.io", "ghcr.io", "localhost:5000"}) {
		t.Fatalf("registries = %v", got)
	}
}
