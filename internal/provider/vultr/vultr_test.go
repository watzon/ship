package vultr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

var _ provider.Provider = Client{}

func TestDesiredInstancesUsesPoolsAndSource(t *testing.T) {
	instances := DesiredInstances(testEnvironment(2))
	if len(instances) != 2 {
		t.Fatalf("len = %d", len(instances))
	}
	if instances[0].Name != "web-1" || instances[0].Pool != "web" {
		t.Fatalf("unexpected instance: %+v", instances[0])
	}
	if instances[0].Location != "ewr" || instances[0].Size != "vc2-2c-4gb" || instances[0].Image != "os_id:2284" {
		t.Fatalf("provider options missing from plan: %+v", instances[0])
	}
}

func TestDesiredInstancesApplyPoolShapeOverrides(t *testing.T) {
	env := testEnvironment(1)
	env.Hosts.Pools["worker"] = config.Pool{
		Count:    1,
		Location: "lax",
		Size:     "vc2-4c-8gb",
		Image:    "snapshot_id:snapshot-abc",
	}
	instances := DesiredInstances(env)
	if len(instances) != 2 {
		t.Fatalf("len = %d", len(instances))
	}
	worker := instances[0]
	if worker.Pool != "worker" {
		worker = instances[1]
	}
	if worker.Pool != "worker" || worker.Location != "lax" || worker.Size != "vc2-4c-8gb" || worker.Image != "snapshot_id:snapshot-abc" {
		t.Fatalf("worker = %+v", worker)
	}
}

func TestDesiredInstanceTagsIncludeShipLabels(t *testing.T) {
	instances := DesiredInstancesFor("demo", "production", testEnvironment(1))
	if len(instances) != 1 {
		t.Fatalf("len = %d", len(instances))
	}
	tags := tagsForPlan(instances[0])
	for _, want := range []string{
		"ship:managed-by=ship",
		"ship:project=demo",
		"ship:environment=production",
		"ship:pool=web",
	} {
		if !contains(tags, want) {
			t.Fatalf("tags missing %q in %+v", want, tags)
		}
	}
}

func TestCreateInstanceUsesPoolSourceOverride(t *testing.T) {
	api := newFakeVultrAPI(t, nil)
	env := testEnvironment(1)
	plan := DesiredInstancesFor("demo", "production", env)[0]
	plan.Image = "image_id:image-abc"
	if _, err := api.client().CreateInstance(context.Background(), plan, *env.Provider.Vultr); err != nil {
		t.Fatal(err)
	}
	if len(api.creates) != 1 {
		t.Fatalf("creates = %d", len(api.creates))
	}
	create := api.creates[0]
	if create.ImageID != "image-abc" || create.OSID != 0 || create.SnapshotID != "" || create.AppID != 0 {
		t.Fatalf("create source = %+v", create)
	}
}

func TestReconcileCreatesMissingInstances(t *testing.T) {
	api := newFakeVultrAPI(t, nil)
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
	create := api.creates[0]
	if create.Region != "ewr" || create.Plan != "vc2-2c-4gb" || create.Label != "web-1" {
		t.Fatalf("create request = %+v", create)
	}
	if got, want := create.UserData, base64.StdEncoding.EncodeToString([]byte("#cloud-config\npackages: [htop]\n")); got != want {
		t.Fatalf("user_data = %q, want %q", got, want)
	}
	if create.OSID != 2284 || create.ImageID != "" || create.SnapshotID != "" || create.AppID != 0 {
		t.Fatalf("create source = %+v", create)
	}
	if len(create.SSHKeyID) != 1 || create.SSHKeyID[0] != "ship-key" {
		t.Fatalf("sshkey_id = %+v", create.SSHKeyID)
	}
	if create.Hostname != "ship-host" || create.FirewallGroupID != "firewall-1" {
		t.Fatalf("create host settings = %+v", create)
	}
	if create.Backups != "enabled" || create.EnableIPv6 == nil || !*create.EnableIPv6 || create.DDoSProtection == nil || !*create.DDoSProtection {
		t.Fatalf("create feature settings = %+v", create)
	}
	if create.ActivationEmail == nil || *create.ActivationEmail {
		t.Fatalf("activation_email = %+v", create.ActivationEmail)
	}
	if create.EnableVPC == nil || !*create.EnableVPC || len(create.AttachVPC) != 1 || create.AttachVPC[0] != "vpc-123" {
		t.Fatalf("vpc settings = %+v", create)
	}
	if create.VPCOnly == nil || *create.VPCOnly || create.DisablePublicIPv4 == nil || *create.DisablePublicIPv4 {
		t.Fatalf("public/private network settings = %+v", create)
	}
	if create.ReservedIPv4 != "192.0.2.50" || create.UserScheme != "limited" || create.ScriptID != "script-123" {
		t.Fatalf("create boot settings = %+v", create)
	}
	if create.AppVariables["admin_email"] != "ops@example.com" {
		t.Fatalf("app variables = %+v", create.AppVariables)
	}
	if len(api.firewalls) != 1 || api.firewalls[0].Description != "ship-demo-production-firewall" {
		t.Fatalf("firewalls = %+v", api.firewalls)
	}
	if len(api.firewallRules["firewall-1"]) != 7 {
		t.Fatalf("firewall rules = %+v", api.firewallRules)
	}
	for _, want := range []createFirewallRuleRequest{
		{IPType: "v4", Protocol: "tcp", Port: "22", Subnet: "203.0.113.0", SubnetSize: 24, Notes: "ship-ssh"},
		{IPType: "v4", Protocol: "tcp", Port: "80", Subnet: "0.0.0.0", SubnetSize: 0, Notes: "ship-http"},
		{IPType: "v6", Protocol: "tcp", Port: "80", Subnet: "::", SubnetSize: 0, Notes: "ship-http"},
		{IPType: "v4", Protocol: "tcp", Port: "443", Subnet: "0.0.0.0", SubnetSize: 0, Notes: "ship-https"},
		{IPType: "v6", Protocol: "tcp", Port: "443", Subnet: "::", SubnetSize: 0, Notes: "ship-https"},
		{IPType: "v4", Protocol: "udp", Port: "443", Subnet: "0.0.0.0", SubnetSize: 0, Notes: "ship-http3"},
		{IPType: "v6", Protocol: "udp", Port: "443", Subnet: "::", SubnetSize: 0, Notes: "ship-http3"},
	} {
		if !containsFirewallRule(api.firewallRules["firewall-1"], want) {
			t.Fatalf("firewall rule missing %+v in %+v", want, api.firewallRules["firewall-1"])
		}
	}
	for _, want := range []string{"ship:managed-by=ship", "ship:project=demo", "ship:environment=production", "ship:pool=web"} {
		if !contains(create.Tags, want) {
			t.Fatalf("create tags missing %q in %+v", want, create.Tags)
		}
	}
	if result.Created[0].ID == "" || result.Created[0].PublicAddress == "" {
		t.Fatalf("created instance missing facts: %+v", result.Created[0])
	}
}

func TestEnsureFirewallUsesExistingFirewallAndAddsMissingRules(t *testing.T) {
	api := newFakeVultrAPI(t, nil)
	api.existingFirewalls = []FirewallGroup{{ID: "firewall-existing", Description: "ship-demo-production-firewall"}}
	api.existingFirewallRules = map[string][]FirewallRule{
		"firewall-existing": {
			{IPType: "v4", Protocol: "tcp", Port: "80", Subnet: "0.0.0.0", SubnetSize: 0, Notes: "ship-http"},
		},
	}
	firewall, err := api.client().EnsureFirewall(context.Background(), "demo", "production", *testEnvironment(1).Provider.Vultr)
	if err != nil {
		t.Fatal(err)
	}
	if firewall.ID != "firewall-existing" {
		t.Fatalf("firewall = %+v", firewall)
	}
	if len(api.firewalls) != 0 {
		t.Fatalf("created firewalls = %+v", api.firewalls)
	}
	if len(api.firewallRules["firewall-existing"]) != 6 {
		t.Fatalf("added rules = %+v", api.firewallRules["firewall-existing"])
	}
}

func TestReconcileLeavesMatchingInstancesAndReportsExtra(t *testing.T) {
	existing := []Instance{
		{ID: "instance-1", Label: "web-1", Tags: shipTags("demo", "production", "web")},
		{ID: "instance-old", Label: "web-old", Tags: shipTags("demo", "production", "web")},
	}
	api := newFakeVultrAPI(t, existing)
	result, err := api.client().Reconcile(context.Background(), "demo", "production", testEnvironment(2))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Existing) != 1 || result.Existing[0].ID != "instance-1" {
		t.Fatalf("existing = %+v", result.Existing)
	}
	if len(result.Created) != 1 || result.Created[0].Name != "web-2" {
		t.Fatalf("created = %+v", result.Created)
	}
	if len(result.Extra) != 1 || result.Extra[0].Name != "web-old" {
		t.Fatalf("extra = %+v", result.Extra)
	}
}

func TestListInstancesPaginatesAndFiltersTags(t *testing.T) {
	api := newFakeVultrAPI(t, nil)
	api.pages = map[string]listPage{
		"": {
			instances: []Instance{
				{ID: "unmanaged", Label: "other", Tags: []string{"not-ship"}},
				{ID: "web-1", Label: "web-1", MainIP: "192.0.2.10", Tags: shipTags("demo", "production", "web")},
			},
			next: "cursor-2",
		},
		"cursor-2": {
			instances: []Instance{
				{ID: "staging", Label: "web-staging", Tags: shipTags("demo", "staging", "web")},
				{ID: "worker-1", Label: "worker-1", MainIP: "192.0.2.11", Tags: shipTags("demo", "production", "worker")},
			},
		},
	}
	instances, err := api.client().ListInstances(context.Background(), "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 2 {
		t.Fatalf("instances = %+v", instances)
	}
	if got, want := strings.Join(api.cursors, ","), ",cursor-2"; got != want {
		t.Fatalf("cursors = %q, want %q", got, want)
	}
}

func TestDeleteInstanceDeletesByID(t *testing.T) {
	api := newFakeVultrAPI(t, []Instance{{ID: "instance-1", Label: "web-1", Tags: shipTags("demo", "production", "web")}})
	if err := api.client().Delete(context.Background(), provider.Host{ID: "instance-1"}); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(api.deletes, ","), "instance-1"; got != want {
		t.Fatalf("deleted ids = %q, want %q", got, want)
	}
}

func TestReconcileDryRunDoesNotCallAPI(t *testing.T) {
	client := Client{DryRun: true}
	result, err := client.Reconcile(context.Background(), "demo", "production", testEnvironment(1))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Desired) != 1 || len(result.Created) != 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestCredentialChecks(t *testing.T) {
	checks := Client{}.CredentialChecks(func(key string) (string, bool) {
		if key != "VULTR_API_KEY" {
			t.Fatalf("lookup key = %q", key)
		}
		return "token", true
	})
	if len(checks) != 1 || !checks[0].Present || checks[0].Name != "vultr token" {
		t.Fatalf("checks = %+v", checks)
	}
}

func testEnvironment(count int) config.Environment {
	return config.Environment{
		Provider: config.ProviderConfig{Vultr: &config.VultrConfig{
			Region:            "ewr",
			Plan:              "vc2-2c-4gb",
			OSID:              2284,
			Hostname:          "ship-host",
			UserData:          "#cloud-config\npackages: [htop]\n",
			SSHKeyIDs:         []string{"ship-key"},
			SSHAllowedCIDRs:   []string{"203.0.113.0/24"},
			Backups:           boolPointer(true),
			IPv6:              boolPointer(true),
			DDoSProtection:    boolPointer(true),
			ActivationEmail:   boolPointer(false),
			EnableVPC:         boolPointer(true),
			VPCIDs:            []string{"vpc-123"},
			VPCOnly:           boolPointer(false),
			DisablePublicIPv4: boolPointer(false),
			ReservedIPv4:      "192.0.2.50",
			UserScheme:        "limited",
			ScriptID:          "script-123",
			AppVariables:      map[string]string{"admin_email": "ops@example.com"},
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {Count: count},
		}},
	}
}

func boolPointer(value bool) *bool {
	return &value
}

func shipTags(project, environment, pool string) []string {
	return []string{
		"ship:managed-by=ship",
		"ship:project=" + project,
		"ship:environment=" + environment,
		"ship:pool=" + pool,
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsFirewallRule(rules []createFirewallRuleRequest, want createFirewallRuleRequest) bool {
	for _, rule := range rules {
		if rule == want {
			return true
		}
	}
	return false
}

type fakeVultrAPI struct {
	server                *httptest.Server
	existing              []Instance
	pages                 map[string]listPage
	existingFirewalls     []FirewallGroup
	existingFirewallRules map[string][]FirewallRule
	creates               []createInstanceRequest
	firewalls             []createFirewallRequest
	firewallRules         map[string][]createFirewallRuleRequest
	deletes               []string
	cursors               []string
	nextID                int
}

type listPage struct {
	instances []Instance
	next      string
}

type createInstanceRequest struct {
	Region            string            `json:"region"`
	Plan              string            `json:"plan"`
	Label             string            `json:"label"`
	Tags              []string          `json:"tags"`
	Hostname          string            `json:"hostname"`
	SSHKeyID          []string          `json:"sshkey_id"`
	FirewallGroupID   string            `json:"firewall_group_id"`
	Backups           string            `json:"backups"`
	EnableIPv6        *bool             `json:"enable_ipv6"`
	DDoSProtection    *bool             `json:"ddos_protection"`
	ActivationEmail   *bool             `json:"activation_email"`
	EnableVPC         *bool             `json:"enable_vpc"`
	AttachVPC         []string          `json:"attach_vpc"`
	VPCOnly           *bool             `json:"vpc_only"`
	DisablePublicIPv4 *bool             `json:"disable_public_ipv4"`
	ReservedIPv4      string            `json:"reserved_ipv4"`
	UserScheme        string            `json:"user_scheme"`
	ScriptID          string            `json:"script_id"`
	AppVariables      map[string]string `json:"app_variables"`
	UserData          string            `json:"user_data"`
	OSID              int               `json:"os_id"`
	ImageID           string            `json:"image_id"`
	SnapshotID        string            `json:"snapshot_id"`
	AppID             int               `json:"app_id"`
}

type createFirewallRequest struct {
	Description string `json:"description"`
}

type createFirewallRuleRequest struct {
	IPType     string `json:"ip_type"`
	Protocol   string `json:"protocol"`
	Port       string `json:"port"`
	Subnet     string `json:"subnet"`
	SubnetSize int    `json:"subnet_size"`
	Notes      string `json:"notes"`
}

func newFakeVultrAPI(t *testing.T, existing []Instance) *fakeVultrAPI {
	t.Helper()
	api := &fakeVultrAPI{
		existing:              existing,
		existingFirewallRules: map[string][]FirewallRule{},
		firewallRules:         map[string][]createFirewallRuleRequest{},
		nextID:                100,
	}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/instances":
			cursor := r.URL.Query().Get("cursor")
			api.cursors = append(api.cursors, cursor)
			page, ok := api.pages[cursor]
			if !ok {
				page = listPage{instances: api.existing}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"instances": page.instances,
				"meta": map[string]any{
					"links": map[string]any{"next": page.next},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/firewalls":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"firewall_groups": api.existingFirewalls,
				"meta":            map[string]any{"links": map[string]any{}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/firewalls":
			var req createFirewallRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			api.firewalls = append(api.firewalls, req)
			id := fmt.Sprintf("firewall-%d", len(api.firewalls))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"firewall_group": FirewallGroup{ID: id, Description: req.Description},
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/firewalls/") && strings.HasSuffix(r.URL.Path, "/rules"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/firewalls/"), "/rules")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"firewall_rules": api.existingFirewallRules[id],
				"meta":           map[string]any{"links": map[string]any{}},
			})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/firewalls/") && strings.HasSuffix(r.URL.Path, "/rules"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/firewalls/"), "/rules")
			var req createFirewallRuleRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			api.firewallRules[id] = append(api.firewallRules[id], req)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && r.URL.Path == "/instances":
			var req createInstanceRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			api.creates = append(api.creates, req)
			api.nextID++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"instance": Instance{
					ID:     fmt.Sprintf("instance-%d", api.nextID),
					Label:  req.Label,
					MainIP: fmt.Sprintf("192.0.2.%d", api.nextID-99),
					Tags:   req.Tags,
				},
				"job_ids": []string{"job-1"},
			})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/instances/"):
			api.deletes = append(api.deletes, strings.TrimPrefix(r.URL.Path, "/instances/"))
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (api *fakeVultrAPI) client() Client {
	return Client{
		Token:   "test-token",
		HTTP:    api.server.Client(),
		BaseURL: api.server.URL,
	}
}
