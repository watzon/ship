package vultr

import (
	"context"
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
	if create.OSID != 2284 || create.ImageID != "" || create.SnapshotID != "" || create.AppID != 0 {
		t.Fatalf("create source = %+v", create)
	}
	if len(create.SSHKeyID) != 1 || create.SSHKeyID[0] != "ship-key" {
		t.Fatalf("sshkey_id = %+v", create.SSHKeyID)
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
			Region:    "ewr",
			Plan:      "vc2-2c-4gb",
			OSID:      2284,
			SSHKeyIDs: []string{"ship-key"},
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {Count: count},
		}},
	}
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

type fakeVultrAPI struct {
	server   *httptest.Server
	existing []Instance
	pages    map[string]listPage
	creates  []createInstanceRequest
	deletes  []string
	cursors  []string
	nextID   int
}

type listPage struct {
	instances []Instance
	next      string
}

type createInstanceRequest struct {
	Region     string   `json:"region"`
	Plan       string   `json:"plan"`
	Label      string   `json:"label"`
	Tags       []string `json:"tags"`
	SSHKeyID   []string `json:"sshkey_id"`
	OSID       int      `json:"os_id"`
	ImageID    string   `json:"image_id"`
	SnapshotID string   `json:"snapshot_id"`
	AppID      int      `json:"app_id"`
}

func newFakeVultrAPI(t *testing.T, existing []Instance) *fakeVultrAPI {
	t.Helper()
	api := &fakeVultrAPI{existing: existing, nextID: 100}
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
