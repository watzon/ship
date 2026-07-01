package cloudscale

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
		Provider: config.ProviderConfig{Cloudscale: &config.CloudscaleConfig{
			Zone:     "rma1",
			Flavor:   "flex-4-2",
			Image:    "debian-13",
			SSHKeys:  []string{"ssh-ed25519 AAAA..."},
			UserData: "#cloud-config\npackages: [htop]\n",
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {
				Count:    1,
				Size:     "flex-8-4",
				Image:    "ubuntu-24.04",
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
	if plan.Location != "rma1" || plan.Size != "flex-8-4" || plan.Image != "ubuntu-24.04" {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.UserData != "#cloud-config\npackages: [curl]\n" {
		t.Fatalf("user data = %q", plan.UserData)
	}
	if plan.Labels["owner"] != "platform" || plan.Labels[provider.LabelProject] != "demo" {
		t.Fatalf("labels = %+v", plan.Labels)
	}
}

func TestCreateServerSendsBearerTokenAndRichPayload(t *testing.T) {
	var createBody map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/servers" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		decodeJSON(t, r, &createBody)
		writeJSON(t, w, http.StatusCreated, Server{
			UUID: "server-1",
			Name: "web-1",
			Tags: provider.ShipLabels("demo", "production", "web"),
			Interfaces: []Interface{{
				Type:      "public",
				Addresses: []Address{{Version: 4, Address: "198.51.100.90"}},
			}},
		})
	}))
	defer api.Close()

	usePrivate := true
	useIPv6 := false
	client := testClient(api)
	server, err := client.CreateServer(context.Background(), provider.HostPlan{
		Project:     "demo",
		Environment: "production",
		Name:        "web-1",
		Pool:        "web",
		Location:    "rma1",
		Size:        "flex-4-2",
		Image:       "debian-13",
		UserData:    "#cloud-config\nruncmd: [true]\n",
		Labels:      provider.ShipLabels("demo", "production", "web"),
	}, config.CloudscaleConfig{
		SSHKeys:           []string{"ssh-ed25519 AAAA..."},
		UserDataHandling:  "pass-through",
		VolumeSizeGB:      50,
		BulkVolumeSizeGB:  100,
		UsePrivateNetwork: &usePrivate,
		UseIPv6:           &useIPv6,
		Volumes:           []config.CloudscaleVolume{{SizeGB: 20, Type: "ssd"}},
	}, []string{"group-1"})
	if err != nil {
		t.Fatal(err)
	}
	if server.UUID != "server-1" {
		t.Fatalf("server = %+v", server)
	}
	if createBody["name"] != "web-1" ||
		createBody["zone"] != "rma1" ||
		createBody["flavor"] != "flex-4-2" ||
		createBody["image"] != "debian-13" ||
		createBody["volume_size_gb"] != float64(50) ||
		createBody["bulk_volume_size_gb"] != float64(100) ||
		createBody["use_private_network"] != true ||
		createBody["use_ipv6"] != false {
		t.Fatalf("create body = %+v", createBody)
	}
	if createBody["user_data"] != "#cloud-config\nruncmd: [true]\n" {
		t.Fatalf("user_data = %q", createBody["user_data"])
	}
	if createBody["user_data_handling"] != "pass-through" {
		t.Fatalf("user_data_handling = %q", createBody["user_data_handling"])
	}
	sshKeys := createBody["ssh_keys"].([]any)
	if sshKeys[0] != "ssh-ed25519 AAAA..." {
		t.Fatalf("ssh_keys = %+v", sshKeys)
	}
	serverGroups := createBody["server_groups"].([]any)
	if serverGroups[0] != "group-1" {
		t.Fatalf("server_groups = %+v", serverGroups)
	}
	labels := createBody["tags"].(map[string]any)
	if labels[LabelManagedBy] != "ship" || labels[LabelPool] != "web" {
		t.Fatalf("tags = %+v", labels)
	}
}

func TestListServersFiltersByShipTagsAndReturnsPublicAddress(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/servers" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		if r.URL.Query().Get("tag:"+LabelManagedBy) != "ship" ||
			r.URL.Query().Get("tag:"+LabelProject) != "demo" ||
			r.URL.Query().Get("tag:"+LabelEnvironment) != "production" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		writeJSON(t, w, http.StatusOK, []Server{
			{
				UUID: "server-1",
				Name: "web-1",
				Tags: provider.ShipLabels("demo", "production", "web"),
				Interfaces: []Interface{{
					Type:      "public",
					Addresses: []Address{{Version: 6, Address: "2001:db8::1"}, {Version: 4, Address: "198.51.100.91"}},
				}},
			},
			{UUID: "server-2", Name: "staging", Tags: provider.ShipLabels("demo", "staging", "web")},
			{UUID: "server-3", Name: "unmanaged", Tags: map[string]string{"owner": "ops"}},
		})
	}))
	defer api.Close()

	hosts, err := testClient(api).List(context.Background(), "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].ID != "server-1" || hosts[0].PublicAddress != "198.51.100.91" {
		t.Fatalf("hosts = %+v", hosts)
	}
}

func TestEnsureServerGroupCreatesAntiAffinityGroup(t *testing.T) {
	var groupBody map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/server-groups":
			writeJSON(t, w, http.StatusOK, []ServerGroup{})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/server-groups":
			decodeJSON(t, r, &groupBody)
			writeJSON(t, w, http.StatusCreated, ServerGroup{
				UUID: "group-1",
				Name: "ship-demo-production",
				Type: "anti-affinity",
				Zone: SlugRef{Slug: "rma1"},
				Tags: provider.ShipLabels("demo", "production", "server-group"),
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer api.Close()

	group, err := testClient(api).EnsureServerGroup(context.Background(), "demo", "production", config.CloudscaleConfig{
		Zone: "rma1",
		ServerGroup: config.CloudscaleServerGroupConfig{
			Managed: boolPtr(true),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if group.UUID != "group-1" {
		t.Fatalf("group = %+v", group)
	}
	if groupBody["name"] != "ship-demo-production" || groupBody["type"] != "anti-affinity" || groupBody["zone"] != "rma1" {
		t.Fatalf("group body = %+v", groupBody)
	}
	tags := groupBody["tags"].(map[string]any)
	if tags[LabelManagedBy] != "ship" || tags[LabelPool] != "server-group" {
		t.Fatalf("tags = %+v", tags)
	}
}

func TestDeleteServerUsesServerEndpoint(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/servers/server-1" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer api.Close()

	if err := testClient(api).Delete(context.Background(), provider.Host{ID: "server-1"}); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialChecksRequireToken(t *testing.T) {
	checks := (Client{}).CredentialChecks(func(string) (string, bool) { return "", false })
	if len(checks) != 1 {
		t.Fatalf("checks = %+v", checks)
	}
	if checks[0].Present || !strings.Contains(checks[0].MissingMessage, "CLOUDSCALE_API_TOKEN") {
		t.Fatalf("check = %+v", checks[0])
	}
}

func testClient(api *httptest.Server) Client {
	return Client{
		Token:   "token",
		HTTP:    api.Client(),
		BaseURL: api.URL + "/v1",
	}
}

func assertAuth(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer token" {
		t.Fatalf("authorization = %q", got)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
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

func boolPtr(value bool) *bool {
	return &value
}
