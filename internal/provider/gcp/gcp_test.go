package gcp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

var _ provider.Provider = Client{}

func TestDesiredInstancesUsesPoolsAndProviderShape(t *testing.T) {
	instances := DesiredInstances(testEnvironment(2))
	if len(instances) != 2 {
		t.Fatalf("len = %d", len(instances))
	}
	if instances[0].Name != "web-1" || instances[0].Pool != "web" {
		t.Fatalf("instance = %+v", instances[0])
	}
	if instances[0].Location != "us-central1-a" || instances[0].Size != "e2-medium" || instances[0].Image != "family/ubuntu-2404-lts" {
		t.Fatalf("provider shape missing: %+v", instances[0])
	}
}

func TestCreateHostProvisionsReplacement(t *testing.T) {
	api := newFakeGCPAPI(t, nil)
	env := testEnvironment(1)
	plans := DesiredInstancesFor("demo", "production", env)
	if len(plans) == 0 {
		t.Fatal("no desired plans")
	}
	host, err := api.client().CreateHost(context.Background(), "demo", "production", env, plans[0])
	if err != nil {
		t.Fatal(err)
	}
	if host.PublicAddress != "203.0.113.10" {
		t.Fatalf("created host = %+v", host)
	}
	if len(api.firewalls) != 2 {
		t.Fatalf("firewalls not ensured through the shared backend: %+v", api.firewalls)
	}
	if len(api.creates) != 1 {
		t.Fatalf("creates = %+v", api.creates)
	}
}

func TestReconcileCreatesFirewallRulesAndInstance(t *testing.T) {
	api := newFakeGCPAPI(t, nil)
	result, err := api.client().Reconcile(context.Background(), "demo", "production", testEnvironment(1))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("created = %+v", result.Created)
	}
	if result.Created[0].PublicAddress != "203.0.113.10" {
		t.Fatalf("created host = %+v", result.Created[0])
	}
	if len(api.firewalls) != 2 {
		t.Fatalf("firewalls = %+v", api.firewalls)
	}
	publicFirewall := api.firewalls[0]
	if publicFirewall.Name != "ship-demo-production-firewall" ||
		strings.Join(publicFirewall.SourceRanges, ",") != "0.0.0.0/0,::/0" ||
		strings.Join(publicFirewall.Allowed[0].Ports, ",") != "80,443" {
		t.Fatalf("public firewall = %+v", publicFirewall)
	}
	sshFirewall := api.firewalls[1]
	if sshFirewall.Name != "ship-demo-production-firewall-ssh" ||
		strings.Join(sshFirewall.SourceRanges, ",") != "203.0.113.0/24" ||
		strings.Join(sshFirewall.Allowed[0].Ports, ",") != "22" {
		t.Fatalf("ssh firewall = %+v", sshFirewall)
	}
	if len(api.creates) != 1 {
		t.Fatalf("creates = %+v", api.creates)
	}
	create := api.creates[0]
	if create.Name != "web-1" || create.MachineType != "zones/us-central1-a/machineTypes/e2-medium" {
		t.Fatalf("create = %+v", create)
	}
	if create.Labels["managed-by"] != "ship" || create.Labels["project"] != "demo" || create.Labels["environment"] != "production" || create.Labels["pool"] != "web" {
		t.Fatalf("labels = %+v", create.Labels)
	}
	if !contains(create.Tags.Items, "ship-demo-production-hosts") || !contains(create.Tags.Items, "ship-web") {
		t.Fatalf("tags = %+v", create.Tags.Items)
	}
	sourceImage := create.Disks[0].InitializeParams.SourceImage
	if sourceImage != "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts" {
		t.Fatalf("source image = %q", sourceImage)
	}
	if create.Disks[0].InitializeParams.DiskSizeGB != "40" || create.Disks[0].InitializeParams.DiskType != "zones/us-central1-a/diskTypes/pd-balanced" {
		t.Fatalf("disk params = %+v", create.Disks[0].InitializeParams)
	}
	if create.Metadata.Items["user-data"] != "#cloud-config\npackages: [htop]\n" || create.Metadata.Items["enable-oslogin"] != "TRUE" {
		t.Fatalf("metadata = %+v", create.Metadata.Items)
	}
	if len(create.NetworkInterfaces) != 1 || len(create.NetworkInterfaces[0].AccessConfigs) != 1 {
		t.Fatalf("network interfaces = %+v", create.NetworkInterfaces)
	}
}

func TestListFiltersLabelsAndDeleteUsesInstanceName(t *testing.T) {
	api := newFakeGCPAPI(t, []Instance{
		{Name: "web-1", Labels: gcpLabels(provider.ShipLabels("demo", "production", "web")), NetworkInterfaces: []NetworkInterface{{AccessConfigs: []AccessConfig{{NatIP: "203.0.113.10"}}}}},
		{Name: "staging", Labels: gcpLabels(provider.ShipLabels("demo", "staging", "web"))},
		{Name: "other", Labels: map[string]string{"managed-by": "someone-else"}},
	})
	hosts, err := api.client().List(context.Background(), "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].Name != "web-1" || hosts[0].PublicAddress != "203.0.113.10" {
		t.Fatalf("hosts = %+v", hosts)
	}
	if err := api.client().Delete(context.Background(), hosts[0]); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(api.deletes, ","), "web-1"; got != want {
		t.Fatalf("deletes = %q, want %q", got, want)
	}
}

func TestCredentialChecks(t *testing.T) {
	checks := Client{}.CredentialChecks(func(key string) (string, bool) {
		return "", key == "GOOGLE_APPLICATION_CREDENTIALS"
	})
	if len(checks) != 1 || !checks[0].Present || checks[0].Name != "gcp credentials" {
		t.Fatalf("checks = %+v", checks)
	}
}

func TestBearerTokenUsesServiceAccountCredentials(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		data, err := ioReadAllForm(r)
		if err != nil {
			t.Fatal(err)
		}
		if data.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Fatalf("grant_type = %q", data.Get("grant_type"))
		}
		if parts := strings.Split(data.Get("assertion"), "."); len(parts) != 3 {
			t.Fatalf("assertion parts = %d", len(parts))
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "service-token"})
	}))
	defer tokenServer.Close()

	dir := t.TempDir()
	credentialsPath := filepath.Join(dir, "service-account.json")
	credentials := map[string]string{
		"client_email": "ship@example.iam.gserviceaccount.com",
		"private_key":  privateKey,
		"token_uri":    tokenServer.URL,
	}
	data, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialsPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	client := Client{
		CredentialsFile: credentialsPath,
		HTTP:            tokenServer.Client(),
		Now:             func() time.Time { return time.Unix(1000, 0) },
	}
	token, err := client.bearerToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "service-token" {
		t.Fatalf("token = %q", token)
	}
}

func ioReadAllForm(r *http.Request) (url.Values, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	return r.Form, nil
}

func testEnvironment(count int) config.Environment {
	return config.Environment{
		Provider: config.ProviderConfig{GCP: &config.GCPConfig{
			ProjectID:       "demo-project",
			Zone:            "us-central1-a",
			MachineType:     "e2-medium",
			Image:           "family/ubuntu-2404-lts",
			ImageProject:    "ubuntu-os-cloud",
			UserData:        "#cloud-config\npackages: [htop]\n",
			Network:         "default",
			NetworkTags:     []string{"ship-web"},
			Metadata:        map[string]string{"enable-oslogin": "TRUE"},
			ExternalIP:      boolPointer(true),
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
			BootDisk: config.GCPBootDiskConfig{
				SizeGB: 40,
				Type:   "pd-balanced",
			},
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {Count: count},
		}},
	}
}

func boolPointer(value bool) *bool {
	return &value
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type fakeGCPAPI struct {
	server    *httptest.Server
	instances []Instance
	firewalls []createFirewallRequest
	creates   []createInstanceRequest
	deletes   []string
}

type createFirewallRequest struct {
	Name         string        `json:"name"`
	Network      string        `json:"network"`
	Direction    string        `json:"direction"`
	TargetTags   []string      `json:"targetTags"`
	SourceRanges []string      `json:"sourceRanges"`
	Allowed      []allowedRule `json:"allowed"`
}

type allowedRule struct {
	IPProtocol string   `json:"IPProtocol"`
	Ports      []string `json:"ports"`
}

type createInstanceRequest struct {
	Name        string                   `json:"name"`
	MachineType string                   `json:"machineType"`
	Labels      map[string]string        `json:"labels"`
	Tags        struct{ Items []string } `json:"tags"`
	Disks       []struct {
		InitializeParams struct {
			SourceImage string `json:"sourceImage"`
			DiskSizeGB  string `json:"diskSizeGb"`
			DiskType    string `json:"diskType"`
		} `json:"initializeParams"`
	} `json:"disks"`
	Metadata struct {
		Items map[string]string
	} `json:"-"`
	RawMetadata struct {
		Items []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"items"`
	} `json:"metadata"`
	NetworkInterfaces []struct {
		AccessConfigs []map[string]string `json:"accessConfigs"`
	} `json:"networkInterfaces"`
}

func newFakeGCPAPI(t *testing.T, instances []Instance) *fakeGCPAPI {
	t.Helper()
	api := &fakeGCPAPI{instances: instances}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/projects/demo-project/global/firewalls":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []Firewall{}})
		case r.Method == http.MethodPost && r.URL.Path == "/projects/demo-project/global/firewalls":
			var req createFirewallRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			api.firewalls = append(api.firewalls, req)
			_ = json.NewEncoder(w).Encode(Firewall{ID: fmt.Sprintf("fw-%d", len(api.firewalls)), Name: req.Name})
		case r.Method == http.MethodGet && r.URL.Path == "/projects/demo-project/zones/us-central1-a/instances":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": api.instances})
		case r.Method == http.MethodPost && r.URL.Path == "/projects/demo-project/zones/us-central1-a/instances":
			var req createInstanceRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			for _, item := range req.RawMetadata.Items {
				if req.Metadata.Items == nil {
					req.Metadata.Items = map[string]string{}
				}
				req.Metadata.Items[item.Key] = item.Value
			}
			api.creates = append(api.creates, req)
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "operation-1"})
		case r.Method == http.MethodGet && r.URL.Path == "/projects/demo-project/zones/us-central1-a/instances/web-1":
			_ = json.NewEncoder(w).Encode(Instance{
				Name:   "web-1",
				Labels: gcpLabels(provider.ShipLabels("demo", "production", "web")),
				NetworkInterfaces: []NetworkInterface{{
					AccessConfigs: []AccessConfig{{NatIP: "203.0.113.10"}},
				}},
			})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/projects/demo-project/zones/us-central1-a/instances/"):
			api.deletes = append(api.deletes, strings.TrimPrefix(r.URL.Path, "/projects/demo-project/zones/us-central1-a/instances/"))
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "operation-delete"})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (api *fakeGCPAPI) client() Client {
	return Client{
		AccessToken: "test-token",
		ProjectID:   "demo-project",
		Zone:        "us-central1-a",
		HTTP:        api.server.Client(),
		BaseURL:     api.server.URL,
	}
}
