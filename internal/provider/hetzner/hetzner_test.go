package hetzner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

var _ provider.Provider = Client{}

func TestDesiredServersUsesPools(t *testing.T) {
	env := testEnvironment(2)
	servers := DesiredServers(env)
	if len(servers) != 2 {
		t.Fatalf("len = %d", len(servers))
	}
	if servers[0].Name != "web-1" || servers[0].Pool != "web" {
		t.Fatalf("unexpected server: %+v", servers[0])
	}
}

func TestDesiredServerLabelsIncludeShipLabels(t *testing.T) {
	servers := DesiredServersFor("demo", "production", testEnvironment(1))
	if len(servers) != 1 {
		t.Fatalf("len = %d", len(servers))
	}
	labels := servers[0].Labels
	want := map[string]string{
		LabelManagedBy:   "ship",
		LabelProject:     "demo",
		LabelEnvironment: "production",
		LabelPool:        "web",
	}
	for key, value := range want {
		if labels[key] != value {
			t.Fatalf("label %s = %q, want %q in %+v", key, labels[key], value, labels)
		}
	}
}

func TestReconcileCreatesMissingServers(t *testing.T) {
	api := newFakeHetznerAPI(t, nil)
	result, err := api.client().Reconcile(context.Background(), "demo", "production", testEnvironment(1))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("created = %+v", result.Created)
	}
	if len(result.Existing) != 0 || len(result.Extra) != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(api.creates) != 1 {
		t.Fatalf("creates = %d", len(api.creates))
	}
	if api.selector != "managed-by=ship,project=demo,environment=production" {
		t.Fatalf("label selector = %q", api.selector)
	}
	create := api.creates[0]
	if create.Labels[LabelManagedBy] != "ship" || create.Labels[LabelProject] != "demo" || create.Labels[LabelEnvironment] != "production" || create.Labels[LabelPool] != "web" {
		t.Fatalf("labels = %+v", create.Labels)
	}
	if create.UserData != "#cloud-config\npackages: [htop]\n" {
		t.Fatalf("user_data = %q", create.UserData)
	}
	if len(create.SSHKeys) != 1 || create.SSHKeys[0] != "ship-key" {
		t.Fatalf("ssh_keys = %+v", create.SSHKeys)
	}
	if len(api.networks) != 1 || len(api.firewalls) != 1 {
		t.Fatalf("network/firewall creates = %d/%d", len(api.networks), len(api.firewalls))
	}
	if len(create.Networks) != 1 || create.Networks[0] != 300 {
		t.Fatalf("networks = %+v", create.Networks)
	}
	if len(create.Firewalls) != 1 || create.Firewalls[0]["firewall"] != 400 {
		t.Fatalf("firewalls = %+v", create.Firewalls)
	}
	if result.Created[0].ID == "" || result.Created[0].PublicAddress == "" {
		t.Fatalf("created server missing facts: %+v", result.Created[0])
	}
}

func TestReconcileLeavesMatchingServersUnchanged(t *testing.T) {
	existing := []Server{{
		ID:         10,
		Name:       "web-1",
		Labels:     shipLabels("demo", "production", "web"),
		PrivateNet: []PrivateNet{{Network: 300}},
		PublicNet:  PublicNet{Firewalls: []ServerFirewall{{ID: 400, Status: "applied"}}},
	}}
	api := newFakeHetznerAPI(t, existing)
	result, err := api.client().Reconcile(context.Background(), "demo", "production", testEnvironment(1))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Existing) != 1 || result.Existing[0].ID != "10" {
		t.Fatalf("existing = %+v", result.Existing)
	}
	if len(result.Created) != 0 || len(api.creates) != 0 {
		t.Fatalf("created result=%+v requests=%+v", result.Created, api.creates)
	}
	if len(api.networkAttachments) != 0 || len(api.firewallApplications) != 0 {
		t.Fatalf("attachment repairs = %+v/%+v", api.networkAttachments, api.firewallApplications)
	}
}

func TestReconcileRepairsExistingServerNetworkAndFirewallAttachments(t *testing.T) {
	existing := []Server{{
		ID:     10,
		Name:   "web-1",
		Labels: shipLabels("demo", "production", "web"),
	}}
	api := newFakeHetznerAPI(t, existing)
	result, err := api.client().Reconcile(context.Background(), "demo", "production", testEnvironment(1))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Existing) != 1 || result.Existing[0].ID != "10" {
		t.Fatalf("existing = %+v", result.Existing)
	}
	if got := strings.Join(api.networkAttachments, ","); got != "10:300" {
		t.Fatalf("network attachments = %q", got)
	}
	if got := strings.Join(api.firewallApplications, ","); got != "400:10" {
		t.Fatalf("firewall applications = %q", got)
	}
}

func TestReconcileSkipsDisabledNetworkAndFirewallAttachments(t *testing.T) {
	existing := []Server{{
		ID:     10,
		Name:   "web-1",
		Labels: shipLabels("demo", "production", "web"),
	}}
	env := testEnvironment(1)
	disabled := false
	env.Provider.Hetzner.Network.Enabled = &disabled
	env.Provider.Hetzner.Firewall.Enabled = &disabled
	api := newFakeHetznerAPI(t, existing)
	if _, err := api.client().Reconcile(context.Background(), "demo", "production", env); err != nil {
		t.Fatal(err)
	}
	if len(api.networks) != 0 || len(api.firewalls) != 0 {
		t.Fatalf("network/firewall creates = %d/%d", len(api.networks), len(api.firewalls))
	}
	if len(api.networkAttachments) != 0 || len(api.firewallApplications) != 0 {
		t.Fatalf("attachment repairs = %+v/%+v", api.networkAttachments, api.firewallApplications)
	}
}

func TestReconcileCreatesOnlyMissingAndReportsExtra(t *testing.T) {
	existing := []Server{
		{ID: 10, Name: "web-1", Labels: shipLabels("demo", "production", "web")},
		{ID: 11, Name: "web-old", Labels: shipLabels("demo", "production", "web")},
	}
	api := newFakeHetznerAPI(t, existing)
	result, err := api.client().Reconcile(context.Background(), "demo", "production", testEnvironment(2))
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
}

func TestWaitActionPollsUntilSuccess(t *testing.T) {
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/actions/55" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		polls++
		status := "running"
		if polls == 2 {
			status = "success"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"action": Action{ID: 55, Status: status}})
	}))
	t.Cleanup(server.Close)

	client := Client{Token: "test-token", BaseURL: server.URL, HTTP: server.Client(), PollInterval: time.Nanosecond}
	if err := client.WaitAction(context.Background(), 55); err != nil {
		t.Fatal(err)
	}
	if polls != 2 {
		t.Fatalf("polls = %d", polls)
	}
}

func TestWaitActionReturnsActionError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"action": Action{
				ID:     55,
				Status: "error",
				Error:  &ActionError{Code: "invalid_input", Message: "boom"},
			},
		})
	}))
	t.Cleanup(server.Close)

	client := Client{Token: "test-token", BaseURL: server.URL, HTTP: server.Client(), PollInterval: time.Nanosecond}
	err := client.WaitAction(context.Background(), 55)
	if err == nil {
		t.Fatal("expected action error")
	}
	if !strings.Contains(err.Error(), "invalid_input") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v", err)
	}
}

func TestWaitActionUsesDefaultableTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"action": Action{ID: 55, Status: "running"},
		})
	}))
	t.Cleanup(server.Close)

	client := Client{
		Token:         "test-token",
		BaseURL:       server.URL,
		HTTP:          server.Client(),
		PollInterval:  time.Nanosecond,
		ActionTimeout: time.Millisecond,
	}
	err := client.WaitAction(context.Background(), 55)
	if err == nil {
		t.Fatal("expected timeout")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("error = %v", err)
	}
}

func TestDecommissionDeletesManagedEnvironmentServers(t *testing.T) {
	existing := []Server{
		{ID: 10, Name: "web-1", Labels: shipLabels("demo", "production", "web")},
		{ID: 11, Name: "worker-1", Labels: shipLabels("demo", "production", "worker")},
	}
	api := newFakeHetznerAPI(t, existing)
	result, err := api.client().Decommission(context.Background(), "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 2 {
		t.Fatalf("deleted result = %+v", result.Deleted)
	}
	if got, want := strings.Join(api.deletes, ","), "10,11"; got != want {
		t.Fatalf("deleted ids = %q, want %q", got, want)
	}
	if api.selector != "managed-by=ship,project=demo,environment=production" {
		t.Fatalf("label selector = %q", api.selector)
	}
}

func testEnvironment(count int) config.Environment {
	return config.Environment{
		Provider: config.ProviderConfig{Hetzner: &config.HetznerConfig{
			Location:        "ash",
			ServerType:      "cpx31",
			Image:           "ubuntu-24.04",
			UserData:        "#cloud-config\npackages: [htop]\n",
			SSHKeys:         []string{"ship-key"},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {Count: count},
		}},
	}
}

func shipLabels(project, environment, pool string) map[string]string {
	return map[string]string{
		LabelManagedBy:   "ship",
		LabelProject:     project,
		LabelEnvironment: environment,
		LabelPool:        pool,
	}
}

type fakeHetznerAPI struct {
	server               *httptest.Server
	existing             []Server
	creates              []createServerRequest
	networks             []createNetworkRequest
	firewalls            []createFirewallRequest
	networkAttachments   []string
	firewallApplications []string
	deletes              []string
	selector             string
	nextID               int64
}

type createServerRequest struct {
	Name       string             `json:"name"`
	ServerType string             `json:"server_type"`
	Image      string             `json:"image"`
	Location   string             `json:"location"`
	Labels     map[string]string  `json:"labels"`
	SSHKeys    []string           `json:"ssh_keys"`
	UserData   string             `json:"user_data"`
	Networks   []int64            `json:"networks"`
	Firewalls  []map[string]int64 `json:"firewalls"`
}

type createNetworkRequest struct {
	Name    string            `json:"name"`
	IPRange string            `json:"ip_range"`
	Labels  map[string]string `json:"labels"`
}

type createFirewallRequest struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	Rules  []map[string]any  `json:"rules"`
}

type attachNetworkRequest struct {
	Network int64 `json:"network"`
}

type applyFirewallRequest struct {
	ApplyTo []struct {
		Type   string `json:"type"`
		Server struct {
			ID int64 `json:"id"`
		} `json:"server"`
	} `json:"apply_to"`
}

func newFakeHetznerAPI(t *testing.T, existing []Server) *fakeHetznerAPI {
	t.Helper()
	api := &fakeHetznerAPI{existing: existing, nextID: 100}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/networks":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"networks": []Network{},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/networks":
			var req createNetworkRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			api.networks = append(api.networks, req)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"network": Network{ID: 300, Name: req.Name, Labels: req.Labels},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/firewalls":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"firewalls": []Firewall{},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/firewalls":
			var req createFirewallRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			api.firewalls = append(api.firewalls, req)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"firewall": Firewall{ID: 400, Name: req.Name, Labels: req.Labels},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/servers":
			api.selector = r.URL.Query().Get("label_selector")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"servers": api.existing,
				"meta": map[string]any{
					"pagination": map[string]any{"next_page": nil},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/servers":
			var req createServerRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			api.creates = append(api.creates, req)
			id := api.nextID
			api.nextID++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"server": Server{
					ID:     id,
					Name:   req.Name,
					Labels: req.Labels,
					PublicNet: PublicNet{IPv4: PublicIPv4{
						IP: fmt.Sprintf("192.0.2.%d", id-99),
					}},
				},
				"action": Action{ID: id, Status: "running"},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions/attach_to_network"):
			var req attachNetworkRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/servers/"), "/actions/attach_to_network")
			api.networkAttachments = append(api.networkAttachments, id+":"+strconv.FormatInt(req.Network, 10))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"action": Action{ID: 500 + int64(len(api.networkAttachments)), Status: "running"},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions/apply_to_resources"):
			var req applyFirewallRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			firewallID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/firewalls/"), "/actions/apply_to_resources")
			if len(req.ApplyTo) != 1 || req.ApplyTo[0].Type != "server" {
				t.Fatalf("apply_to = %+v", req.ApplyTo)
			}
			api.firewallApplications = append(api.firewallApplications, firewallID+":"+strconv.FormatInt(req.ApplyTo[0].Server.ID, 10))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"actions": []Action{{ID: 600 + int64(len(api.firewallApplications)), Status: "running"}},
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/actions/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"action": Action{Status: "success"},
			})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/servers/"):
			id := strings.TrimPrefix(r.URL.Path, "/servers/")
			api.deletes = append(api.deletes, id)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"action": Action{ID: 200 + int64(len(api.deletes)), Status: "running"},
			})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (api *fakeHetznerAPI) client() Client {
	return Client{
		Token:        "test-token",
		HTTP:         api.server.Client(),
		BaseURL:      api.server.URL,
		PollInterval: time.Nanosecond,
	}
}
