package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/docker"
	"github.com/watzon/ship/internal/scheduler"
)

func TestBuildActionsStopsFixedPortReplicaBeforeStart(t *testing.T) {
	cfg := testConfig()
	env := cfg.Environments["production"]
	oldName := ContainerName("demo", "production", "web", 1, "old")
	actions, err := BuildActions(PlanInput{
		Config:      cfg,
		Environment: env,
		EnvName:     "production",
		ReleaseID:   "new",
		Images:      map[string]string{"web": "example/web@sha256:111"},
		StateDir:    "/tmp/ship-state",
		Observed: []ObservedContainer{{
			Host: scheduler.Host{Name: "web-1", Pool: "web", User: "root"},
			Container: docker.ContainerSummary{
				Names: oldName,
				Labels: map[string]string{
					docker.LabelManagedBy:   docker.LabelManagedByValue,
					docker.LabelProject:     "demo",
					docker.LabelEnvironment: "production",
					docker.LabelService:     "web",
					docker.LabelReplica:     "1",
					docker.LabelRelease:     "old",
				},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []ActionKind
	for _, action := range actions {
		kinds = append(kinds, action.Kind)
	}
	wantKinds := []ActionKind{ActionPull, ActionStop, ActionStart, ActionHealth, ActionIngress}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("kinds = %#v, want %#v", kinds, wantKinds)
	}
	if actions[1].ContainerName != oldName {
		t.Fatalf("stop action = %+v", actions[1])
	}
	start := actions[2]
	if start.ContainerName != ContainerName("demo", "production", "web", 1, "new") {
		t.Fatalf("container name = %q", start.ContainerName)
	}
	if start.Labels[docker.LabelProject] != "demo" || start.Labels[docker.LabelEnvironment] != "production" || start.Labels[docker.LabelRelease] != "new" {
		t.Fatalf("labels = %+v", start.Labels)
	}
	if actions[3].Health.URL != "http://127.0.0.1:3000/up" || actions[3].Health.TimeoutSeconds != 30 {
		t.Fatalf("health = %+v", actions[3].Health)
	}
	if actions[4].IngressPath != "/tmp/ship-state/ingress/production.Caddyfile" || !strings.Contains(actions[4].IngressConfig, "example.com") {
		t.Fatalf("ingress action = %+v", actions[4])
	}
}

func TestBuildActionsAddsCustomServiceLabels(t *testing.T) {
	cfg := testConfig()
	svc := cfg.Services["web"]
	svc.Labels = map[string]string{
		"com.example.team": "platform",
		"tier":             "frontend",
	}
	svc.NetworkAliases = []string{"frontend", "web"}
	cfg.Services["web"] = svc
	env := cfg.Environments["production"]
	actions, err := BuildActions(PlanInput{
		Config:      cfg,
		Environment: env,
		EnvName:     "production",
		ReleaseID:   "new",
		Images:      map[string]string{"web": "example/web@sha256:111"},
		StateDir:    t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var start Action
	for _, action := range actions {
		if action.Kind == ActionStart {
			start = action
			break
		}
	}
	if start.Labels["com.example.team"] != "platform" || start.Labels["tier"] != "frontend" {
		t.Fatalf("custom labels = %+v", start.Labels)
	}
	if start.Labels[docker.LabelService] != "web" || start.Labels[docker.LabelRelease] != "new" {
		t.Fatalf("ship labels = %+v", start.Labels)
	}
	if start.Network != "ship-demo-production" || start.NetworkDriver != "bridge" {
		t.Fatalf("network = %q driver = %q", start.Network, start.NetworkDriver)
	}
	if strings.Join(start.NetworkAliases, ",") != "frontend,web" {
		t.Fatalf("network aliases = %+v", start.NetworkAliases)
	}
}

func TestBuildActionsUsesConfiguredDockerNetwork(t *testing.T) {
	cfg := testConfig()
	cfg.Docker.Network.Name = "edge-net"
	cfg.Docker.Network.Driver = "overlay"
	env := cfg.Environments["production"]
	actions, err := BuildActions(PlanInput{
		Config:      cfg,
		Environment: env,
		EnvName:     "production",
		ReleaseID:   "new",
		Images:      map[string]string{"web": "example/web@sha256:111"},
		StateDir:    t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var start, ingressAction *Action
	for i := range actions {
		switch actions[i].Kind {
		case ActionStart:
			start = &actions[i]
		case ActionIngress:
			ingressAction = &actions[i]
		}
	}
	if start == nil || start.Network != "edge-net" || start.NetworkDriver != "overlay" {
		t.Fatalf("start network = %+v", start)
	}
	if ingressAction == nil || ingressAction.Network != "edge-net" || ingressAction.NetworkDriver != "overlay" {
		t.Fatalf("ingress network = %+v", ingressAction)
	}
}

func TestBuildActionsHonorsMaxSurgeOneForRollingReplacement(t *testing.T) {
	cfg := rollingStrategyConfig(0, 1)
	env := cfg.Environments["production"]
	actions, err := BuildActions(PlanInput{
		Config:      cfg,
		Environment: env,
		EnvName:     "production",
		ReleaseID:   "new",
		Images:      map[string]string{"web": "example/web@sha256:111"},
		StateDir:    t.TempDir(),
		Observed: []ObservedContainer{
			statusObserved("web-1", "web", 1, "old", "Up 1 minute"),
			statusObserved("web-2", "web", 2, "old", "Up 1 minute"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := actionKinds(actions)
	want := []ActionKind{
		ActionPull, ActionStart, ActionHealth, ActionStop,
		ActionPull, ActionStart, ActionHealth, ActionStop,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %#v, want %#v", got, want)
	}
	if actions[3].ContainerName != ContainerName("demo", "production", "web", 1, "old") {
		t.Fatalf("first stop = %+v", actions[3])
	}
	if actions[7].ContainerName != ContainerName("demo", "production", "web", 2, "old") {
		t.Fatalf("second stop = %+v", actions[7])
	}
}

func TestBuildActionsInsertsCanaryPauseAfterFirstHealthyReplica(t *testing.T) {
	cfg := rollingStrategyConfig(0, 1)
	env := cfg.Environments["production"]
	env.Hosts.Pools["web"] = config.Pool{Count: 3}
	cfg.Environments["production"] = env
	web := cfg.Services["web"]
	web.Scale = 3
	web.Rolling.CanaryPauseSeconds = 45
	cfg.Services["web"] = web
	actions, err := BuildActions(PlanInput{
		Config:      cfg,
		Environment: env,
		EnvName:     "production",
		ReleaseID:   "new",
		Images:      map[string]string{"web": "example/web@sha256:111"},
		StateDir:    t.TempDir(),
		Observed: []ObservedContainer{
			statusObserved("web-1", "web", 1, "old", "Up 1 minute"),
			statusObserved("web-2", "web", 2, "old", "Up 1 minute"),
			statusObserved("web-3", "web", 3, "old", "Up 1 minute"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := actionKinds(actions)
	want := []ActionKind{
		ActionPull, ActionStart, ActionHealth, ActionCanary, ActionStop,
		ActionPull, ActionStart, ActionHealth, ActionStop,
		ActionPull, ActionStart, ActionHealth, ActionStop,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %#v, want %#v", got, want)
	}
	if actions[3].PauseDuration != 45*time.Second || actions[3].Service != "web" || actions[3].Replica != 1 {
		t.Fatalf("canary action = %+v", actions[3])
	}
}

func TestBuildActionsHonorsConfiguredCanaryReplicaBatch(t *testing.T) {
	cfg := rollingStrategyConfig(0, 2)
	env := cfg.Environments["production"]
	env.Hosts.Pools["web"] = config.Pool{Count: 4}
	cfg.Environments["production"] = env
	web := cfg.Services["web"]
	web.Scale = 4
	web.Rolling.CanaryReplicas = 2
	web.Rolling.CanaryPauseSeconds = 30
	cfg.Services["web"] = web
	actions, err := BuildActions(PlanInput{
		Config:      cfg,
		Environment: env,
		EnvName:     "production",
		ReleaseID:   "new",
		Images:      map[string]string{"web": "example/web@sha256:111"},
		StateDir:    t.TempDir(),
		Observed: []ObservedContainer{
			statusObserved("web-1", "web", 1, "old", "Up 1 minute"),
			statusObserved("web-2", "web", 2, "old", "Up 1 minute"),
			statusObserved("web-3", "web", 3, "old", "Up 1 minute"),
			statusObserved("web-4", "web", 4, "old", "Up 1 minute"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var canaryIndexes []int
	for i, action := range actions {
		if action.Kind == ActionCanary {
			canaryIndexes = append(canaryIndexes, i)
		}
	}
	if !reflect.DeepEqual(canaryIndexes, []int{6}) {
		t.Fatalf("canary indexes = %#v, actions = %#v", canaryIndexes, actionKinds(actions))
	}
	if actions[6].Replica != 2 || actions[6].PauseDuration != 30*time.Second {
		t.Fatalf("canary action = %+v", actions[6])
	}
}

func TestBuildActionsCarriesHealthRetrySettings(t *testing.T) {
	cfg := testConfig()
	web := cfg.Services["web"]
	web.Rolling.HealthRetries = 4
	web.Rolling.HealthIntervalSeconds = 7
	cfg.Services["web"] = web
	env := cfg.Environments["production"]
	actions, err := BuildActions(PlanInput{
		Config:      cfg,
		Environment: env,
		EnvName:     "production",
		ReleaseID:   "new",
		Images:      map[string]string{"web": "example/web@sha256:111"},
		StateDir:    t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var health Action
	for _, action := range actions {
		if action.Kind == ActionHealth {
			health = action
			break
		}
	}
	if health.Kind != ActionHealth {
		t.Fatalf("missing health action: %+v", actions)
	}
	if health.HealthRetries != 4 || health.HealthInterval != 7*time.Second {
		t.Fatalf("health retry settings = retries %d interval %s", health.HealthRetries, health.HealthInterval)
	}
}

func TestBuildActionsHonorsMaxUnavailableWhenSurgeDisabled(t *testing.T) {
	cfg := rollingStrategyConfig(1, 0)
	env := cfg.Environments["production"]
	actions, err := BuildActions(PlanInput{
		Config:      cfg,
		Environment: env,
		EnvName:     "production",
		ReleaseID:   "new",
		Images:      map[string]string{"web": "example/web@sha256:111"},
		StateDir:    t.TempDir(),
		Observed: []ObservedContainer{
			statusObserved("web-1", "web", 1, "old", "Up 1 minute"),
			statusObserved("web-2", "web", 2, "old", "Up 1 minute"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := actionKinds(actions)
	want := []ActionKind{
		ActionPull, ActionStop, ActionStart, ActionHealth,
		ActionPull, ActionStop, ActionStart, ActionHealth,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %#v, want %#v", got, want)
	}
	if actions[1].ContainerName != ContainerName("demo", "production", "web", 1, "old") {
		t.Fatalf("first pre-stop = %+v", actions[1])
	}
	if actions[5].ContainerName != ContainerName("demo", "production", "web", 2, "old") {
		t.Fatalf("second pre-stop = %+v", actions[5])
	}
}

func TestBuildActionsUsesHostContactsForIngressUpstreamsOnly(t *testing.T) {
	cfg := testConfig()
	env := cfg.Environments["production"]
	env.Hosts.Pools["ingress"] = config.Pool{Count: 1}
	actions, err := BuildActions(PlanInput{
		Config:      cfg,
		Environment: env,
		Hosts: []scheduler.Host{
			{Name: "ingress-1", Pool: "ingress", User: "root", Contact: "198.51.100.20"},
			{Name: "web-1", Pool: "web", User: "root", Contact: "198.51.100.10"},
		},
		EnvName:   "production",
		ReleaseID: "new",
		Images:    map[string]string{"web": "example/web@sha256:111"},
		StateDir:  "/tmp/ship-state",
	})
	if err != nil {
		t.Fatal(err)
	}
	var ingressAction *Action
	for i := range actions {
		if actions[i].Kind == ActionIngress {
			ingressAction = &actions[i]
			break
		}
	}
	if ingressAction == nil {
		t.Fatalf("missing ingress action: %+v", actions)
	}
	if !strings.Contains(ingressAction.IngressConfig, "reverse_proxy 198.51.100.10:3000") {
		t.Fatalf("contact upstream missing:\n%s", ingressAction.IngressConfig)
	}
	if strings.Contains(ingressAction.IngressConfig, "web-1:3000") {
		t.Fatalf("logical host name leaked into upstream:\n%s", ingressAction.IngressConfig)
	}
	if len(ingressAction.IngressHosts) != 1 || ingressAction.IngressHosts[0].Name != "ingress-1" {
		t.Fatalf("ingress hosts = %+v", ingressAction.IngressHosts)
	}
	if ingressAction.CaddyDataVolume != CaddyDataVolumeName("demo", "production") || ingressAction.CaddyConfigVolume != CaddyConfigVolumeName("demo", "production") {
		t.Fatalf("caddy volumes = data %q config %q", ingressAction.CaddyDataVolume, ingressAction.CaddyConfigVolume)
	}
}

func TestBuildActionsUsesConfiguredCaddyVolumes(t *testing.T) {
	cfg := testConfig()
	cfg.Ingress.Caddy.DataVolume = "custom-caddy-data"
	cfg.Ingress.Caddy.ConfigVolume = "custom-caddy-config"
	env := cfg.Environments["production"]
	actions, err := BuildActions(PlanInput{
		Config:      cfg,
		Environment: env,
		EnvName:     "production",
		ReleaseID:   "new",
		Images:      map[string]string{"web": "example/web@sha256:111"},
		StateDir:    t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var ingressAction *Action
	for i := range actions {
		if actions[i].Kind == ActionIngress {
			ingressAction = &actions[i]
			break
		}
	}
	if ingressAction == nil {
		t.Fatalf("missing ingress action: %+v", actions)
	}
	if ingressAction.CaddyDataVolume != "custom-caddy-data" || ingressAction.CaddyConfigVolume != "custom-caddy-config" {
		t.Fatalf("caddy volumes = data %q config %q", ingressAction.CaddyDataVolume, ingressAction.CaddyConfigVolume)
	}
}

func actionKinds(actions []Action) []ActionKind {
	kinds := make([]ActionKind, 0, len(actions))
	for _, action := range actions {
		kinds = append(kinds, action.Kind)
	}
	return kinds
}

func rollingStrategyConfig(maxUnavailable, maxSurge int) *config.Config {
	cfg := testConfig()
	env := cfg.Environments["production"]
	env.Hosts.Pools["web"] = config.Pool{Count: 2}
	cfg.Environments["production"] = env
	web := cfg.Services["web"]
	web.Scale = 2
	web.Ports = nil
	web.Ingress = nil
	web.Health = config.HealthCheck{Command: "bin/check"}
	web.Rolling = config.Rolling{MaxUnavailable: maxUnavailable, MaxSurge: maxSurge}
	cfg.Services["web"] = web
	return cfg
}

func TestAggregateStatusReportsMissingWrongReleaseAndExtras(t *testing.T) {
	cfg := testConfig()
	web := cfg.Services["web"]
	web.Scale = 2
	cfg.Services["web"] = web
	env := cfg.Environments["production"]
	env.Hosts.Pools["web"] = config.Pool{Count: 2}
	report, err := AggregateStatus(StatusInput{
		Config:         cfg,
		Environment:    env,
		EnvName:        "production",
		CurrentRelease: "new",
		Observed: []ObservedContainer{
			statusObserved("web-1", "web", 1, "new", "Up 10 seconds"),
			statusObserved("web-2", "web", 2, "old", "Up 2 minutes"),
			statusObserved("web-1", "worker", 1, "old", "Exited"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Desired) != 2 {
		t.Fatalf("desired = %+v", report.Desired)
	}
	if report.Desired[0].State != "ok" {
		t.Fatalf("web.1 state = %+v", report.Desired[0])
	}
	if report.Desired[1].State != "wrong_release" || !strings.Contains(strings.Join(report.Desired[1].Drift, ","), "old") {
		t.Fatalf("web.2 state = %+v", report.Desired[1])
	}
	if report.Summary.Missing != 1 || report.Summary.WrongRelease != 1 || report.Summary.Extra != 2 || !report.Summary.Drift {
		t.Fatalf("summary = %+v", report.Summary)
	}
	if report.ExtraObserved[0].Host != "web-1" || report.ExtraObserved[1].Host != "web-2" {
		t.Fatalf("extras = %+v", report.ExtraObserved)
	}
}

func TestAggregateStatusKeepsConfiguredAccessoryObservedWithoutDrift(t *testing.T) {
	cfg := testConfig()
	cfg.Accessories = map[string]config.Accessory{
		"postgres": {Image: "postgres:17", Pool: "web"},
	}
	env := cfg.Environments["production"]
	report, err := AggregateStatus(StatusInput{
		Config:         cfg,
		Environment:    env,
		EnvName:        "production",
		CurrentRelease: "new",
		Observed: []ObservedContainer{
			statusObserved("web-1", "web", 1, "new", "Up 10 seconds"),
			statusAccessoryObserved("web-1", "postgres", "Up 5 seconds"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Extra != 0 || report.Summary.Drift {
		t.Fatalf("summary = %+v", report.Summary)
	}
	if len(report.ExtraObserved) != 0 {
		t.Fatalf("extra observed = %+v", report.ExtraObserved)
	}
	foundAccessory := false
	for _, observed := range report.Observed {
		if observed.Accessory == "postgres" && observed.Kind == "accessory" {
			foundAccessory = true
		}
	}
	if !foundAccessory {
		t.Fatalf("observed did not include configured accessory: %+v", report.Observed)
	}
}

func TestAggregateStatusClassifiesManagedCaddyAsIngress(t *testing.T) {
	cfg := testConfig()
	env := cfg.Environments["production"]
	report, err := AggregateStatus(StatusInput{
		Config:         cfg,
		Environment:    env,
		EnvName:        "production",
		CurrentRelease: "new",
		Observed: []ObservedContainer{
			statusObserved("web-1", "web", 1, "new", "Up 10 seconds"),
			statusCaddyObserved("web-1", "Up 5 seconds"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Extra != 0 || report.Summary.Drift {
		t.Fatalf("summary = %+v", report.Summary)
	}
	if len(report.ExtraObserved) != 0 {
		t.Fatalf("extra observed = %+v", report.ExtraObserved)
	}
	foundIngress := false
	for _, observed := range report.Observed {
		if observed.Kind == "ingress" && observed.Name == CaddyContainerName("demo", "production") {
			foundIngress = true
			if observed.Service != "" || observed.Replica != 0 || observed.Release != "" {
				t.Fatalf("ingress status retained service drift fields: %+v", observed)
			}
		}
	}
	if !foundIngress {
		t.Fatalf("observed did not include ingress caddy: %+v", report.Observed)
	}
}

func TestBuildActionsStopsZeroScaleAndRemovedServiceContainers(t *testing.T) {
	cfg := testConfig()
	web := cfg.Services["web"]
	web.Scale = 0
	cfg.Services["web"] = web
	env := cfg.Environments["production"]
	actions, err := BuildActions(PlanInput{
		Config:      cfg,
		Environment: env,
		EnvName:     "production",
		ReleaseID:   "new",
		Images:      map[string]string{"web": "example/web@sha256:111"},
		Observed: []ObservedContainer{
			oldObserved("web-1", "web", "1", "old-web"),
			oldObserved("web-1", "worker", "1", "old-worker"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, action := range actions {
		got = append(got, string(action.Kind)+":"+action.ContainerName)
	}
	want := []string{
		"stop:" + ContainerName("demo", "production", "web", 1, "old-web"),
		"stop:" + ContainerName("demo", "production", "worker", 1, "old-worker"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("actions = %#v, want %#v", got, want)
	}
}

func TestBuildAndExecuteActionsClearsStaleIngressWhenCurrentConfigIsEmpty(t *testing.T) {
	dir := t.TempDir()
	ingressPath := filepath.Join(dir, "ingress", "production.Caddyfile")
	if err := os.MkdirAll(filepath.Dir(ingressPath), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := "example.com {\n  reverse_proxy web-1:3000\n}\n"
	if err := os.WriteFile(ingressPath, []byte(previous), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	web := cfg.Services["web"]
	web.Scale = 0
	cfg.Services["web"] = web
	env := cfg.Environments["production"]
	oldName := ContainerName("demo", "production", "web", 1, "old")
	actions, err := BuildActions(PlanInput{
		Config:      cfg,
		Environment: env,
		EnvName:     "production",
		ReleaseID:   "new",
		StateDir:    dir,
		Observed: []ObservedContainer{{
			Host: scheduler.Host{Name: "web-1", Pool: "web", User: "root"},
			Container: docker.ContainerSummary{
				Names: oldName,
				Labels: map[string]string{
					docker.LabelManagedBy:   docker.LabelManagedByValue,
					docker.LabelProject:     "demo",
					docker.LabelEnvironment: "production",
					docker.LabelService:     "web",
					docker.LabelReplica:     "1",
					docker.LabelRelease:     "old",
				},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 {
		t.Fatalf("actions = %+v, want clear ingress plus stop", actions)
	}
	if actions[0].Kind != ActionIngress || actions[0].IngressPath != ingressPath || actions[0].IngressConfig != "" {
		t.Fatalf("clear ingress action = %+v", actions[0])
	}
	if len(actions[0].IngressHosts) != 1 || actions[0].IngressHosts[0].Name != "web-1" {
		t.Fatalf("clear ingress hosts = %+v", actions[0].IngressHosts)
	}
	if actions[1].Kind != ActionStop || actions[1].ContainerName != oldName {
		t.Fatalf("stop action = %+v", actions[1])
	}

	fake := &ingressAgent{}
	if err := ExecuteActions(context.Background(), actions, func(host scheduler.Host) Agent {
		return fake
	}, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(ingressPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "" {
		t.Fatalf("local caddyfile = %q, want cleared", data)
	}
	if len(fake.reloads) != 0 {
		t.Fatalf("reloads = %#v, want none for clear", fake.reloads)
	}
	if len(fake.clears) != 0 {
		t.Fatalf("clears = %#v, want none for clear", fake.clears)
	}
	if len(fake.validated) != 0 {
		t.Fatalf("validated = %#v", fake.validated)
	}
}

func TestExecuteActionsUsesAgentMethodsInOrder(t *testing.T) {
	var slept time.Duration
	fake := &fakeAgent{}
	actions := []Action{
		{Kind: ActionPull, Host: scheduler.Host{Name: "web-1"}, Image: "example/web@sha256:111"},
		{Kind: ActionStart, Host: scheduler.Host{Name: "web-1"}, ContainerName: "ship_demo_production_web_1_new", Image: "example/web@sha256:111", Labels: map[string]string{docker.LabelProject: "demo"}, Network: "ship-demo-production", NetworkDriver: "bridge"},
		{Kind: ActionHealth, Host: scheduler.Host{Name: "web-1"}, ContainerName: "ship_demo_production_web_1_new", Health: agent.HealthCheckParams{URL: "http://127.0.0.1:3000/up"}},
		{Kind: ActionDrain, DrainTimeout: 2 * time.Second},
		{Kind: ActionStop, Host: scheduler.Host{Name: "web-1"}, ContainerName: "old"},
	}
	err := ExecuteActions(context.Background(), actions, func(host scheduler.Host) Agent {
		return fake
	}, func(d time.Duration) {
		slept += d
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pull", "ensure_network", "run_container", "health_check", "stop_container"}
	if !reflect.DeepEqual(fake.methods, want) {
		t.Fatalf("methods = %#v, want %#v", fake.methods, want)
	}
	if slept != 2*time.Second {
		t.Fatalf("slept = %v", slept)
	}
}

func TestExecuteActionsRetriesHealthCheck(t *testing.T) {
	fake := &retryHealthAgent{failuresBeforeOK: 2}
	var sleeps []time.Duration
	action := Action{
		Kind:           ActionHealth,
		Host:           scheduler.Host{Name: "web-1"},
		ContainerName:  "ship_demo_production_web_1_new",
		Health:         agent.HealthCheckParams{URL: "http://127.0.0.1:3000/up"},
		HealthRetries:  3,
		HealthInterval: 150 * time.Millisecond,
	}
	err := ExecuteActions(context.Background(), []Action{action}, func(host scheduler.Host) Agent {
		return fake
	}, func(d time.Duration) {
		sleeps = append(sleeps, d)
	})
	if err != nil {
		t.Fatal(err)
	}
	if fake.healthChecks != 3 {
		t.Fatalf("health checks = %d", fake.healthChecks)
	}
	wantSleeps := []time.Duration{150 * time.Millisecond, 150 * time.Millisecond}
	if !reflect.DeepEqual(sleeps, wantSleeps) {
		t.Fatalf("sleeps = %#v, want %#v", sleeps, wantSleeps)
	}
}

func TestExecuteFixedPortActionsAvoidsPortCollision(t *testing.T) {
	cfg := testConfig()
	env := cfg.Environments["production"]
	oldName := ContainerName("demo", "production", "web", 1, "old")
	actions, err := BuildActions(PlanInput{
		Config:      cfg,
		Environment: env,
		EnvName:     "production",
		ReleaseID:   "new",
		Images:      map[string]string{"web": "example/web@sha256:111"},
		StateDir:    t.TempDir(),
		Observed: []ObservedContainer{{
			Host: scheduler.Host{Name: "web-1", Pool: "web", User: "root"},
			Container: docker.ContainerSummary{
				Names: oldName,
				Labels: map[string]string{
					docker.LabelManagedBy:   docker.LabelManagedByValue,
					docker.LabelProject:     "demo",
					docker.LabelEnvironment: "production",
					docker.LabelService:     "web",
					docker.LabelReplica:     "1",
					docker.LabelRelease:     "old",
				},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fixedPortAgent{
		activePorts: map[int]string{3000: oldName},
	}
	if err := ExecuteActions(context.Background(), actions, func(host scheduler.Host) Agent {
		return fake
	}, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"pull",
		"stop_container:" + oldName,
		"run_container:" + ContainerName("demo", "production", "web", 1, "new"),
		"health_check",
	}
	for _, event := range want {
		if !contains(fake.events, event) {
			t.Fatalf("events missing %q: %#v", event, fake.events)
		}
	}
}

func TestExecuteIngressActionReloadsHostsAndWritesLocalState(t *testing.T) {
	dir := t.TempDir()
	ingressPath := filepath.Join(dir, "ingress", "production.Caddyfile")
	action := Action{
		Kind:          ActionIngress,
		IngressPath:   ingressPath,
		IngressConfig: "example.com {\n  reverse_proxy web-1:3000\n}\n",
		IngressHosts:  []scheduler.Host{{Name: "ingress-1"}, {Name: "ingress-2"}},
	}
	agents := map[string]*ingressAgent{}
	err := ExecuteActions(context.Background(), []Action{action}, func(host scheduler.Host) Agent {
		if agents[host.Name] == nil {
			agents[host.Name] = &ingressAgent{host: host.Name}
		}
		return agents[host.Name]
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(ingressPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != action.IngressConfig {
		t.Fatalf("local caddyfile = %q", data)
	}
	for _, host := range []string{"ingress-1", "ingress-2"} {
		if len(agents[host].reloads) != 1 || agents[host].reloads[0] != action.IngressConfig {
			t.Fatalf("%s reloads = %#v", host, agents[host].reloads)
		}
		if len(agents[host].runs) != 1 {
			t.Fatalf("%s runs = %#v", host, agents[host].runs)
		}
		runArgs := strings.Join(agents[host].runs[0].Args, " ")
		for _, needle := range []string{
			"--restart unless-stopped",
			"-p 443:443/udp",
			"-v ship_caddy_data:/data",
			"-v ship_caddy_config:/config",
		} {
			if !strings.Contains(runArgs, needle) {
				t.Fatalf("%s run args %q missing %q", host, runArgs, needle)
			}
		}
		if !agents[host].validated[0] {
			t.Fatalf("%s reload was not validated", host)
		}
	}
}

func TestExecuteIngressActionRollsBackReloadedHostsOnFailure(t *testing.T) {
	dir := t.TempDir()
	ingressPath := filepath.Join(dir, "ingress", "production.Caddyfile")
	if err := os.MkdirAll(filepath.Dir(ingressPath), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := "example.com {\n  reverse_proxy web-old:3000\n}\n"
	if err := os.WriteFile(ingressPath, []byte(previous), 0o644); err != nil {
		t.Fatal(err)
	}
	action := Action{
		Kind:          ActionIngress,
		IngressPath:   ingressPath,
		IngressConfig: "example.com {\n  reverse_proxy web-new:3000\n}\n",
		IngressHosts:  []scheduler.Host{{Name: "ingress-1"}, {Name: "ingress-2"}},
	}
	agents := map[string]*ingressAgent{
		"ingress-1": {host: "ingress-1"},
		"ingress-2": {host: "ingress-2", failReload: true},
	}
	err := ExecuteActions(context.Background(), []Action{action}, func(host scheduler.Host) Agent {
		return agents[host.Name]
	}, nil)
	if err == nil {
		t.Fatal("expected reload failure")
	}
	data, readErr := os.ReadFile(ingressPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != previous {
		t.Fatalf("local caddyfile changed after failure: %q", data)
	}
	if !reflect.DeepEqual(agents["ingress-1"].reloads, []string{action.IngressConfig, previous}) {
		t.Fatalf("ingress-1 reloads = %#v", agents["ingress-1"].reloads)
	}
	if !reflect.DeepEqual(agents["ingress-2"].reloads, []string{action.IngressConfig}) {
		t.Fatalf("ingress-2 reloads = %#v", agents["ingress-2"].reloads)
	}
}

func TestExecuteActionsDoesNotReloadIngressAfterHealthFailure(t *testing.T) {
	fake := &ingressAgent{failHealth: true}
	actions := []Action{
		{Kind: ActionStart, Host: scheduler.Host{Name: "web-1"}, ContainerName: "new", Image: "example/web:1"},
		{Kind: ActionHealth, Host: scheduler.Host{Name: "web-1"}, ContainerName: "new", Health: agent.HealthCheckParams{URL: "http://127.0.0.1:3000/up"}},
		{Kind: ActionIngress, IngressPath: filepath.Join(t.TempDir(), "Caddyfile"), IngressConfig: "example.com {\n  reverse_proxy web-1:3000\n}\n", IngressHosts: []scheduler.Host{{Name: "ingress-1"}}},
	}
	err := ExecuteActions(context.Background(), actions, func(host scheduler.Host) Agent {
		return fake
	}, nil)
	if err == nil {
		t.Fatal("expected health failure")
	}
	if len(fake.reloads) != 0 {
		t.Fatalf("ingress reloaded after failed health: %#v", fake.reloads)
	}
}

func TestExecuteActionsSleepsForCanaryPause(t *testing.T) {
	var sleeps []time.Duration
	err := ExecuteActions(context.Background(), []Action{
		{Kind: ActionCanary, Service: "web", Replica: 1, PauseDuration: 12 * time.Second},
	}, func(host scheduler.Host) Agent {
		return &ingressAgent{}
	}, func(duration time.Duration) {
		sleeps = append(sleeps, duration)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sleeps, []time.Duration{12 * time.Second}) {
		t.Fatalf("sleeps = %#v", sleeps)
	}
}

func TestHealthCheckCommandRunsInsideContainer(t *testing.T) {
	params, ok, err := HealthCheck(config.Service{
		Health:  config.HealthCheck{Command: "bin/check 'quoted'"},
		Rolling: config.Rolling{HealthTimeoutSeconds: 7},
	}, "ship_demo_web")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected health check")
	}
	if params.TimeoutSeconds != 7 || !strings.Contains(params.Command, "docker exec 'ship_demo_web' sh -lc 'bin/check '") {
		t.Fatalf("params = %+v", params)
	}
}

func TestDockerArgsIncludesExplicitEnvAndEnvFile(t *testing.T) {
	args := DockerArgs(config.Service{
		Env:   []string{"RACK_ENV=production", ""},
		Ports: []int{3000},
		Publish: []string{
			"127.0.0.1:8080:80",
			"5353:5353/udp",
		},
		Volumes: []string{
			"uploads:/app/uploads",
			"/srv/config:/app/config:ro",
		},
		Resources: config.ResourceConfig{
			CPUs:              "1.5",
			Memory:            "512m",
			MemoryReservation: "256m",
			MemorySwap:        "1g",
			CPUShares:         512,
			CPUSetCPUs:        "0,1",
			PIDsLimit:         256,
		},
		Runtime: config.RuntimeConfig{
			ReadOnly:           true,
			Init:               true,
			User:               "1000:1000",
			Workdir:            "/app",
			Hostname:           "web-runtime",
			Entrypoint:         "/docker-entrypoint",
			IPC:                "host",
			PID:                "container:sidecar",
			CgroupNS:           "host",
			StopSignal:         "SIGTERM",
			StopTimeoutSeconds: 30,
			ShmSize:            "1g",
			GPUs:               "all",
			HealthCMD:          "curl -fsS http://127.0.0.1:3000/up || exit 1",
			HealthInterval:     "10s",
			HealthTimeout:      "3s",
			HealthStartPeriod:  "20s",
			HealthRetries:      3,
			CapAdd:             []string{"SYS_PTRACE"},
			CapDrop:            []string{"NET_RAW"},
			GroupAdd:           []string{"video"},
			SecurityOpt:        []string{"no-new-privileges:true"},
			Sysctls:            map[string]string{"net.core.somaxconn": "1024"},
			Ulimits:            []string{"nofile=262144:262144"},
			Mounts:             []string{"type=bind,source=/srv/cache,target=/cache,readonly"},
			AddHosts:           []string{"host.docker.internal:host-gateway"},
			DNS:                []string{"1.1.1.1"},
			DNSSearch:          []string{"svc.local"},
			DNSOptions:         []string{"ndots:1"},
			Devices:            []string{"/dev/fuse:/dev/fuse"},
			DeviceCgroupRules:  []string{"c 10:229 rwm"},
			Tmpfs:              []string{"/tmp:rw,noexec,nosuid,size=64m"},
		},
		Logging: config.LoggingConfig{
			Driver: "json-file",
			Options: map[string]string{
				"max-size": "10m",
				"max-file": "3",
			},
		},
	}, "/var/lib/ship/secrets/production/service-web.env")

	want := []string{
		"-e",
		"RACK_ENV=production",
		"--env-file",
		"/var/lib/ship/secrets/production/service-web.env",
		"--restart",
		"unless-stopped",
		"-v",
		"uploads:/app/uploads",
		"-v",
		"/srv/config:/app/config:ro",
		"--cpus",
		"1.5",
		"--memory",
		"512m",
		"--memory-reservation",
		"256m",
		"--memory-swap",
		"1g",
		"--cpu-shares",
		"512",
		"--cpuset-cpus",
		"0,1",
		"--pids-limit",
		"256",
		"--read-only",
		"--init",
		"--user",
		"1000:1000",
		"--workdir",
		"/app",
		"--hostname",
		"web-runtime",
		"--entrypoint",
		"/docker-entrypoint",
		"--ipc",
		"host",
		"--pid",
		"container:sidecar",
		"--cgroupns",
		"host",
		"--stop-signal",
		"SIGTERM",
		"--stop-timeout",
		"30",
		"--shm-size",
		"1g",
		"--gpus",
		"all",
		"--health-cmd",
		"curl -fsS http://127.0.0.1:3000/up || exit 1",
		"--health-interval",
		"10s",
		"--health-timeout",
		"3s",
		"--health-start-period",
		"20s",
		"--health-retries",
		"3",
		"--cap-add",
		"SYS_PTRACE",
		"--cap-drop",
		"NET_RAW",
		"--group-add",
		"video",
		"--security-opt",
		"no-new-privileges:true",
		"--ulimit",
		"nofile=262144:262144",
		"--mount",
		"type=bind,source=/srv/cache,target=/cache,readonly",
		"--add-host",
		"host.docker.internal:host-gateway",
		"--dns",
		"1.1.1.1",
		"--dns-search",
		"svc.local",
		"--dns-option",
		"ndots:1",
		"--device",
		"/dev/fuse:/dev/fuse",
		"--device-cgroup-rule",
		"c 10:229 rwm",
		"--tmpfs",
		"/tmp:rw,noexec,nosuid,size=64m",
		"--sysctl",
		"net.core.somaxconn=1024",
		"--log-driver",
		"json-file",
		"--log-opt",
		"max-file=3",
		"--log-opt",
		"max-size=10m",
		"-p",
		"3000:3000",
		"-p",
		"127.0.0.1:8080:80",
		"-p",
		"5353:5353/udp",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
}

func TestDockerArgsIncludesCustomRestartPolicy(t *testing.T) {
	args := DockerArgs(config.Service{RestartPolicy: "on-failure:3"})
	want := []string{"--restart", "on-failure:3"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
}

func TestDockerOneOffArgsOmitRestartPolicyAndPorts(t *testing.T) {
	args := DockerOneOffArgs(config.Service{
		RestartPolicy: "always",
		Ports:         []int{3000},
		Env:           []string{"RACK_ENV=production"},
		Runtime:       config.RuntimeConfig{Privileged: true},
	})
	want := []string{"-e", "RACK_ENV=production", "--privileged"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
}

func testConfig() *config.Config {
	return &config.Config{
		Project:  "demo",
		Registry: "example/demo",
		Environments: map[string]config.Environment{
			"production": {
				Hosts: config.HostsConfig{Pools: map[string]config.Pool{
					"web": {Count: 1},
				}},
			},
		},
		Services: map[string]config.Service{
			"web": {
				Image:   config.ImageSpec{Ref: "example/web:latest"},
				Command: "./bin/server",
				Pool:    "web",
				Scale:   1,
				Ports:   []int{3000},
				Health:  config.HealthCheck{HTTP: "/up"},
				Ingress: &config.Ingress{Domains: []string{"example.com"}},
			},
		},
	}
}

func oldObserved(host, service, replica, release string) ObservedContainer {
	return ObservedContainer{
		Host: scheduler.Host{Name: host, Pool: "web", User: "root"},
		Container: docker.ContainerSummary{
			Names: ContainerName("demo", "production", service, 1, release),
			Labels: map[string]string{
				docker.LabelManagedBy:   docker.LabelManagedByValue,
				docker.LabelProject:     "demo",
				docker.LabelEnvironment: "production",
				docker.LabelService:     service,
				docker.LabelReplica:     replica,
				docker.LabelRelease:     release,
			},
		},
	}
}

func statusObserved(host, service string, replica int, release, status string) ObservedContainer {
	return ObservedContainer{
		Host: scheduler.Host{Name: host, Pool: "web", User: "root"},
		Container: docker.ContainerSummary{
			Names:  ContainerName("demo", "production", service, replica, release),
			Image:  "example/" + service + ":" + release,
			Status: status,
			Labels: map[string]string{
				docker.LabelManagedBy:   docker.LabelManagedByValue,
				docker.LabelProject:     "demo",
				docker.LabelEnvironment: "production",
				docker.LabelService:     service,
				docker.LabelReplica:     fmt.Sprint(replica),
				docker.LabelRelease:     release,
			},
		},
	}
}

func statusAccessoryObserved(host, name, status string) ObservedContainer {
	return ObservedContainer{
		Host: scheduler.Host{Name: host, Pool: "web", User: "root"},
		Container: docker.ContainerSummary{
			Names:  "ship_demo_production_accessory_" + safeNamePart(name),
			Image:  "postgres:17",
			Status: status,
			Labels: map[string]string{
				docker.LabelManagedBy:   docker.LabelManagedByValue,
				docker.LabelProject:     "demo",
				docker.LabelEnvironment: "production",
				docker.LabelAccessory:   safeLabelValue(name),
			},
		},
	}
}

func statusCaddyObserved(host, status string) ObservedContainer {
	return ObservedContainer{
		Host: scheduler.Host{Name: host, Pool: "web", User: "root"},
		Container: docker.ContainerSummary{
			Names:  CaddyContainerName("demo", "production"),
			Image:  "caddy:2",
			Status: status,
			Labels: map[string]string{
				docker.LabelManagedBy:   docker.LabelManagedByValue,
				docker.LabelProject:     "demo",
				docker.LabelEnvironment: "production",
				docker.LabelService:     "caddy",
			},
		},
	}
}

type fakeAgent struct {
	methods []string
}

func (f *fakeAgent) Call(ctx context.Context, method string, params any, out any) error {
	f.methods = append(f.methods, method)
	if result, ok := out.(*agent.HealthCheckResult); ok {
		*result = agent.HealthCheckResult{OK: true}
	}
	return nil
}

type retryHealthAgent struct {
	healthChecks     int
	failuresBeforeOK int
}

func (f *retryHealthAgent) Call(ctx context.Context, method string, params any, out any) error {
	if method != "health_check" {
		return fmt.Errorf("unexpected method %s", method)
	}
	f.healthChecks++
	if f.healthChecks <= f.failuresBeforeOK {
		if result, ok := out.(*agent.HealthCheckResult); ok {
			*result = agent.HealthCheckResult{OK: false}
		}
		return nil
	}
	if result, ok := out.(*agent.HealthCheckResult); ok {
		*result = agent.HealthCheckResult{OK: true}
	}
	return nil
}

type fixedPortAgent struct {
	activePorts map[int]string
	events      []string
}

func (f *fixedPortAgent) Call(ctx context.Context, method string, params any, out any) error {
	switch method {
	case "pull":
		f.events = append(f.events, "pull")
	case "run_oneoff_container", "write_file", "ensure_network":
		return nil
	case "docker_inspect":
		if result, ok := out.(*agent.DockerInspectResult); ok {
			result.Inspect = json.RawMessage(`[{"State":{"Running":true}}]`)
		}
		return nil
	case "run_container":
		p := params.(agent.RunContainerParams)
		f.events = append(f.events, "run_container:"+p.Name)
		for i := 0; i < len(p.Args)-1; i++ {
			if p.Args[i] != "-p" {
				continue
			}
			if p.Args[i+1] == "3000:3000" {
				if holder := f.activePorts[3000]; holder != "" {
					return fmt.Errorf("port 3000 already allocated by %s", holder)
				}
				f.activePorts[3000] = p.Name
			}
		}
	case "health_check":
		f.events = append(f.events, "health_check")
		if result, ok := out.(*agent.HealthCheckResult); ok {
			*result = agent.HealthCheckResult{OK: true}
		}
	case "stop_container":
		name := params.(map[string]string)["name"]
		f.events = append(f.events, "stop_container:"+name)
		for port, holder := range f.activePorts {
			if holder == name {
				delete(f.activePorts, port)
			}
		}
	default:
		f.events = append(f.events, method)
	}
	return nil
}

type ingressAgent struct {
	host       string
	reloads    []string
	runs       []agent.RunContainerParams
	validated  []bool
	clears     []bool
	failReload bool
	failHealth bool
}

func (f *ingressAgent) Call(ctx context.Context, method string, params any, out any) error {
	switch method {
	case "write_file":
		p := params.(agent.WriteFileParams)
		f.reloads = append(f.reloads, p.Content)
		f.clears = append(f.clears, strings.TrimSpace(p.Content) == "")
	case "run_oneoff_container":
		f.validated = append(f.validated, true)
		if f.failReload {
			return fmt.Errorf("validate failed on %s", f.host)
		}
	case "run_container":
		if f.failReload {
			return fmt.Errorf("reload failed on %s", f.host)
		}
		f.runs = append(f.runs, params.(agent.RunContainerParams))
	case "docker_inspect":
		if result, ok := out.(*agent.DockerInspectResult); ok {
			result.Inspect = json.RawMessage(`[{"State":{"Running":true}}]`)
		}
	case "ensure_network":
		return nil
	case "caddy_reload":
		p := params.(agent.CaddyReloadParams)
		f.reloads = append(f.reloads, p.Config)
		f.validated = append(f.validated, p.Validate)
		f.clears = append(f.clears, p.Clear)
		if f.failReload {
			return fmt.Errorf("reload failed on %s", f.host)
		}
	case "health_check":
		if f.failHealth {
			return fmt.Errorf("health failed")
		}
		if result, ok := out.(*agent.HealthCheckResult); ok {
			*result = agent.HealthCheckResult{OK: true}
		}
	case "pull", "stop_container", "list_ship_containers":
		return nil
	default:
		return fmt.Errorf("unexpected method %s", method)
	}
	return nil
}

func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
