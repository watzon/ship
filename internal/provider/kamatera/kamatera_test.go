package kamatera

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
		Provider: config.ProviderConfig{Kamatera: &config.KamateraConfig{
			Datacenter: "US-NY2",
			CPU:        "2B",
			RAMMB:      4096,
			Image:      "US-NY2:ubuntu_server_24.04_64-bit",
			DiskGB:     40,
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {
				Count:    1,
				Location: "IL",
				Size:     "4B",
				Image:    "IL:ubuntu_server_24.04_64-bit",
				Labels:   map[string]string{"tier": "edge"},
			},
		}},
	}

	plans := DesiredServersFor("demo", "production", env)
	if len(plans) != 1 {
		t.Fatalf("plans = %+v", plans)
	}
	plan := plans[0]
	if plan.Location != "IL" || plan.Size != "4B" || plan.Image != "IL:ubuntu_server_24.04_64-bit" {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.Labels[provider.LabelProject] != "demo" || plan.Labels["tier"] != "edge" {
		t.Fatalf("labels = %+v", plan.Labels)
	}
}

func TestCreateServerSendsKamateraPayloadAndWaitsForServer(t *testing.T) {
	var createBody map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/service/server":
			decodeJSON(t, r, &createBody)
			writeJSON(t, w, http.StatusOK, []int{17635771})
		case r.Method == http.MethodGet && r.URL.Path == "/service/servers":
			writeJSON(t, w, http.StatusOK, []ServerSummary{{
				ID:         "server-1",
				Datacenter: "US-NY2",
				Name:       "ship-demo-production-web-1",
				Power:      "on",
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/service/server/server-1":
			writeJSON(t, w, http.StatusOK, Server{
				ID:         "server-1",
				Datacenter: "US-NY2",
				Name:       "ship-demo-production-web-1",
				Networks:   []Network{{Network: "wan-us", IPs: []string{"2001:db8::1", "198.51.100.10"}}},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer api.Close()

	backup := true
	power := true
	client := testClient(api)
	server, err := client.CreateServer(context.Background(), provider.HostPlan{
		Project:     "demo",
		Environment: "production",
		Name:        "web-1",
		Pool:        "web",
		Location:    "US-NY2",
		Size:        "2B",
		Image:       "US-NY2:ubuntu_server_24.04_64-bit",
	}, config.KamateraConfig{
		RAMMB:        4096,
		DiskGB:       40,
		Billing:      "hourly",
		Traffic:      "t5000",
		Network:      "wan",
		NetworkIP:    "198.51.100.50",
		NetworkBits:  24,
		SSHPublicKey: "ssh-ed25519 AAAA...",
		Backup:       &backup,
		Power:        &power,
	})
	if err != nil {
		t.Fatal(err)
	}
	if server.ID != "server-1" || publicAddress(server.Networks) != "198.51.100.10" {
		t.Fatalf("server = %+v", server)
	}
	if createBody["disk_src_0"] != "US-NY2:ubuntu_server_24.04_64-bit" ||
		createBody["datacenter"] != "US-NY2" ||
		createBody["name"] != "ship-demo-production-web-1" ||
		createBody["cpu"] != "2B" ||
		createBody["ram"] != float64(4096) ||
		createBody["password"] != "ServerPassword1" ||
		createBody["billing"] != "hourly" ||
		createBody["traffic"] != "t5000" ||
		createBody["disk_size_0"] != float64(40) ||
		createBody["network_name_0"] != "wan" ||
		createBody["network_ip_0"] != "198.51.100.50" ||
		createBody["network_bits_0"] != float64(24) ||
		createBody["selectedSSHKeyValue"] != "ssh-ed25519 AAAA..." ||
		createBody["backup"] != true ||
		createBody["power"] != true {
		t.Fatalf("create body = %+v", createBody)
	}
}

func TestListServersFiltersShipPrefixAndReturnsLogicalHosts(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/service/servers":
			writeJSON(t, w, http.StatusOK, []ServerSummary{
				{ID: "server-1", Name: "ship-demo-production-web-1", Datacenter: "US-NY2"},
				{ID: "server-2", Name: "ship-demo-staging-web-1", Datacenter: "US-NY2"},
				{ID: "server-3", Name: "unmanaged", Datacenter: "US-NY2"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/service/server/server-1":
			writeJSON(t, w, http.StatusOK, Server{
				ID:       "server-1",
				Name:     "ship-demo-production-web-1",
				Networks: []Network{{Network: "private", IPs: []string{"10.0.0.10"}}, {Network: "wan-us", IPs: []string{"198.51.100.11"}}},
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
	if len(hosts) != 1 || hosts[0].ID != "server-1" || hosts[0].Name != "web-1" || hosts[0].Pool != "web" || hosts[0].PublicAddress != "198.51.100.11" {
		t.Fatalf("hosts = %+v", hosts)
	}
}

func TestDeleteServerTerminatesWithConfirmAndForce(t *testing.T) {
	var body map[string]string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		if r.Method != http.MethodDelete || r.URL.Path != "/service/server/server-1/terminate" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		decodeJSON(t, r, &body)
		writeJSON(t, w, http.StatusOK, 17643931)
	}))
	defer api.Close()

	if err := testClient(api).Delete(context.Background(), provider.Host{ID: "server-1"}); err != nil {
		t.Fatal(err)
	}
	if body["confirm"] != "1" || body["force"] != "1" {
		t.Fatalf("delete body = %+v", body)
	}
}

func TestCredentialChecksRequireKamateraCredentialsAndPassword(t *testing.T) {
	checks := (Client{PasswordEnv: "SHIP_KAMATERA_PASSWORD"}).CredentialChecks(func(string) (string, bool) {
		return "", false
	})
	if len(checks) != 3 {
		t.Fatalf("checks = %+v", checks)
	}
	if checks[0].Present || !strings.Contains(checks[0].MissingMessage, "KAMATERA_CLIENT_ID") {
		t.Fatalf("client id check = %+v", checks[0])
	}
	if checks[1].Present || !strings.Contains(checks[1].MissingMessage, "KAMATERA_SECRET") {
		t.Fatalf("secret check = %+v", checks[1])
	}
	if checks[2].Present || !strings.Contains(checks[2].MissingMessage, "SHIP_KAMATERA_PASSWORD") {
		t.Fatalf("password check = %+v", checks[2])
	}
}

func testClient(api *httptest.Server) Client {
	return Client{
		ClientID:       "client",
		Secret:         "secret",
		ServerPassword: "ServerPassword1",
		PasswordEnv:    defaultPasswordEnv,
		HTTP:           api.Client(),
		BaseURL:        api.URL + "/service",
	}
}

func assertAuth(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Header.Get("clientId") != "client" {
		t.Fatalf("clientId = %q", r.Header.Get("clientId"))
	}
	if r.Header.Get("secret") != "secret" {
		t.Fatalf("secret = %q", r.Header.Get("secret"))
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
