package latitude

import (
	"context"
	"encoding/base64"
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
		Provider: config.ProviderConfig{Latitude: &config.LatitudeConfig{
			Project:         "proj-demo",
			Site:            "ASH",
			Plan:            "c2-small-x86",
			OperatingSystem: "ubuntu_24_04_x64_lts",
			SSHKeys:         []string{"ssh-key-1"},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
			UserData:        "#cloud-config\npackages: [htop]\n",
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {
				Count:    1,
				Size:     "c3-medium-x86",
				Image:    "debian_12",
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
	if plan.Location != "ASH" || plan.Size != "c3-medium-x86" || plan.Image != "debian_12" {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.UserData != "#cloud-config\npackages: [curl]\n" {
		t.Fatalf("user data = %q", plan.UserData)
	}
	if plan.Labels["owner"] != "platform" || plan.Labels[provider.LabelProject] != "demo" {
		t.Fatalf("labels = %+v", plan.Labels)
	}
}

func TestCreateServerSendsJSONAPIPayload(t *testing.T) {
	var createBody map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/servers" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		decodeJSON(t, r, &createBody)
		writeJSON(t, w, http.StatusCreated, serverResponse{Data: Server{
			ID: "sv_1",
			Attributes: ServerAttributes{
				Hostname:    "ship-demo-production-web-1",
				PrimaryIPv4: "198.51.100.100",
			},
		}})
	}))
	defer api.Close()

	server, err := testClient(api).CreateServer(context.Background(), provider.HostPlan{
		Project:     "demo",
		Environment: "production",
		Name:        "web-1",
		Pool:        "web",
		Location:    "ASH",
		Size:        "c2-small-x86",
		Image:       "ubuntu_24_04_x64_lts",
		Labels:      provider.ShipLabels("demo", "production", "web"),
	}, config.LatitudeConfig{
		Project:    "proj-demo",
		SSHKeys:    []string{"ssh-key-1", "ssh-key-2"},
		RAID:       "raid-1",
		Billing:    "hourly",
		DiskLayout: []config.LatitudeDiskLayout{{Count: 2, Role: "os", RAIDLevel: "raid-1", Filesystem: "ext4", MountPoint: "/"}},
	}, "ud_1")
	if err != nil {
		t.Fatal(err)
	}
	if server.ID != "sv_1" {
		t.Fatalf("server = %+v", server)
	}
	data := createBody["data"].(map[string]any)
	if data["type"] != "servers" {
		t.Fatalf("data = %+v", data)
	}
	attrs := data["attributes"].(map[string]any)
	if attrs["project"] != "proj-demo" ||
		attrs["plan"] != "c2-small-x86" ||
		attrs["site"] != "ASH" ||
		attrs["operating_system"] != "ubuntu_24_04_x64_lts" ||
		attrs["hostname"] != "ship-demo-production-web-1" ||
		attrs["user_data"] != "ud_1" ||
		attrs["raid"] != "raid-1" ||
		attrs["billing"] != "hourly" {
		t.Fatalf("attributes = %+v", attrs)
	}
	sshKeys := attrs["ssh_keys"].([]any)
	if sshKeys[0] != "ssh-key-1" || sshKeys[1] != "ssh-key-2" {
		t.Fatalf("ssh keys = %+v", sshKeys)
	}
	diskLayout := attrs["disk_layout"].([]any)
	disk := diskLayout[0].(map[string]any)
	if disk["count"] != float64(2) || disk["role"] != "os" || disk["raid_level"] != "raid-1" {
		t.Fatalf("disk layout = %+v", diskLayout)
	}
}

func TestListServersFiltersShipPrefixedHostnames(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		if r.Method == http.MethodGet && r.URL.Path == "/tags" {
			writeJSON(t, w, http.StatusOK, tagsResponse{})
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/servers" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		if r.URL.Query().Get("page[size]") != "100" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		writeJSON(t, w, http.StatusOK, serversResponse{
			Data: []Server{
				{ID: "sv_1", Attributes: ServerAttributes{Hostname: "ship-demo-production-web-1", PrimaryIPv4: "198.51.100.101"}},
				{ID: "sv_2", Attributes: ServerAttributes{Hostname: "ship-demo-staging-web-1", PrimaryIPv4: "198.51.100.102"}},
				{ID: "sv_3", Attributes: ServerAttributes{Hostname: "manual-host", PrimaryIPv4: "198.51.100.103"}},
			},
			Meta: pageMeta{CurrentPage: 1, TotalPages: 1},
		})
	}))
	defer api.Close()

	hosts, err := testClient(api).List(context.Background(), "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].Name != "web-1" || hosts[0].Pool != "web" || hosts[0].PublicAddress != "198.51.100.101" {
		t.Fatalf("hosts = %+v", hosts)
	}
	if hosts[0].Labels[provider.LabelProject] != "demo" || hosts[0].Labels[provider.LabelEnvironment] != "production" {
		t.Fatalf("labels = %+v", hosts[0].Labels)
	}
}

func TestListServersUsesLatitudeTagsWhenAvailable(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/tags":
			writeJSON(t, w, http.StatusOK, tagsResponse{Data: []LatitudeTag{
				tag("tag-managed", tagNameForLabel(LabelManagedBy, "ship")),
				tag("tag-project", tagNameForLabel(LabelProject, "demo")),
				tag("tag-env", tagNameForLabel(LabelEnvironment, "production")),
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/servers":
			if got := r.URL.Query().Get("filter[tags]"); got != "tag-env,tag-managed,tag-project" {
				t.Fatalf("filter tags = %q query=%s", got, r.URL.RawQuery)
			}
			writeJSON(t, w, http.StatusOK, serversResponse{
				Data: []Server{{
					ID: "sv_1",
					Attributes: ServerAttributes{
						Hostname:    "ship-demo-production-web-1",
						PrimaryIPv4: "198.51.100.104",
						Tags: []LatitudeTag{
							tag("tag-managed", tagNameForLabel(LabelManagedBy, "ship")),
							tag("tag-project", tagNameForLabel(LabelProject, "demo")),
							tag("tag-env", tagNameForLabel(LabelEnvironment, "production")),
						},
					},
				}},
				Meta: pageMeta{CurrentPage: 1, TotalPages: 1},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer api.Close()

	hosts, err := testClient(api).List(context.Background(), "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].ID != "sv_1" {
		t.Fatalf("hosts = %+v", hosts)
	}
}

func TestReconcileCreatesAndAttachesShipTags(t *testing.T) {
	managedFirewall := false
	var createdTags []map[string]any
	var patchedServer map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/tags":
			writeJSON(t, w, http.StatusOK, tagsResponse{})
		case r.Method == http.MethodPost && r.URL.Path == "/tags":
			var body map[string]any
			decodeJSON(t, r, &body)
			createdTags = append(createdTags, body)
			name := body["data"].(map[string]any)["attributes"].(map[string]any)["name"].(string)
			writeJSON(t, w, http.StatusCreated, tagResponse{Data: tag("id-"+name, name)})
		case r.Method == http.MethodGet && r.URL.Path == "/servers":
			writeJSON(t, w, http.StatusOK, serversResponse{Meta: pageMeta{CurrentPage: 1, TotalPages: 1}})
		case r.Method == http.MethodPost && r.URL.Path == "/servers":
			writeJSON(t, w, http.StatusCreated, serverResponse{Data: Server{
				ID: "sv_1",
				Attributes: ServerAttributes{
					Hostname:    "ship-demo-production-web-1",
					PrimaryIPv4: "198.51.100.105",
				},
			}})
		case r.Method == http.MethodPatch && r.URL.Path == "/servers/sv_1":
			decodeJSON(t, r, &patchedServer)
			writeJSON(t, w, http.StatusOK, serverResponse{Data: Server{
				ID: "sv_1",
				Attributes: ServerAttributes{
					Hostname:    "ship-demo-production-web-1",
					PrimaryIPv4: "198.51.100.105",
					Tags: []LatitudeTag{
						tag("id-"+tagNameForLabel(LabelManagedBy, "ship"), tagNameForLabel(LabelManagedBy, "ship")),
					},
				},
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer api.Close()

	env := config.Environment{
		Provider: config.ProviderConfig{Latitude: &config.LatitudeConfig{
			Project:         "proj-demo",
			Site:            "ASH",
			Plan:            "c2-small-x86",
			OperatingSystem: "ubuntu_24_04_x64_lts",
			SSHKeys:         []string{"ssh-key-1"},
			Firewall:        config.LatitudeFirewall{Managed: &managedFirewall},
		}},
		Hosts: config.HostsConfig{
			Labels: map[string]string{"owner": "platform"},
			Pools:  map[string]config.Pool{"web": {Count: 1}},
		},
	}
	result, err := testClient(api).Reconcile(context.Background(), "demo", "production", env)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("result = %+v", result)
	}
	if len(createdTags) < 5 {
		t.Fatalf("created tags = %+v", createdTags)
	}
	attrs := patchedServer["data"].(map[string]any)["attributes"].(map[string]any)
	tagIDs := attrs["tags"].([]any)
	if len(tagIDs) < 5 {
		t.Fatalf("patched server = %+v", patchedServer)
	}
}

func TestEnsureFirewallCreatesManagedRules(t *testing.T) {
	var createBody map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/firewalls":
			if got := r.URL.Query().Get("filter[project]"); got != "proj-demo" {
				t.Fatalf("filter project = %q", got)
			}
			writeJSON(t, w, http.StatusOK, firewallsResponse{Meta: pageMeta{CurrentPage: 1, TotalPages: 1}})
		case r.Method == http.MethodPost && r.URL.Path == "/firewalls":
			decodeJSON(t, r, &createBody)
			writeJSON(t, w, http.StatusCreated, firewallResponse{Data: Firewall{
				ID: "fw_1",
				Attributes: FirewallAttributes{
					Name:  "ship-demo-production-firewall",
					Rules: firewallRules(config.LatitudeConfig{SSHAllowedCIDRs: []string{"203.0.113.0/24"}}),
				},
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer api.Close()

	firewall, err := testClient(api).EnsureFirewall(context.Background(), "demo", "production", config.LatitudeConfig{
		Project:         "proj-demo",
		SSHAllowedCIDRs: []string{"203.0.113.0/24"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if firewall.ID != "fw_1" {
		t.Fatalf("firewall = %+v", firewall)
	}
	attrs := createBody["data"].(map[string]any)["attributes"].(map[string]any)
	if attrs["name"] != "ship-demo-production-firewall" || attrs["project"] != "proj-demo" {
		t.Fatalf("attrs = %+v", attrs)
	}
	rules := attrs["rules"].([]any)
	if len(rules) != 4 {
		t.Fatalf("rules = %+v", rules)
	}
	ssh := rules[0].(map[string]any)
	if ssh["from"] != "203.0.113.0/24" || ssh["to"] != "ANY" || ssh["port"] != "22" || ssh["protocol"] != "TCP" {
		t.Fatalf("ssh rule = %+v", ssh)
	}
}

func TestEnsureFirewallUpdatesExistingWithMissingRules(t *testing.T) {
	var patchBody map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/firewalls":
			writeJSON(t, w, http.StatusOK, firewallsResponse{
				Data: []Firewall{{
					ID: "fw_1",
					Attributes: FirewallAttributes{
						Name:  "ship-demo-production-firewall",
						Rules: []FirewallRule{firewallRule("ANY", "ANY", "80", "TCP", "Allow HTTP")},
					},
				}},
				Meta: pageMeta{CurrentPage: 1, TotalPages: 1},
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/firewalls/fw_1":
			decodeJSON(t, r, &patchBody)
			writeJSON(t, w, http.StatusOK, firewallResponse{Data: Firewall{ID: "fw_1"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer api.Close()

	if _, err := testClient(api).EnsureFirewall(context.Background(), "demo", "production", config.LatitudeConfig{
		Project:         "proj-demo",
		SSHAllowedCIDRs: []string{"203.0.113.0/24"},
	}); err != nil {
		t.Fatal(err)
	}
	if patchBody == nil {
		t.Fatal("expected firewall update")
	}
	rules := patchBody["data"].(map[string]any)["attributes"].(map[string]any)["rules"].([]any)
	if len(rules) != 4 {
		t.Fatalf("rules = %+v", rules)
	}
}

func TestEnsureFirewallAssignmentsSkipsExistingServers(t *testing.T) {
	var assigned []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/firewalls/fw_1/assignments":
			writeJSON(t, w, http.StatusOK, firewallAssignmentsResponse{
				Data: []FirewallAssignment{{
					ID: "fwasg_1",
					Attributes: FirewallAssignmentAttributes{
						Server:     FirewallAssignmentServer{ID: "sv_existing"},
						FirewallID: "fw_1",
					},
				}},
				Meta: pageMeta{CurrentPage: 1, TotalPages: 1},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/firewalls/fw_1/assignments":
			var body map[string]any
			decodeJSON(t, r, &body)
			attrs := body["data"].(map[string]any)["attributes"].(map[string]any)
			assigned = append(assigned, attrs["server_id"].(string))
			writeJSON(t, w, http.StatusCreated, firewallAssignmentResponse{Data: FirewallAssignment{ID: "fwasg_2"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer api.Close()

	err := testClient(api).EnsureFirewallAssignments(context.Background(), "fw_1", []provider.Host{
		{ID: "sv_existing"},
		{ID: "sv_new"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(assigned) != 1 || assigned[0] != "sv_new" {
		t.Fatalf("assigned = %+v", assigned)
	}
}

func TestReconcileAssignsManagedFirewallToExistingHosts(t *testing.T) {
	var assigned []string
	ownershipTags := []LatitudeTag{
		tag("tag-managed", tagNameForLabel(LabelManagedBy, "ship")),
		tag("tag-project", tagNameForLabel(LabelProject, "demo")),
		tag("tag-env", tagNameForLabel(LabelEnvironment, "production")),
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/tags":
			writeJSON(t, w, http.StatusOK, tagsResponse{Data: ownershipTags})
		case r.Method == http.MethodGet && r.URL.Path == "/firewalls":
			writeJSON(t, w, http.StatusOK, firewallsResponse{
				Data: []Firewall{{
					ID: "fw_1",
					Attributes: FirewallAttributes{
						Name:  "ship-demo-production-firewall",
						Rules: firewallRules(config.LatitudeConfig{SSHAllowedCIDRs: []string{"203.0.113.0/24"}}),
					},
				}},
				Meta: pageMeta{CurrentPage: 1, TotalPages: 1},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/servers":
			writeJSON(t, w, http.StatusOK, serversResponse{
				Data: []Server{{
					ID: "sv_1",
					Attributes: ServerAttributes{
						Hostname:    "ship-demo-production-web-1",
						PrimaryIPv4: "198.51.100.106",
						Tags:        ownershipTags,
					},
				}},
				Meta: pageMeta{CurrentPage: 1, TotalPages: 1},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/firewalls/fw_1/assignments":
			writeJSON(t, w, http.StatusOK, firewallAssignmentsResponse{Meta: pageMeta{CurrentPage: 1, TotalPages: 1}})
		case r.Method == http.MethodPost && r.URL.Path == "/firewalls/fw_1/assignments":
			var body map[string]any
			decodeJSON(t, r, &body)
			attrs := body["data"].(map[string]any)["attributes"].(map[string]any)
			assigned = append(assigned, attrs["server_id"].(string))
			writeJSON(t, w, http.StatusCreated, firewallAssignmentResponse{Data: FirewallAssignment{ID: "fwasg_1"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer api.Close()

	env := config.Environment{
		Provider: config.ProviderConfig{Latitude: &config.LatitudeConfig{
			Project:         "proj-demo",
			Site:            "ASH",
			Plan:            "c2-small-x86",
			OperatingSystem: "ubuntu_24_04_x64_lts",
			SSHKeys:         []string{"ssh-key-1"},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{"web": {Count: 1}}},
	}
	result, err := testClient(api).Reconcile(context.Background(), "demo", "production", env)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Existing) != 1 || len(assigned) != 1 || assigned[0] != "sv_1" {
		t.Fatalf("result = %+v assigned=%+v", result, assigned)
	}
}

func TestEnsureUserDataReusesExistingOrCreatesBase64Content(t *testing.T) {
	var created map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/user_data":
			writeJSON(t, w, http.StatusOK, userDataListResponse{Data: []UserData{{
				ID: "ud_existing",
				Attributes: UserDataAttributes{
					Description: "other",
					Content:     base64.StdEncoding.EncodeToString([]byte("other")),
				},
			}}})
		case r.Method == http.MethodPost && r.URL.Path == "/user_data":
			decodeJSON(t, r, &created)
			writeJSON(t, w, http.StatusCreated, userDataResponse{Data: UserData{ID: "ud_new"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer api.Close()

	userData, err := testClient(api).EnsureUserData(context.Background(), "demo", "production", "proj-demo", "#cloud-config\n")
	if err != nil {
		t.Fatal(err)
	}
	if userData.ID != "ud_new" {
		t.Fatalf("user data = %+v", userData)
	}
	attrs := created["data"].(map[string]any)["attributes"].(map[string]any)
	if attrs["project"] != "proj-demo" {
		t.Fatalf("attrs = %+v", attrs)
	}
	if attrs["content"] != base64.StdEncoding.EncodeToString([]byte("#cloud-config\n")) {
		t.Fatalf("content = %q", attrs["content"])
	}
	if !strings.HasPrefix(attrs["description"].(string), "ship-demo-production-") {
		t.Fatalf("description = %q", attrs["description"])
	}
}

func TestDeleteServerUsesServerEndpoint(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		if r.Method != http.MethodDelete || r.URL.Path != "/servers/sv_1" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer api.Close()

	if err := testClient(api).Delete(context.Background(), provider.Host{ID: "sv_1"}); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialChecksAcceptEitherTokenName(t *testing.T) {
	checks := (Client{}).CredentialChecks(func(key string) (string, bool) {
		return "", key == "LATITUDESH_BEARER"
	})
	if len(checks) != 1 || !checks[0].Present {
		t.Fatalf("checks = %+v", checks)
	}
}

func testClient(api *httptest.Server) Client {
	return Client{Token: "token", HTTP: api.Client(), BaseURL: api.URL}
}

func assertAuth(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer token" {
		t.Fatalf("authorization = %q", got)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/vnd.api+json")
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

func tag(id, name string) LatitudeTag {
	return LatitudeTag{ID: id, Type: "tags", Attributes: TagAttributes{Name: name}}
}
