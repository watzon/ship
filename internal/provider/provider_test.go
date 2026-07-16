package provider

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/watzon/ship/internal/config"
)

func TestHostPlansUsesEnvironmentPools(t *testing.T) {
	env := config.Environment{
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"worker": {Count: 1, User: "deploy"},
			"web":    {Count: 2},
		}},
	}

	plans := HostPlans("demo", "production", env, HostPlanOptions{Location: "ash", Size: "small", Image: "ubuntu", UserData: "#cloud-config\n"})

	if len(plans) != 3 {
		t.Fatalf("plans = %+v", plans)
	}
	if plans[0].Name != "web-1" || plans[0].Pool != "web" || plans[0].User != "root" {
		t.Fatalf("first plan = %+v", plans[0])
	}
	if plans[2].Name != "worker-1" || plans[2].Pool != "worker" || plans[2].User != "deploy" {
		t.Fatalf("worker plan = %+v", plans[2])
	}
	if plans[0].Location != "ash" || plans[0].Size != "small" || plans[0].Image != "ubuntu" || plans[0].UserData != "#cloud-config\n" {
		t.Fatalf("provider options missing from plan: %+v", plans[0])
	}
	wantLabels := map[string]string{
		LabelManagedBy:   "ship",
		LabelProject:     "demo",
		LabelEnvironment: "production",
		LabelPool:        "web",
		LabelHost:        "web-1",
	}
	if !reflect.DeepEqual(plans[0].Labels, wantLabels) {
		t.Fatalf("labels = %+v, want %+v", plans[0].Labels, wantLabels)
	}
}

func TestHostPlansAppliesPoolShapeOverrides(t *testing.T) {
	env := config.Environment{
		Hosts: config.HostsConfig{
			Labels: map[string]string{
				"owner":       "platform",
				"cost-center": "shared",
			},
			Pools: map[string]config.Pool{
				"worker": {
					Count:    1,
					Location: "hil",
					Size:     "large",
					Image:    "ubuntu-worker",
					UserData: "#cloud-config\npackages: [htop]\n",
					Labels: map[string]string{
						"cost-center": "batch",
						"workload":    "jobs",
					},
				},
				"web": {Count: 1},
			},
		},
	}

	plans := HostPlans("demo", "production", env, HostPlanOptions{Location: "ash", Size: "small", Image: "ubuntu", UserData: "#cloud-config\n"})

	if len(plans) != 2 {
		t.Fatalf("plans = %+v", plans)
	}
	if plans[0].Pool != "web" || plans[0].Location != "ash" || plans[0].Size != "small" || plans[0].Image != "ubuntu" || plans[0].UserData != "#cloud-config\n" {
		t.Fatalf("web plan = %+v", plans[0])
	}
	if plans[1].Pool != "worker" || plans[1].Location != "hil" || plans[1].Size != "large" || plans[1].Image != "ubuntu-worker" || plans[1].UserData != "#cloud-config\npackages: [htop]\n" {
		t.Fatalf("worker plan = %+v", plans[1])
	}
	if plans[0].Labels["owner"] != "platform" || plans[0].Labels["cost-center"] != "shared" {
		t.Fatalf("web labels = %+v", plans[0].Labels)
	}
	if plans[1].Labels["owner"] != "platform" || plans[1].Labels["cost-center"] != "batch" || plans[1].Labels["workload"] != "jobs" {
		t.Fatalf("worker labels = %+v", plans[1].Labels)
	}
	if plans[1].Labels[LabelManagedBy] != "ship" || plans[1].Labels[LabelPool] != "worker" {
		t.Fatalf("ship labels missing/protected: %+v", plans[1].Labels)
	}
}

func TestReconcileHostsCreatesMissingAndReportsExtra(t *testing.T) {
	backend := &fakeBackend{
		existing: []Host{
			{ID: "1", Name: "web-1", Pool: "web"},
			{ID: "2", Name: "web-old", Pool: "web"},
		},
	}
	desired := []HostPlan{
		{Name: "web-1", Pool: "web"},
		{Name: "web-2", Pool: "web"},
	}

	result, err := ReconcileHosts(context.Background(), "demo", "production", desired, backend)
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Existing) != 1 || result.Existing[0].Name != "web-1" {
		t.Fatalf("existing = %+v", result.Existing)
	}
	if len(result.Created) != 1 || result.Created[0].Name != "web-2" {
		t.Fatalf("created = %+v", result.Created)
	}
	if len(result.Extra) != 1 || result.Extra[0].Name != "web-old" {
		t.Fatalf("extra = %+v", result.Extra)
	}
	if got := backend.created; !reflect.DeepEqual(got, []string{"web-2"}) {
		t.Fatalf("created requests = %+v", got)
	}
}

func TestReconcileHostsMatchesReplacementByHostLabel(t *testing.T) {
	backend := &fakeBackend{
		existing: []Host{
			{ID: "1", Name: "web-1", Pool: "web"},
			{ID: "2", Name: "web-2-m20260715", Pool: "web", Labels: map[string]string{LabelHost: "web-2"}},
		},
	}
	desired := []HostPlan{
		{Name: "web-1", Pool: "web"},
		{Name: "web-2", Pool: "web"},
	}

	result, err := ReconcileHosts(context.Background(), "demo", "production", desired, backend)
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Existing) != 2 {
		t.Fatalf("existing = %+v", result.Existing)
	}
	if result.Existing[1].Name != "web-2-m20260715" {
		t.Fatalf("replacement not matched by label: %+v", result.Existing)
	}
	if len(result.Created) != 0 || len(result.Extra) != 0 {
		t.Fatalf("created = %+v extra = %+v", result.Created, result.Extra)
	}
}

func TestReconcileHostsPrefersNameMatchOverLabelDuplicate(t *testing.T) {
	backend := &fakeBackend{
		existing: []Host{
			{ID: "2", Name: "web-2-m20260715", Pool: "web", Labels: map[string]string{LabelHost: "web-2"}},
			{ID: "1", Name: "web-2", Pool: "web", Labels: map[string]string{LabelHost: "web-2"}},
		},
	}
	desired := []HostPlan{{Name: "web-2", Pool: "web"}}

	result, err := ReconcileHosts(context.Background(), "demo", "production", desired, backend)
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Existing) != 1 || result.Existing[0].Name != "web-2" {
		t.Fatalf("existing = %+v", result.Existing)
	}
	if len(result.Created) != 0 {
		t.Fatalf("created = %+v", result.Created)
	}
	if len(result.Extra) != 1 || result.Extra[0].Name != "web-2-m20260715" {
		t.Fatalf("extra = %+v", result.Extra)
	}
}

func TestLogicalName(t *testing.T) {
	if got := LogicalName(Host{Name: "web-1"}); got != "web-1" {
		t.Fatalf("LogicalName = %q", got)
	}
	if got := LogicalName(Host{Name: "web-2-m1", Labels: map[string]string{LabelHost: "web-2"}}); got != "web-2" {
		t.Fatalf("LogicalName = %q", got)
	}
}

func TestReconcileHostsPropagatesBackendErrors(t *testing.T) {
	wantErr := errors.New("list failed")
	_, err := ReconcileHosts(context.Background(), "demo", "production", nil, &fakeBackend{listErr: wantErr})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

type fakeBackend struct {
	existing []Host
	created  []string
	listErr  error
}

func (f *fakeBackend) List(context.Context, string, string) ([]Host, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]Host(nil), f.existing...), nil
}

func (f *fakeBackend) Create(_ context.Context, plan HostPlan) (Host, error) {
	f.created = append(f.created, plan.Name)
	return Host{ID: "new-" + plan.Name, Name: plan.Name, Pool: plan.Pool, PublicAddress: "192.0.2.10"}, nil
}
