package proxmox

import (
	"context"
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

func TestCreateClonesConfiguresStartsAndDiscoversAddress(t *testing.T) {
	var requests []string
	var cloneForm, configForm url.Values
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "PVEAPIToken=root@pam!ship=secret" {
			t.Fatalf("authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/cluster/nextid":
			writeData(t, w, 101)
		case r.Method == http.MethodPost && r.URL.Path == "/api2/json/nodes/pve1/qemu/9000/clone":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			cloneForm = r.PostForm
			writeData(t, w, "UPID:pve1:clone")
		case r.Method == http.MethodPost && r.URL.Path == "/api2/json/nodes/pve1/qemu/101/config":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			configForm = r.PostForm
			writeData(t, w, "UPID:pve1:config")
		case r.Method == http.MethodPost && r.URL.Path == "/api2/json/nodes/pve1/qemu/101/status/start":
			writeData(t, w, "UPID:pve1:start")
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api2/json/nodes/pve1/tasks/"):
			writeData(t, w, TaskStatus{Status: "stopped", ExitStatus: "OK"})
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve1/qemu/101/agent/network-get-interfaces":
			writeData(t, w, map[string]any{"result": []map[string]any{{
				"name": "eth0",
				"ip-addresses": []map[string]any{
					{"ip-address": "127.0.0.1", "ip-address-type": "ipv4"},
					{"ip-address": "198.51.100.10", "ip-address-type": "ipv4"},
				},
			}}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer api.Close()

	client := testClient(api.URL)
	result, err := client.Create(context.Background(), provider.HostPlan{
		Project:     "demo",
		Environment: "production",
		Name:        "web-1",
		Pool:        "web",
		User:        "deploy",
		Labels:      provider.ShipLabels("demo", "production", "web"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "101" || result.Name != "web-1" || result.PublicAddress != "198.51.100.10" {
		t.Fatalf("result = %+v", result)
	}
	if cloneForm.Get("newid") != "101" || cloneForm.Get("name") != "web-1" || cloneForm.Get("full") != "1" || cloneForm.Get("storage") != "local-zfs" {
		t.Fatalf("clone form = %+v", cloneForm)
	}
	if configForm.Get("ciuser") != "deploy" || configForm.Get("sshkeys") != "ssh-ed25519 AAAA..." || configForm.Get("ipconfig0") != "ip=dhcp" {
		t.Fatalf("config form = %+v", configForm)
	}
	if configForm.Get("memory") != "2048" || configForm.Get("cores") != "2" || configForm.Get("net0") != "virtio,bridge=vmbr0,tag=30" || configForm.Get("agent") != "enabled=1" {
		t.Fatalf("config form = %+v", configForm)
	}
	for _, tag := range []string{"ship", "ship-project-demo", "ship-env-production", "ship-pool-web", "gpu"} {
		if !strings.Contains(configForm.Get("tags"), tag) {
			t.Fatalf("tags missing %q: %s", tag, configForm.Get("tags"))
		}
	}
	want := []string{
		"GET /api2/json/cluster/nextid",
		"POST /api2/json/nodes/pve1/qemu/9000/clone",
		"GET /api2/json/nodes/pve1/tasks/UPID:pve1:clone/status",
		"POST /api2/json/nodes/pve1/qemu/101/config",
		"GET /api2/json/nodes/pve1/tasks/UPID:pve1:config/status",
		"POST /api2/json/nodes/pve1/qemu/101/status/start",
		"GET /api2/json/nodes/pve1/tasks/UPID:pve1:start/status",
		"GET /api2/json/nodes/pve1/qemu/101/agent/network-get-interfaces",
	}
	if strings.Join(requests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestListFiltersShipTagsAndUsesGuestAgentAddress(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve1/qemu":
			writeData(t, w, []VM{
				{VMID: 101, Name: "web-1", Tags: "ship;ship-project-demo;ship-env-production;ship-pool-web"},
				{VMID: 102, Name: "other", Tags: "ship;ship-project-demo;ship-env-staging;ship-pool-web"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve1/qemu/101/agent/network-get-interfaces":
			writeData(t, w, map[string]any{"result": []map[string]any{{
				"name": "eth0",
				"ip-addresses": []map[string]any{
					{"ip-address": "203.0.113.10", "ip-address-type": "ipv4"},
				},
			}}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer api.Close()

	hosts, err := testClient(api.URL).List(context.Background(), "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].ID != "101" || hosts[0].Name != "web-1" || hosts[0].Pool != "web" || hosts[0].PublicAddress != "203.0.113.10" {
		t.Fatalf("hosts = %+v", hosts)
	}
}

func TestDeleteDestroysVMAndWaitsForTask(t *testing.T) {
	var deletedQuery string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/api2/json/nodes/pve1/qemu/101":
			deletedQuery = r.URL.RawQuery
			writeData(t, w, "UPID:pve1:delete")
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve1/tasks/UPID:pve1:delete/status":
			writeData(t, w, TaskStatus{Status: "stopped", ExitStatus: "OK"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer api.Close()

	err := testClient(api.URL).Delete(context.Background(), provider.Host{ID: "101", Name: "web-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(deletedQuery, "purge=1") || !strings.Contains(deletedQuery, "destroy-unreferenced-disks=1") {
		t.Fatalf("delete query = %q", deletedQuery)
	}
}

func TestCredentialChecksRequireToken(t *testing.T) {
	checks := (Client{}).CredentialChecks(func(string) (string, bool) { return "", false })
	if len(checks) != 1 || !checks[0].Required || checks[0].Present {
		t.Fatalf("checks = %+v", checks)
	}
	checks = (Client{}).CredentialChecks(func(key string) (string, bool) {
		return "root@pam!ship=secret", key == "PVE_API_TOKEN"
	})
	if !checks[0].Present {
		t.Fatalf("checks = %+v", checks)
	}
}

func testClient(baseURL string) Client {
	return Client{
		Token:   "root@pam!ship=secret",
		BaseURL: baseURL,
		HTTP:    http.DefaultClient,
		Config: config.ProxmoxConfig{
			APIURL:     baseURL,
			Node:       "pve1",
			TemplateID: 9000,
			Storage:    "local-zfs",
			Bridge:     "vmbr0",
			VLAN:       30,
			MemoryMB:   2048,
			Cores:      2,
			SSHKeys:    []string{"ssh-ed25519 AAAA..."},
			Tags:       []string{"gpu"},
		},
	}
}

func writeData(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		t.Fatal(err)
	}
}
