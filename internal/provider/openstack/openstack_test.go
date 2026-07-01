package openstack

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

var _ provider.Provider = Client{}

func TestDesiredServersForUsesProviderDefaultsAndPoolOverrides(t *testing.T) {
	env := config.Environment{
		Provider: config.ProviderConfig{OpenStack: &config.OpenStackConfig{
			Region:   "GRA11",
			Flavor:   "b2-7",
			Image:    "ubuntu-24.04",
			UserData: "#cloud-config\npackages: [htop]\n",
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {
				Count:    1,
				Size:     "b2-15",
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
	if plan.Location != "GRA11" || plan.Size != "b2-15" || plan.Image != "ubuntu-24.04" {
		t.Fatalf("plan shape = %+v", plan)
	}
	if plan.UserData != "#cloud-config\npackages: [curl]\n" {
		t.Fatalf("user data = %q", plan.UserData)
	}
	if plan.Labels["owner"] != "platform" || plan.Labels[provider.LabelProject] != "demo" {
		t.Fatalf("labels = %+v", plan.Labels)
	}
}

func TestAuthenticateUsesKeystoneCatalogAndCreateServer(t *testing.T) {
	var authBody map[string]any
	var createBody map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v3/auth/tokens":
			decodeJSON(t, r, &authBody)
			w.Header().Set("X-Subject-Token", "token-123")
			writeJSON(t, w, map[string]any{"token": map[string]any{"catalog": []map[string]any{{
				"type": "compute",
				"endpoints": []map[string]any{{
					"interface": "public",
					"region":    "GRA11",
					"url":       "https://example.invalid/compute/v2/project-id",
				}},
			}}}})
		case r.Method == http.MethodPost && r.URL.Path == "/compute/v2/project-id/servers":
			if r.Header.Get("X-Auth-Token") != "token-123" {
				t.Fatalf("token header = %q", r.Header.Get("X-Auth-Token"))
			}
			decodeJSON(t, r, &createBody)
			writeJSON(t, w, map[string]any{"server": Server{
				ID:       "server-1",
				Name:     "web-1",
				Metadata: provider.ShipLabels("demo", "production", "web"),
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()

	client := Client{
		AuthURL:     api.URL + "/v3",
		Region:      "GRA11",
		Username:    "alice",
		Password:    "secret",
		ProjectName: "demo-project",
		HTTP:        rewriteHostClient(api.URL, api.Client()),
	}
	authed, err := client.authenticated(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	plan := provider.HostPlan{
		Project:     "demo",
		Environment: "production",
		Name:        "web-1",
		Pool:        "web",
		Location:    "GRA11",
		Size:        "b2-7",
		Image:       "image-id",
		UserData:    "#cloud-config\npackages: [htop]\n",
		Labels:      provider.ShipLabels("demo", "production", "web"),
	}
	configDrive := true
	server, err := authed.CreateServer(context.Background(), plan, config.OpenStackConfig{
		Network:          "net-id",
		KeyName:          "ship-key",
		SecurityGroups:   []string{"ship-web"},
		AvailabilityZone: "nova",
		ConfigDrive:      &configDrive,
		Metadata:         map[string]string{"role": "web"},
		Tags:             []string{"ship"},
		SchedulerHints:   map[string]any{"group": "server-group-id"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if server.ID != "server-1" {
		t.Fatalf("server = %+v", server)
	}
	if !strings.Contains(toJSON(t, authBody), `"password"`) {
		t.Fatalf("auth body = %+v", authBody)
	}
	body := createBody["server"].(map[string]any)
	if body["flavorRef"] != "b2-7" || body["imageRef"] != "image-id" || body["key_name"] != "ship-key" {
		t.Fatalf("create body = %+v", body)
	}
	if body["user_data"] != base64.StdEncoding.EncodeToString([]byte(plan.UserData)) {
		t.Fatalf("user_data = %q", body["user_data"])
	}
	metadata := body["metadata"].(map[string]any)
	if metadata["managed-by"] != "ship" || metadata["role"] != "web" {
		t.Fatalf("metadata = %+v", metadata)
	}
	hints := createBody["OS-SCH-HNT:scheduler_hints"].(map[string]any)
	if hints["group"] != "server-group-id" {
		t.Fatalf("scheduler hints = %+v", hints)
	}
}

func TestListServersFiltersMetadata(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/servers/detail" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		writeJSON(t, w, map[string]any{"servers": []Server{
			{
				ID:       "server-1",
				Name:     "web-1",
				Metadata: provider.ShipLabels("demo", "production", "web"),
				Addresses: map[string][]Address{
					"Ext-Net": {{Address: "198.51.100.10", Type: "floating", Version: 4}},
				},
			},
			{ID: "server-2", Name: "staging", Metadata: provider.ShipLabels("demo", "staging", "web")},
		}})
	}))
	defer api.Close()

	client := Client{Token: "token", ComputeURL: api.URL, HTTP: api.Client()}
	hosts, err := client.List(context.Background(), "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 {
		t.Fatalf("hosts = %+v", hosts)
	}
	if hosts[0].Name != "web-1" || hosts[0].PublicAddress != "198.51.100.10" {
		t.Fatalf("host = %+v", hosts[0])
	}
}

func TestEnsureSecurityGroupCreatesGroupAndMissingRules(t *testing.T) {
	var createGroupBody map[string]any
	var createdRules []map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2.0/security-groups":
			writeJSON(t, w, map[string]any{"security_groups": []SecurityGroup{}})
		case r.Method == http.MethodPost && r.URL.Path == "/v2.0/security-groups":
			decodeJSON(t, r, &createGroupBody)
			writeJSON(t, w, map[string]any{"security_group": SecurityGroup{
				ID:   "sg-1",
				Name: "ship-demo-production-security-group",
				Tags: tagsFromLabels(provider.ShipLabels("demo", "production", "security-group")),
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/v2.0/security-group-rules":
			if r.URL.Query().Get("security_group_id") != "sg-1" {
				t.Fatalf("security_group_id = %q", r.URL.Query().Get("security_group_id"))
			}
			port := 80
			writeJSON(t, w, map[string]any{"security_group_rules": []SecurityGroupRule{{
				Direction:       "ingress",
				EtherType:       "IPv4",
				Protocol:        "tcp",
				PortRangeMin:    &port,
				PortRangeMax:    &port,
				RemoteIPPrefix:  "0.0.0.0/0",
				SecurityGroupID: "sg-1",
			}}})
		case r.Method == http.MethodPost && r.URL.Path == "/v2.0/security-group-rules":
			var body map[string]any
			decodeJSON(t, r, &body)
			createdRules = append(createdRules, body["security_group_rule"].(map[string]any))
			writeJSON(t, w, map[string]any{"security_group_rule": body["security_group_rule"]})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()

	client := Client{Token: "token", NetworkURL: api.URL + "/v2.0", HTTP: api.Client()}
	sg, err := client.EnsureSecurityGroup(context.Background(), "demo", "production", config.OpenStackConfig{
		SSHAllowedCIDRs: []string{"203.0.113.0/24"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sg.ID != "sg-1" {
		t.Fatalf("security group = %+v", sg)
	}
	group := createGroupBody["security_group"].(map[string]any)
	if group["name"] != "ship-demo-production-security-group" || group["stateful"] != true {
		t.Fatalf("group body = %+v", group)
	}
	if len(createdRules) != 6 {
		t.Fatalf("created rules = %+v", createdRules)
	}
	if !hasRule(createdRules, "tcp", "203.0.113.0/24", 22) || !hasRule(createdRules, "tcp", "::/0", 443) || !hasRule(createdRules, "udp", "::/0", 443) {
		t.Fatalf("created rules missing expected entries: %+v", createdRules)
	}
}

func TestCreateServerAllocatesFloatingIP(t *testing.T) {
	var createFloatingIPBody map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/servers":
			writeJSON(t, w, map[string]any{"server": Server{ID: "server-1", Name: "web-1", Metadata: provider.ShipLabels("demo", "production", "web")}})
		case r.Method == http.MethodGet && r.URL.Path == "/v2.0/ports":
			if r.URL.Query().Get("device_id") != "server-1" {
				t.Fatalf("device_id = %q", r.URL.Query().Get("device_id"))
			}
			writeJSON(t, w, map[string]any{"ports": []Port{{ID: "port-1", NetworkID: "net-id", DeviceID: "server-1"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v2.0/floatingips":
			if r.URL.Query().Get("port_id") != "port-1" {
				t.Fatalf("port_id = %q", r.URL.Query().Get("port_id"))
			}
			writeJSON(t, w, map[string]any{"floatingips": []FloatingIP{}})
		case r.Method == http.MethodPost && r.URL.Path == "/v2.0/floatingips":
			decodeJSON(t, r, &createFloatingIPBody)
			writeJSON(t, w, map[string]any{"floatingip": FloatingIP{
				ID:                "fip-1",
				FloatingNetworkID: "ext-net-id",
				FloatingIPAddress: "198.51.100.20",
				PortID:            "port-1",
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()

	client := Client{Token: "token", ComputeURL: api.URL, NetworkURL: api.URL + "/v2.0", HTTP: api.Client()}
	plan := provider.HostPlan{
		Project:     "demo",
		Environment: "production",
		Name:        "web-1",
		Pool:        "web",
		Size:        "b2-7",
		Image:       "image-id",
		Labels:      provider.ShipLabels("demo", "production", "web"),
	}
	server, err := client.CreateServer(context.Background(), plan, config.OpenStackConfig{
		FloatingIP: config.OpenStackFloatingIPConfig{
			NetworkID:   "ext-net-id",
			Description: "ship public endpoint",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if server.AccessIPv4 != "198.51.100.20" {
		t.Fatalf("server = %+v", server)
	}
	floatingIP := createFloatingIPBody["floatingip"].(map[string]any)
	if floatingIP["floating_network_id"] != "ext-net-id" || floatingIP["port_id"] != "port-1" || floatingIP["description"] != "ship public endpoint" {
		t.Fatalf("floating IP body = %+v", floatingIP)
	}
	if !hasString(floatingIP["tags"].([]any), "managed-by-ship") {
		t.Fatalf("floating IP tags = %+v", floatingIP["tags"])
	}
}

func TestEnsureFloatingIPAssociatesExistingAddress(t *testing.T) {
	var updateBody map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2.0/ports":
			writeJSON(t, w, map[string]any{"ports": []Port{{ID: "port-1", NetworkID: "net-id", DeviceID: "server-1"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v2.0/floatingips":
			if r.URL.Query().Get("floating_ip_address") != "198.51.100.30" {
				t.Fatalf("floating_ip_address = %q", r.URL.Query().Get("floating_ip_address"))
			}
			writeJSON(t, w, map[string]any{"floatingips": []FloatingIP{{ID: "fip-1", FloatingIPAddress: "198.51.100.30"}}})
		case r.Method == http.MethodPut && r.URL.Path == "/v2.0/floatingips/fip-1":
			decodeJSON(t, r, &updateBody)
			writeJSON(t, w, map[string]any{"floatingip": FloatingIP{ID: "fip-1", FloatingIPAddress: "198.51.100.30", PortID: "port-1"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()

	client := Client{Token: "token", NetworkURL: api.URL + "/v2.0", HTTP: api.Client()}
	floatingIP, err := client.EnsureFloatingIP(context.Background(), "server-1", provider.HostPlan{}, config.OpenStackConfig{
		FloatingIP: config.OpenStackFloatingIPConfig{
			Address:        "198.51.100.30",
			FixedIPAddress: "10.0.0.5",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if floatingIP.PortID != "port-1" {
		t.Fatalf("floating IP = %+v", floatingIP)
	}
	body := updateBody["floatingip"].(map[string]any)
	if body["port_id"] != "port-1" || body["fixed_ip_address"] != "10.0.0.5" {
		t.Fatalf("update body = %+v", body)
	}
}

func TestDeleteServer(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/servers/server-1" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer api.Close()

	client := Client{Token: "token", ComputeURL: api.URL, HTTP: api.Client()}
	if err := client.Delete(context.Background(), provider.Host{ID: "server-1"}); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileEnsuresManagedSecurityGroupBeforeCreate(t *testing.T) {
	var createServerBody map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/servers/detail":
			writeJSON(t, w, map[string]any{"servers": []Server{}})
		case r.Method == http.MethodPost && r.URL.Path == "/servers":
			decodeJSON(t, r, &createServerBody)
			writeJSON(t, w, map[string]any{"server": Server{ID: "server-1", Name: "web-1", Metadata: provider.ShipLabels("demo", "production", "web")}})
		case r.Method == http.MethodGet && r.URL.Path == "/v2.0/security-groups":
			writeJSON(t, w, map[string]any{"security_groups": []SecurityGroup{{ID: "sg-1", Name: "ship-demo-production-security-group", Tags: tagsFromLabels(provider.ShipLabels("demo", "production", "security-group"))}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v2.0/security-group-rules":
			writeJSON(t, w, map[string]any{"security_group_rules": securityGroupRules(config.OpenStackConfig{SSHAllowedCIDRs: []string{"203.0.113.0/24"}})})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()

	env := config.Environment{
		Provider: config.ProviderConfig{OpenStack: &config.OpenStackConfig{
			ComputeURL:      api.URL,
			NetworkURL:      api.URL + "/v2.0",
			Flavor:          "b2-7",
			Image:           "ubuntu-24.04",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
			SecurityGroups:  []string{"base"},
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{"web": {Count: 1}}},
	}
	client := Client{Token: "token", ComputeURL: api.URL, NetworkURL: api.URL + "/v2.0", HTTP: api.Client()}
	result, err := client.Reconcile(context.Background(), "demo", "production", env)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("result = %+v", result)
	}
	server := createServerBody["server"].(map[string]any)
	groups := server["security_groups"].([]any)
	if len(groups) != 2 || groups[1].(map[string]any)["name"] != "ship-demo-production-security-group" {
		t.Fatalf("security groups = %+v", groups)
	}
}

func TestReconcileDryRunReturnsDesiredWithoutCredentials(t *testing.T) {
	env := config.Environment{
		Provider: config.ProviderConfig{OpenStack: &config.OpenStackConfig{
			Region: "GRA11",
			Flavor: "b2-7",
			Image:  "ubuntu-24.04",
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

func TestCredentialChecksAcceptApplicationCredential(t *testing.T) {
	checks := Client{}.CredentialChecks(func(key string) (string, bool) {
		switch key {
		case "OS_AUTH_URL", "OS_APPLICATION_CREDENTIAL_ID", "OS_APPLICATION_CREDENTIAL_SECRET":
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

func toJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func rewriteHostClient(baseURL string, client *http.Client) *http.Client {
	base, _ := url.Parse(baseURL)
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "example.invalid" {
			req.URL.Scheme = base.Scheme
			req.URL.Host = base.Host
		}
		return http.DefaultTransport.RoundTrip(req)
	})
	return &http.Client{Transport: transport, Timeout: client.Timeout}
}

func hasRule(rules []map[string]any, protocol, cidr string, port int) bool {
	for _, rule := range rules {
		if rule["protocol"] == protocol && rule["remote_ip_prefix"] == cidr && int(rule["port_range_min"].(float64)) == port {
			return true
		}
	}
	return false
}

func hasString(values []any, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
