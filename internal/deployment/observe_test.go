package deployment

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/scheduler"
)

func TestExecuteActionsObservedReportsEachOperationWithItsOwnDuration(t *testing.T) {
	clock := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	health := &retryHealthAgent{failuresBeforeOK: 2}
	actions := []Action{
		{Kind: ActionStart, Host: scheduler.Host{Name: "web-1"}, Service: "web", Replica: 1, ContainerName: "web_1", Image: "img"},
		{Kind: ActionHealth, Host: scheduler.Host{Name: "web-1"}, Service: "web", Replica: 1, ContainerName: "web_1", Health: agent.HealthCheckParams{URL: "http://127.0.0.1/up"}, HealthRetries: 3, HealthInterval: 2 * time.Second},
		{Kind: ActionIngress, IngressPath: filepath.Join(t.TempDir(), "i", "production.Caddyfile"), IngressConfig: "example.com {\n}\n", IngressHosts: []scheduler.Host{{Name: "ingress-1"}, {Name: "ingress-2"}}},
	}
	agents := map[string]*ingressAgent{}
	var events []StepEvent
	_, err := ExecuteActionsObserved(context.Background(), actions, func(host scheduler.Host) Agent {
		switch host.Name {
		case "web-1":
			return advancingAgent{inner: health, clock: &clock, cost: map[string]time.Duration{"run_container": 4 * time.Second, "health_check": time.Second}}
		}
		if agents[host.Name] == nil {
			agents[host.Name] = &ingressAgent{host: host.Name}
		}
		return advancingAgent{inner: agents[host.Name], clock: &clock, cost: map[string]time.Duration{"run_container": 3 * time.Second}}
	}, func(d time.Duration) { clock = clock.Add(d) }, func(e StepEvent) { events = append(events, e) }, now)
	if err != nil {
		t.Fatal(err)
	}
	type brief struct {
		Kind    ActionKind
		Host    string
		Checks  int
		Elapsed time.Duration
	}
	var got []brief
	for _, e := range events {
		got = append(got, brief{e.Kind, e.Host, e.Checks, e.Elapsed})
	}
	want := []brief{
		{ActionStart, "web-1", 0, 4 * time.Second},
		// 3 checks of 1s each plus two 2s waits between them.
		{ActionHealth, "web-1", 3, 7 * time.Second},
		{ActionIngress, "ingress-1", 0, 3 * time.Second},
		{ActionIngress, "ingress-2", 0, 3 * time.Second},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %+v\nwant   %+v", got, want)
	}
}

func TestExecuteActionsObservedReportsFailureAfterEarlierCompletions(t *testing.T) {
	actions := []Action{
		{Kind: ActionStart, Host: scheduler.Host{Name: "web-1"}, Service: "web", Replica: 1, ContainerName: "web_1", Image: "img"},
		{Kind: ActionStart, Host: scheduler.Host{Name: "web-2"}, Service: "web", Replica: 2, ContainerName: "web_2", Image: "img"},
	}
	var events []StepEvent
	_, err := ExecuteActionsObserved(context.Background(), actions, func(host scheduler.Host) Agent {
		return &failOnHostAgent{host: host.Name, failHost: "web-2"}
	}, nil, func(e StepEvent) { events = append(events, e) }, nil)
	if err == nil {
		t.Fatal("want error")
	}
	if len(events) != 2 || events[0].Err != nil || events[0].Replica != 1 || events[1].Err == nil || events[1].Replica != 2 {
		t.Fatalf("events = %+v", events)
	}
}

type advancingAgent struct {
	inner Agent
	clock *time.Time
	cost  map[string]time.Duration
}

func (a advancingAgent) Call(ctx context.Context, method string, params any, out any) error {
	*a.clock = a.clock.Add(a.cost[method])
	if method == "run_container" && a.inner == nil {
		return nil
	}
	if _, ok := a.inner.(*retryHealthAgent); ok && method != "health_check" {
		return nil
	}
	return a.inner.Call(ctx, method, params, out)
}

type failOnHostAgent struct {
	host, failHost string
}

func (a *failOnHostAgent) Call(ctx context.Context, method string, params any, out any) error {
	if method == "run_container" && a.host == a.failHost {
		return errors.New("port is already allocated")
	}
	return nil
}
