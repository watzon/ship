package upcloud

import (
	"context"
	"encoding/json"
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
		Provider: config.ProviderConfig{UpCloud: &config.UpCloudConfig{
			Zone:     "fi-hel1",
			Plan:     "1xCPU-1GB",
			Template: "01000000-0000-4000-8000-000030240200",
			UserData: "#cloud-config\npackages: [htop]\n",
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {
				Count:    1,
				Size:     "2xCPU-4GB",
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
	if plan.Location != "fi-hel1" || plan.Size != "2xCPU-4GB" || plan.Image != "01000000-0000-4000-8000-000030240200" {
		t.Fatalf("plan shape = %+v", plan)
	}
	if plan.UserData != "#cloud-config\npackages: [curl]\n" {
		t.Fatalf("user data = %q", plan.UserData)
	}
	if plan.Labels["owner"] != "platform" || plan.Labels[provider.LabelProject] != "demo" {
		t.Fatalf("labels = %+v", plan.Labels)
	}
}

func TestCreateServerSendsUpCloudShapeAndFirewallRules(t *testing.T) {
	var createBody map[string]any
	var createdRules []FirewallRule
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBasicAuth(t, r)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/server":
			decodeJSON(t, r, &createBody)
			writeJSON(t, w, map[string]any{"server": Server{
				UUID:     "server-1",
				Title:    "web-1",
				Hostname: "web-1",
				Labels:   labelsFromMap(provider.ShipLabels("demo", "production", "web")),
				IPAddresses: struct {
					IPAddress []IPAddress `json:"ip_address"`
				}{IPAddress: []IPAddress{{Access: "public", Address: "198.51.100.50", Family: "IPv4"}}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/server/server-1/firewall_rule":
			writeJSON(t, w, map[string]any{"firewall_rules": map[string]any{"firewall_rule": []FirewallRule{}}})
		case r.Method == http.MethodPost && r.URL.Path == "/server/server-1/firewall_rule":
			var body map[string]FirewallRule
			decodeJSON(t, r, &body)
			createdRules = append(createdRules, body["firewall_rule"])
			writeJSON(t, w, map[string]any{"firewall_rule": body["firewall_rule"]})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()

	metadata := true
	ipv6 := true
	client := Client{Username: "user", Password: "pass", BaseURL: api.URL, HTTP: api.Client()}
	server, err := client.CreateServer(context.Background(), provider.HostPlan{
		Project:     "demo",
		Environment: "production",
		Name:        "web-1",
		Pool:        "web",
		Location:    "fi-hel1",
		Size:        "1xCPU-1GB",
		Image:       "ubuntu-template",
		UserData:    "#cloud-config\npackages: [htop]\n",
		Labels:      provider.ShipLabels("demo", "production", "web"),
	}, config.UpCloudConfig{
		SSHKeys:          []string{"ssh-ed25519 AAAA..."},
		Username:         "deploy",
		Metadata:         &metadata,
		IPv6:             &ipv6,
		PrivateNetworkID: "private-net",
		StorageSizeGB:    40,
		StorageTier:      "maxiops",
		SSHAllowedCIDRs:  []string{"203.0.113.0/24"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if server.UUID != "server-1" {
		t.Fatalf("server = %+v", server)
	}
	body := createBody["server"].(map[string]any)
	if body["zone"] != "fi-hel1" || body["plan"] != "1xCPU-1GB" || body["metadata"] != "yes" || body["user_data"] == "" {
		t.Fatalf("create body = %+v", body)
	}
	login := body["login_user"].(map[string]any)
	if login["username"] != "deploy" {
		t.Fatalf("login_user = %+v", login)
	}
	storage := body["storage_devices"].(map[string]any)["storage_device"].([]any)[0].(map[string]any)
	if storage["storage"] != "ubuntu-template" || int(storage["size"].(float64)) != 40 || storage["tier"] != "maxiops" {
		t.Fatalf("storage = %+v", storage)
	}
	if len(createdRules) != 7 {
		t.Fatalf("created rules = %+v", createdRules)
	}
	if !hasRule(createdRules, "203.0.113.0", "203.0.113.255", "22") ||
		!hasRule(createdRules, "::", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "443") {
		t.Fatalf("created rules missing expected entries: %+v", createdRules)
	}
}

func TestFirewallRuleExpandsCIDRToUpCloudAddressRange(t *testing.T) {
	rule := firewallRule("ship-ssh", "tcp", "198.51.100.4/30", 22)
	if rule.SourceAddressStart != "198.51.100.4" || rule.SourceAddressEnd != "198.51.100.7" {
		t.Fatalf("ipv4 source range = %s-%s", rule.SourceAddressStart, rule.SourceAddressEnd)
	}
	if rule.Family != "IPv4" {
		t.Fatalf("ipv4 family = %q", rule.Family)
	}

	rule = firewallRule("ship-ssh", "tcp", "2001:db8::/126", 22)
	if rule.SourceAddressStart != "2001:db8::" || rule.SourceAddressEnd != "2001:db8::3" {
		t.Fatalf("ipv6 source range = %s-%s", rule.SourceAddressStart, rule.SourceAddressEnd)
	}
	if rule.Family != "IPv6" {
		t.Fatalf("ipv6 family = %q", rule.Family)
	}
}

func TestListServersFiltersAndHydratesDetails(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBasicAuth(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/server":
			if r.URL.Query().Get("search") != "fi-hel1" {
				t.Fatalf("query = %s", r.URL.RawQuery)
			}
			writeJSON(t, w, map[string]any{"servers": map[string]any{"server": []Server{
				{UUID: "server-1", Title: "web-1", Labels: labelsFromMap(provider.ShipLabels("demo", "production", "web"))},
				{UUID: "server-2", Title: "staging", Labels: labelsFromMap(provider.ShipLabels("demo", "staging", "web"))},
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/server/server-1":
			writeJSON(t, w, map[string]any{"server": Server{
				UUID:   "server-1",
				Title:  "web-1",
				Labels: labelsFromMap(provider.ShipLabels("demo", "production", "web")),
				IPAddresses: struct {
					IPAddress []IPAddress `json:"ip_address"`
				}{IPAddress: []IPAddress{{Access: "public", Address: "198.51.100.51", Family: "IPv4"}}},
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()

	client := Client{Username: "user", Password: "pass", Zone: "fi-hel1", BaseURL: api.URL, HTTP: api.Client()}
	hosts, err := client.List(context.Background(), "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].Name != "web-1" || hosts[0].PublicAddress != "198.51.100.51" {
		t.Fatalf("hosts = %+v", hosts)
	}
}

func TestDeleteStopsAndDeletesServer(t *testing.T) {
	var paths []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBasicAuth(t, r)
		paths = append(paths, r.Method+" "+r.URL.String())
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/server/server-1/stop":
			writeJSON(t, w, map[string]any{"server": map[string]any{"uuid": "server-1"}})
		case r.Method == http.MethodDelete && r.URL.Path == "/server/server-1":
			if r.URL.Query().Get("storages") != "1" || r.URL.Query().Get("backups") != "delete" {
				t.Fatalf("query = %s", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()

	client := Client{Username: "user", Password: "pass", BaseURL: api.URL, HTTP: api.Client()}
	if err := client.Delete(context.Background(), provider.Host{ID: "server-1"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "POST /server/server-1/stop,DELETE /server/server-1?backups=delete&storages=1" {
		t.Fatalf("paths = %+v", paths)
	}
}

func TestCredentialChecks(t *testing.T) {
	checks := Client{}.CredentialChecks(func(key string) (string, bool) {
		switch key {
		case "UPCLOUD_USERNAME", "UPCLOUD_PASSWORD":
			return "value", true
		default:
			return "", false
		}
	})
	if len(checks) != 1 || !checks[0].Present {
		t.Fatalf("checks = %+v", checks)
	}
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

func assertBasicAuth(t *testing.T, r *http.Request) {
	t.Helper()
	user, pass, ok := r.BasicAuth()
	if !ok || user != "user" || pass != "pass" {
		t.Fatalf("basic auth = %q/%q ok=%v", user, pass, ok)
	}
}

func hasRule(rules []FirewallRule, start, end, port string) bool {
	for _, rule := range rules {
		if rule.SourceAddressStart == start && rule.SourceAddressEnd == end && rule.DestinationPortStart == port {
			return true
		}
	}
	return false
}
