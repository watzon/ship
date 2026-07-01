package scaleway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

var _ provider.Provider = Client{}

func TestDesiredServersForUsesProviderDefaultsAndPoolOverrides(t *testing.T) {
	env := config.Environment{
		Provider: config.ProviderConfig{Scaleway: &config.ScalewayConfig{
			ProjectID:      "project-id",
			Zone:           "fr-par-1",
			CommercialType: "PLAY2-PICO",
			Image:          "ubuntu_noble",
			UserData:       "#cloud-config\npackages: [htop]\n",
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {
				Count:    1,
				Size:     "DEV1-S",
				UserData: "#cloud-config\npackages: [curl]\n",
				Labels:   map[string]string{"owner": "platform"},
			},
		}},
	}

	plans := DesiredServersFor("demo", "production", env)
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(plans))
	}
	plan := plans[0]
	if plan.Location != "fr-par-1" || plan.Size != "DEV1-S" || plan.Image != "ubuntu_noble" {
		t.Fatalf("plan shape = %+v", plan)
	}
	if plan.UserData != "#cloud-config\npackages: [curl]\n" {
		t.Fatalf("user data = %q", plan.UserData)
	}
	if plan.Labels["owner"] != "platform" || plan.Labels[provider.LabelProject] != "demo" {
		t.Fatalf("labels = %+v", plan.Labels)
	}
}

func TestListServersFiltersShipTagsAndPaginates(t *testing.T) {
	requests := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/instance/v1/zones/fr-par-1/servers" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("project") != "project-id" {
			t.Fatalf("project query = %q", r.URL.Query().Get("project"))
		}
		if !strings.Contains(r.URL.Query().Get("tags"), tagForLabel(LabelEnvironment, "production")) {
			t.Fatalf("tags query = %q", r.URL.Query().Get("tags"))
		}
		switch r.URL.Query().Get("page") {
		case "1":
			servers := makeServers(100, "demo", "staging", "web")
			servers[0] = Server{ID: "server-1", Name: "web-1", Tags: tagsFromLabels(provider.ShipLabels("demo", "production", "web")), PublicIP: &IPAddress{Address: "198.51.100.10", Family: "inet"}}
			writeJSON(t, w, map[string]any{"servers": servers})
		case "2":
			writeJSON(t, w, map[string]any{"servers": []Server{
				{Name: "other-env", Tags: tagsFromLabels(provider.ShipLabels("demo", "staging", "web"))},
				{ID: "server-2", Name: "web-2", Tags: tagsFromLabels(provider.ShipLabels("demo", "production", "web")), PublicIP: &IPAddress{Address: "198.51.100.11", Family: "inet"}},
			}})
		default:
			t.Fatalf("unexpected page %s", r.URL.Query().Get("page"))
		}
	}))
	defer api.Close()

	client := Client{SecretKey: "secret", ProjectID: "project-id", Zone: "fr-par-1", BaseURL: api.URL, HTTP: api.Client()}
	hosts, err := client.List(context.Background(), "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if len(hosts) != 2 {
		t.Fatalf("hosts = %+v", hosts)
	}
	if hosts[0].Name != "web-1" || hosts[0].PublicAddress != "198.51.100.10" {
		t.Fatalf("host = %+v", hosts[0])
	}
	if hosts[1].Name != "web-2" || hosts[1].PublicAddress != "198.51.100.11" {
		t.Fatalf("host = %+v", hosts[1])
	}
}

func TestCreateServerSetsCloudInitBeforePowerOn(t *testing.T) {
	var calls []string
	var createBody map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/instance/v1/zones/fr-par-1/servers":
			decodeJSON(t, r, &createBody)
			writeJSON(t, w, map[string]any{"server": Server{
				ID:             "server-1",
				Name:           "web-1",
				Tags:           tagsFromLabels(provider.ShipLabels("demo", "production", "web")),
				AllowedActions: []string{"poweron"},
				PublicIP:       &IPAddress{Address: "198.51.100.10", Family: "inet"},
			}})
		case r.Method == http.MethodPatch && r.URL.Path == "/instance/v1/zones/fr-par-1/servers/server-1/user_data/cloud-init":
			data, _ := io.ReadAll(r.Body)
			if string(data) != "#cloud-config\npackages: [htop]\n" {
				t.Fatalf("user data body = %q", string(data))
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/instance/v1/zones/fr-par-1/servers/server-1/action":
			var body map[string]string
			decodeJSON(t, r, &body)
			if body["action"] != "poweron" {
				t.Fatalf("action body = %+v", body)
			}
			writeJSON(t, w, map[string]any{"task": map[string]string{"id": "task-1"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()

	enableIPv6 := true
	plan := provider.HostPlan{
		Project:     "demo",
		Environment: "production",
		Name:        "web-1",
		Pool:        "web",
		Location:    "fr-par-1",
		Size:        "DEV1-S",
		Image:       "ubuntu_noble",
		UserData:    "#cloud-config\npackages: [htop]\n",
		Labels:      provider.ShipLabels("demo", "production", "web"),
	}
	client := Client{SecretKey: "secret", BaseURL: api.URL, HTTP: api.Client()}
	server, err := client.CreateServer(context.Background(), plan, config.ScalewayConfig{
		ProjectID:      "project-id",
		Zone:           "fr-par-1",
		CommercialType: "DEV1-S",
		Image:          "ubuntu_noble",
		EnableIPv6:     &enableIPv6,
		SecurityGroup:  config.ScalewaySecurityGroup{ID: "sg-1", Managed: boolPtr(false)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if server.ID != "server-1" {
		t.Fatalf("server = %+v", server)
	}
	wantCalls := []string{
		"POST /instance/v1/zones/fr-par-1/servers",
		"PATCH /instance/v1/zones/fr-par-1/servers/server-1/user_data/cloud-init",
		"POST /instance/v1/zones/fr-par-1/servers/server-1/action",
	}
	if strings.Join(calls, "\n") != strings.Join(wantCalls, "\n") {
		t.Fatalf("calls = %+v", calls)
	}
	if createBody["commercial_type"] != "DEV1-S" || createBody["image"] != "ubuntu_noble" || createBody["security_group"] != "sg-1" {
		t.Fatalf("create body = %+v", createBody)
	}
	tags, ok := createBody["tags"].([]any)
	if !ok || len(tags) == 0 {
		t.Fatalf("tags missing from create body: %+v", createBody)
	}
}

func TestEnsureSecurityGroupCreatesAndReplacesRules(t *testing.T) {
	var createBody map[string]any
	var rulesBody map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/instance/v1/zones/fr-par-1/security_groups":
			writeJSON(t, w, map[string]any{"security_groups": []SecurityGroup{}})
		case r.Method == http.MethodPost && r.URL.Path == "/instance/v1/zones/fr-par-1/security_groups":
			decodeJSON(t, r, &createBody)
			writeJSON(t, w, map[string]any{"security_group": SecurityGroup{
				ID:   "sg-1",
				Name: "ship-demo-production-security-group",
				Tags: tagsFromLabels(provider.ShipLabels("demo", "production", "security-group")),
			}})
		case r.Method == http.MethodPut && r.URL.Path == "/instance/v1/zones/fr-par-1/security_groups/sg-1/rules":
			decodeJSON(t, r, &rulesBody)
			writeJSON(t, w, map[string]any{"rules": []SecurityGroupRule{}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()

	enableIPv6 := true
	client := Client{SecretKey: "secret", ProjectID: "project-id", Zone: "fr-par-1", BaseURL: api.URL, HTTP: api.Client()}
	sg, err := client.EnsureSecurityGroup(context.Background(), "demo", "production", config.ScalewayConfig{
		ProjectID:         "project-id",
		Zone:              "fr-par-1",
		SSHAllowedCIDRs:   []string{"203.0.113.0/24"},
		EnableIPv6:        &enableIPv6,
		SecurityGroup:     config.ScalewaySecurityGroup{},
		DynamicIPRequired: boolPtr(true),
		RoutedIPEnabled:   boolPtr(true),
		CommercialType:    "DEV1-S",
		Image:             "ubuntu_noble",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sg.ID != "sg-1" {
		t.Fatalf("security group = %+v", sg)
	}
	if createBody["inbound_default_policy"] != "drop" || createBody["outbound_default_policy"] != "accept" || createBody["stateful"] != true {
		t.Fatalf("create body = %+v", createBody)
	}
	rules, ok := rulesBody["rules"].([]any)
	if !ok {
		t.Fatalf("rules body = %+v", rulesBody)
	}
	if len(rules) != 7 {
		t.Fatalf("rules = %d, want SSH + IPv4/IPv6 HTTP/HTTPS/HTTP3: %+v", len(rules), rules)
	}
	first := rules[0].(map[string]any)
	if first["ip_range"] != "203.0.113.0/24" || first["dest_port_from"].(float64) != 22 {
		t.Fatalf("first rule = %+v", first)
	}
}

func TestReconcileDryRunReturnsDesiredWithoutToken(t *testing.T) {
	env := config.Environment{
		Provider: config.ProviderConfig{Scaleway: &config.ScalewayConfig{
			ProjectID:         "project-id",
			Zone:              "fr-par-1",
			CommercialType:    "DEV1-S",
			Image:             "ubuntu_noble",
			SSHAllowedCIDRs:   []string{"203.0.113.0/24"},
			DynamicIPRequired: boolPtr(true),
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{"web": {Count: 2}}},
	}

	result, err := Client{DryRun: true}.Reconcile(context.Background(), "demo", "production", env)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Desired) != 2 || len(result.Created) != 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestDeleteServer(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/instance/v1/zones/fr-par-1/servers/server-1" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer api.Close()

	client := Client{SecretKey: "secret", Zone: "fr-par-1", BaseURL: api.URL, HTTP: api.Client()}
	if err := client.Delete(context.Background(), provider.Host{ID: "server-1"}); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialChecksAcceptSCWSecretKey(t *testing.T) {
	checks := Client{}.CredentialChecks(func(key string) (string, bool) {
		if key == "SCW_SECRET_KEY" {
			return "secret", true
		}
		return "", false
	})
	if len(checks) != 1 || !checks[0].Present {
		t.Fatalf("checks = %+v", checks)
	}
}

func makeServers(count int, project, environment, pool string) []Server {
	servers := make([]Server, 0, count)
	for i := 0; i < count; i++ {
		servers = append(servers, Server{
			ID:   "server-" + string(rune('a'+i%26)),
			Name: "web-many",
			Tags: tagsFromLabels(provider.ShipLabels(project, environment, pool)),
		})
	}
	return servers
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func decodeJSON(t *testing.T, r *http.Request, out any) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}

func boolPtr(v bool) *bool {
	return &v
}
