package digitalocean

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

var _ provider.Provider = Client{}

func TestDesiredDropletsUsesPoolsAndProviderOptions(t *testing.T) {
	droplets := DesiredDroplets(testEnvironment(2))
	if len(droplets) != 2 {
		t.Fatalf("len = %d", len(droplets))
	}
	if droplets[0].Name != "web-1" || droplets[0].Pool != "web" {
		t.Fatalf("unexpected droplet: %+v", droplets[0])
	}
	if droplets[0].Location != "nyc3" || droplets[0].Size != "s-2vcpu-4gb" || droplets[0].Image != "ubuntu-24-04-x64" {
		t.Fatalf("provider options missing from plan: %+v", droplets[0])
	}
}

func TestDesiredDropletTagsRoundTripShipLabels(t *testing.T) {
	droplets := DesiredDropletsFor("demo/api", "production", testEnvironment(1))
	if len(droplets) != 1 {
		t.Fatalf("len = %d", len(droplets))
	}
	labels := labelsFromTags(tagsForPlan(droplets[0]))
	want := map[string]string{
		LabelManagedBy:   "ship",
		LabelProject:     "demo/api",
		LabelEnvironment: "production",
		LabelPool:        "web",
	}
	for key, value := range want {
		if labels[key] != value {
			t.Fatalf("label %s = %q, want %q from %+v", key, labels[key], value, labels)
		}
	}
}

func TestCreateHostProvisionsReplacement(t *testing.T) {
	api := newFakeDigitalOceanAPI(t, nil)
	env := testEnvironment(1)
	plans := DesiredDropletsFor("demo", "production", env)
	if len(plans) == 0 {
		t.Fatal("no desired plans")
	}
	host, err := api.client().CreateHost(context.Background(), "demo", "production", env, plans[0])
	if err != nil {
		t.Fatal(err)
	}
	if host.ID == "" || host.PublicAddress == "" {
		t.Fatalf("created droplet missing facts: %+v", host)
	}
	if len(api.creates) != 1 {
		t.Fatalf("creates = %d", len(api.creates))
	}
	if len(api.firewalls) != 1 {
		t.Fatalf("firewall not ensured through the shared backend: %d", len(api.firewalls))
	}
}

func TestReconcileCreatesMissingDroplets(t *testing.T) {
	api := newFakeDigitalOceanAPI(t, nil)
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
	if create.Name != "web-1" || create.Region != "nyc3" || create.Size != "s-2vcpu-4gb" || create.Image != "ubuntu-24-04-x64" {
		t.Fatalf("create request = %+v", create)
	}
	if create.UserData != "#cloud-config\npackages: [htop]\n" {
		t.Fatalf("user_data = %q", create.UserData)
	}
	if len(create.SSHKeys) != 1 || create.SSHKeys[0] != "ship-key" {
		t.Fatalf("ssh_keys = %+v", create.SSHKeys)
	}
	if create.VPCUUID != "vpc-uuid" || create.Monitoring == nil || !*create.Monitoring || create.IPv6 == nil || !*create.IPv6 {
		t.Fatalf("optional droplet features = %+v", create)
	}
	labels := labelsFromTags(create.Tags)
	for _, want := range []string{LabelManagedBy, LabelProject, LabelEnvironment, LabelPool} {
		if labels[want] == "" {
			t.Fatalf("create tags missing %s in %+v", want, create.Tags)
		}
	}
	if len(api.firewalls) != 1 {
		t.Fatalf("firewalls = %d", len(api.firewalls))
	}
	if !contains(api.firewalls[0].Tags, tagForLabel(LabelEnvironment, "production")) {
		t.Fatalf("firewall tags = %+v", api.firewalls[0].Tags)
	}
	if result.Created[0].ID == "" || result.Created[0].PublicAddress == "" {
		t.Fatalf("created droplet missing facts: %+v", result.Created[0])
	}
}

func TestReconcileLeavesMatchingDropletsAndReportsExtra(t *testing.T) {
	existing := []Droplet{
		{ID: 10, Name: "web-1", Tags: shipTags("demo", "production", "web")},
		{ID: 11, Name: "web-old", Tags: shipTags("demo", "production", "web")},
	}
	api := newFakeDigitalOceanAPI(t, existing)
	result, err := api.client().Reconcile(context.Background(), "demo", "production", testEnvironment(2))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Existing) != 1 || result.Existing[0].ID != "10" {
		t.Fatalf("existing = %+v", result.Existing)
	}
	if len(result.Created) != 1 || result.Created[0].Name != "web-2" {
		t.Fatalf("created = %+v", result.Created)
	}
	if len(result.Extra) != 1 || result.Extra[0].Name != "web-old" {
		t.Fatalf("extra = %+v", result.Extra)
	}
}

func TestListDropletsPaginatesAndFiltersTags(t *testing.T) {
	api := newFakeDigitalOceanAPI(t, nil)
	api.pages = map[int]listPage{
		1: {
			droplets: []Droplet{
				{ID: 8, Name: "other", Tags: []string{"not-ship"}},
				{ID: 10, Name: "web-1", Networks: publicNetwork("192.0.2.10"), Tags: shipTags("demo", "production", "web")},
			},
			next: "https://api.digitalocean.com/v2/droplets?page=2",
		},
		2: {
			droplets: []Droplet{
				{ID: 11, Name: "staging", Tags: shipTags("demo", "staging", "web")},
				{ID: 12, Name: "worker-1", Networks: publicNetwork("192.0.2.11"), Tags: shipTags("demo", "production", "worker")},
			},
		},
	}
	droplets, err := api.client().ListDroplets(context.Background(), "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(droplets) != 2 {
		t.Fatalf("droplets = %+v", droplets)
	}
	if got, want := strings.Join(api.listPages, ","), "1,2"; got != want {
		t.Fatalf("pages = %q, want %q", got, want)
	}
	if api.tagName != tagForLabel(LabelEnvironment, "production") {
		t.Fatalf("tag_name = %q", api.tagName)
	}
}

func TestDeleteDropletDeletesByID(t *testing.T) {
	api := newFakeDigitalOceanAPI(t, []Droplet{{ID: 10, Name: "web-1", Tags: shipTags("demo", "production", "web")}})
	if err := api.client().Delete(context.Background(), provider.Host{ID: "10"}); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(api.deletes, ","), "10"; got != want {
		t.Fatalf("deleted ids = %q, want %q", got, want)
	}
}

func TestEnsureFirewallAddsEnvironmentTagToExistingFirewall(t *testing.T) {
	api := newFakeDigitalOceanAPI(t, nil)
	api.existingFirewalls = []Firewall{{ID: "fw-1", Name: "ship-demo-production-firewall"}}
	_, err := api.client().EnsureFirewall(context.Background(), "demo", "production", digitalOceanConfig(testEnvironment(1)))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(api.firewallTagAdds, ","), "fw-1:"+tagForLabel(LabelEnvironment, "production"); got != want {
		t.Fatalf("firewall tag adds = %q, want %q", got, want)
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
		if key != "DIGITALOCEAN_TOKEN" {
			t.Fatalf("lookup key = %q", key)
		}
		return "token", true
	})
	if len(checks) != 1 || !checks[0].Present || checks[0].Name != "digitalocean token" {
		t.Fatalf("checks = %+v", checks)
	}
}

func testEnvironment(count int) config.Environment {
	monitoring := true
	ipv6 := true
	return config.Environment{
		Provider: config.ProviderConfig{DigitalOcean: &config.DigitalOceanConfig{
			Region:          "nyc3",
			Size:            "s-2vcpu-4gb",
			Image:           "ubuntu-24-04-x64",
			UserData:        "#cloud-config\npackages: [htop]\n",
			SSHKeys:         []string{"ship-key"},
			VPCUUID:         "vpc-uuid",
			Monitoring:      &monitoring,
			IPv6:            &ipv6,
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {Count: count},
		}},
	}
}

func digitalOceanConfig(env config.Environment) config.DigitalOceanConfig {
	return *env.Provider.DigitalOcean
}

func shipTags(project, environment, pool string) []string {
	return tagsFromLabels(provider.ShipLabels(project, environment, pool))
}

func publicNetwork(ip string) Networks {
	return Networks{V4: []NetworkV4{{IPAddress: ip, Type: "public"}}}
}

type fakeDigitalOceanAPI struct {
	server            *httptest.Server
	existing          []Droplet
	pages             map[int]listPage
	creates           []createDropletRequest
	deletes           []string
	tags              []string
	firewalls         []createFirewallRequest
	existingFirewalls []Firewall
	firewallTagAdds   []string
	listPages         []string
	tagName           string
	nextID            int64
}

type listPage struct {
	droplets []Droplet
	next     string
}

type createDropletRequest struct {
	Name       string   `json:"name"`
	Region     string   `json:"region"`
	Size       string   `json:"size"`
	Image      string   `json:"image"`
	SSHKeys    []string `json:"ssh_keys"`
	Tags       []string `json:"tags"`
	UserData   string   `json:"user_data"`
	VPCUUID    string   `json:"vpc_uuid"`
	Monitoring *bool    `json:"monitoring"`
	Backups    *bool    `json:"backups"`
	IPv6       *bool    `json:"ipv6"`
}

type createFirewallRequest struct {
	Name          string           `json:"name"`
	Tags          []string         `json:"tags"`
	InboundRules  []map[string]any `json:"inbound_rules"`
	OutboundRules []map[string]any `json:"outbound_rules"`
}

func newFakeDigitalOceanAPI(t *testing.T, existing []Droplet) *fakeDigitalOceanAPI {
	t.Helper()
	api := &fakeDigitalOceanAPI{existing: existing, nextID: 100}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/tags":
			var req struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			api.tags = append(api.tags, req.Name)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"tag": map[string]any{"name": req.Name}})
		case r.Method == http.MethodGet && r.URL.Path == "/firewalls":
			_ = json.NewEncoder(w).Encode(map[string]any{"firewalls": api.existingFirewalls, "links": map[string]any{"pages": map[string]any{}}})
		case r.Method == http.MethodPost && r.URL.Path == "/firewalls":
			var req createFirewallRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			api.firewalls = append(api.firewalls, req)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"firewall": Firewall{ID: "fw-1", Name: req.Name, Tags: req.Tags},
			})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/firewalls/") && strings.HasSuffix(r.URL.Path, "/tags"):
			var req struct {
				Tags []string `json:"tags"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			firewallID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/firewalls/"), "/tags")
			for _, tag := range req.Tags {
				api.firewallTagAdds = append(api.firewallTagAdds, firewallID+":"+tag)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/droplets":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if page == 0 {
				page = 1
			}
			api.listPages = append(api.listPages, strconv.Itoa(page))
			api.tagName = r.URL.Query().Get("tag_name")
			pageData, ok := api.pages[page]
			if !ok {
				pageData = listPage{droplets: api.existing}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"droplets": pageData.droplets,
				"links": map[string]any{
					"pages": map[string]any{"next": pageData.next},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/droplets":
			var req createDropletRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			api.creates = append(api.creates, req)
			api.nextID++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"droplet": Droplet{
					ID:       api.nextID,
					Name:     req.Name,
					Tags:     req.Tags,
					Networks: publicNetwork(fmt.Sprintf("192.0.2.%d", api.nextID-99)),
				},
			})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/droplets/"):
			api.deletes = append(api.deletes, strings.TrimPrefix(r.URL.Path, "/droplets/"))
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (api *fakeDigitalOceanAPI) client() Client {
	return Client{
		Token:   "test-token",
		HTTP:    api.server.Client(),
		BaseURL: api.server.URL,
	}
}
