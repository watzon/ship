package linode

import (
	"context"
	"encoding/base64"
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

func TestDesiredInstancesUsesPoolsAndProviderOptions(t *testing.T) {
	instances := DesiredInstances(testEnvironment(2))
	if len(instances) != 2 {
		t.Fatalf("len = %d", len(instances))
	}
	if instances[0].Name != "web-1" || instances[0].Pool != "web" {
		t.Fatalf("unexpected instance: %+v", instances[0])
	}
	if instances[0].Location != "us-east" || instances[0].Size != "g6-standard-2" || instances[0].Image != "linode/ubuntu24.04" {
		t.Fatalf("provider options missing from plan: %+v", instances[0])
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

func TestCreateHostProvisionsReplacement(t *testing.T) {
	api := newFakeLinodeAPI(t, nil)
	env := testEnvironment(1)
	plans := DesiredInstancesFor("demo", "production", env)
	if len(plans) == 0 {
		t.Fatal("no desired plans")
	}
	host, err := api.client().CreateHost(context.Background(), "demo", "production", env, plans[0])
	if err != nil {
		t.Fatal(err)
	}
	if host.ID == "" || host.PublicAddress == "" {
		t.Fatalf("created instance missing facts: %+v", host)
	}
	if len(api.creates) != 1 {
		t.Fatalf("creates = %d", len(api.creates))
	}
	if create := api.creates[0]; create.FirewallID != 400 {
		t.Fatalf("firewall not resolved into the shared backend: %d", create.FirewallID)
	}
}

func TestReconcileCreatesMissingInstances(t *testing.T) {
	api := newFakeLinodeAPI(t, nil)
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
	if create.Label != "web-1" || create.Region != "us-east" || create.Type != "g6-standard-2" || create.Image != "linode/ubuntu24.04" {
		t.Fatalf("create request = %+v", create)
	}
	if got, want := create.Metadata.UserData, base64.StdEncoding.EncodeToString([]byte("#cloud-config\npackages: [htop]\n")); got != want {
		t.Fatalf("metadata.user_data = %q, want %q", got, want)
	}
	if len(create.AuthorizedKeys) != 1 || create.AuthorizedKeys[0] != "ssh-ed25519 AAAA..." {
		t.Fatalf("authorized_keys = %+v", create.AuthorizedKeys)
	}
	if create.PrivateIP == nil || !*create.PrivateIP || create.BackupsEnabled == nil || !*create.BackupsEnabled {
		t.Fatalf("optional instance features = %+v", create)
	}
	if create.FirewallID != 400 {
		t.Fatalf("firewall_id = %d", create.FirewallID)
	}
	for _, want := range []string{"ship:managed-by=ship", "ship:project=demo", "ship:environment=production", "ship:pool=web"} {
		if !contains(create.Tags, want) {
			t.Fatalf("create tags missing %q in %+v", want, create.Tags)
		}
	}
	if len(api.firewalls) != 1 || api.firewalls[0].Label != "ship-demo-production-firewall" {
		t.Fatalf("firewalls = %+v", api.firewalls)
	}
	if result.Created[0].ID == "" || result.Created[0].PublicAddress == "" {
		t.Fatalf("created instance missing facts: %+v", result.Created[0])
	}
}

func TestReconcileLeavesMatchingInstancesAndReportsExtra(t *testing.T) {
	existing := []Instance{
		{ID: 10, Label: "web-1", Tags: shipTags("demo", "production", "web")},
		{ID: 11, Label: "web-old", Tags: shipTags("demo", "production", "web")},
	}
	api := newFakeLinodeAPI(t, existing)
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

func TestListInstancesPaginatesAndFiltersTags(t *testing.T) {
	api := newFakeLinodeAPI(t, nil)
	api.pages = map[int]listPage{
		1: {
			instances: []Instance{
				{ID: 8, Label: "other", Tags: []string{"not-ship"}},
				{ID: 10, Label: "web-1", IPv4: []string{"192.0.2.10"}, Tags: shipTags("demo", "production", "web")},
			},
			pages: 2,
		},
		2: {
			instances: []Instance{
				{ID: 11, Label: "staging", Tags: shipTags("demo", "staging", "web")},
				{ID: 12, Label: "worker-1", IPv4: []string{"192.0.2.11"}, Tags: shipTags("demo", "production", "worker")},
			},
			pages: 2,
		},
	}
	instances, err := api.client().ListInstances(context.Background(), "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 2 {
		t.Fatalf("instances = %+v", instances)
	}
	if got, want := strings.Join(api.listPages, ","), "1,2"; got != want {
		t.Fatalf("pages = %q, want %q", got, want)
	}
}

func TestDeleteInstanceDeletesByID(t *testing.T) {
	api := newFakeLinodeAPI(t, []Instance{{ID: 10, Label: "web-1", Tags: shipTags("demo", "production", "web")}})
	if err := api.client().Delete(context.Background(), provider.Host{ID: "10"}); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(api.deletes, ","), "10"; got != want {
		t.Fatalf("deleted ids = %q, want %q", got, want)
	}
}

func TestEnsureFirewallUsesExistingFirewall(t *testing.T) {
	api := newFakeLinodeAPI(t, nil)
	api.existingFirewalls = []Firewall{{ID: 444, Label: "ship-demo-production-firewall"}}
	firewall, err := api.client().EnsureFirewall(context.Background(), "demo", "production", linodeConfig(testEnvironment(1)))
	if err != nil {
		t.Fatal(err)
	}
	if firewall.ID != 444 {
		t.Fatalf("firewall = %+v", firewall)
	}
	if len(api.firewalls) != 0 {
		t.Fatalf("created firewalls = %+v", api.firewalls)
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
		if key != "LINODE_TOKEN" {
			t.Fatalf("lookup key = %q", key)
		}
		return "token", true
	})
	if len(checks) != 1 || !checks[0].Present || checks[0].Name != "linode token" {
		t.Fatalf("checks = %+v", checks)
	}
}

func testEnvironment(count int) config.Environment {
	privateIP := true
	backups := true
	return config.Environment{
		Provider: config.ProviderConfig{Linode: &config.LinodeConfig{
			Region:          "us-east",
			Type:            "g6-standard-2",
			Image:           "linode/ubuntu24.04",
			UserData:        "#cloud-config\npackages: [htop]\n",
			AuthorizedKeys:  []string{"ssh-ed25519 AAAA..."},
			PrivateIP:       &privateIP,
			Backups:         &backups,
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {Count: count},
		}},
	}
}

func linodeConfig(env config.Environment) config.LinodeConfig {
	return *env.Provider.Linode
}

func shipTags(project, environment, pool string) []string {
	return tagsFromLabels(provider.ShipLabels(project, environment, pool))
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type fakeLinodeAPI struct {
	server            *httptest.Server
	existing          []Instance
	pages             map[int]listPage
	creates           []createInstanceRequest
	deletes           []string
	firewalls         []createFirewallRequest
	existingFirewalls []Firewall
	listPages         []string
	nextID            int64
}

type listPage struct {
	instances []Instance
	pages     int
}

type createInstanceRequest struct {
	Label           string   `json:"label"`
	Region          string   `json:"region"`
	Type            string   `json:"type"`
	Image           string   `json:"image"`
	AuthorizedKeys  []string `json:"authorized_keys"`
	AuthorizedUsers []string `json:"authorized_users"`
	Tags            []string `json:"tags"`
	Metadata        struct {
		UserData string `json:"user_data"`
	} `json:"metadata"`
	PrivateIP      *bool `json:"private_ip"`
	BackupsEnabled *bool `json:"backups_enabled"`
	FirewallID     int64 `json:"firewall_id"`
}

type createFirewallRequest struct {
	Label string         `json:"label"`
	Rules map[string]any `json:"rules"`
}

func newFakeLinodeAPI(t *testing.T, existing []Instance) *fakeLinodeAPI {
	t.Helper()
	api := &fakeLinodeAPI{existing: existing, nextID: 100}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/networking/firewalls":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data":  api.existingFirewalls,
				"page":  1,
				"pages": 1,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/networking/firewalls":
			var req createFirewallRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			api.firewalls = append(api.firewalls, req)
			_ = json.NewEncoder(w).Encode(Firewall{ID: 400, Label: req.Label})
		case r.Method == http.MethodGet && r.URL.Path == "/linode/instances":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if page == 0 {
				page = 1
			}
			api.listPages = append(api.listPages, strconv.Itoa(page))
			pageData, ok := api.pages[page]
			if !ok {
				pageData = listPage{instances: api.existing, pages: 1}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data":  pageData.instances,
				"page":  page,
				"pages": pageData.pages,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/linode/instances":
			var req createInstanceRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			api.creates = append(api.creates, req)
			api.nextID++
			_ = json.NewEncoder(w).Encode(Instance{
				ID:    api.nextID,
				Label: req.Label,
				IPv4:  []string{fmt.Sprintf("192.0.2.%d", api.nextID-99)},
				Tags:  req.Tags,
			})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/linode/instances/"):
			api.deletes = append(api.deletes, strings.TrimPrefix(r.URL.Path, "/linode/instances/"))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (api *fakeLinodeAPI) client() Client {
	return Client{
		Token:   "test-token",
		HTTP:    api.server.Client(),
		BaseURL: api.server.URL,
	}
}
