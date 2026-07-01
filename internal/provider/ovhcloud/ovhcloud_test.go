package ovhcloud

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

var _ provider.Provider = Client{}

func TestDesiredServersForUsesProviderDefaultsAndPoolOverrides(t *testing.T) {
	env := config.Environment{
		Provider: config.ProviderConfig{OVHCloud: &config.OVHCloudConfig{
			ServiceName: "project-id",
			Region:      "GRA11",
			FlavorID:    "b2-7",
			ImageID:     "image-id",
			SSHKeyID:    "key-id",
			UserData:    "#cloud-config\npackages: [htop]\n",
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {
				Count:    1,
				Size:     "b3-8",
				Image:    "pool-image",
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
	if plan.Location != "GRA11" || plan.Size != "b3-8" || plan.Image != "pool-image" {
		t.Fatalf("plan shape = %+v", plan)
	}
	if plan.UserData != "#cloud-config\npackages: [curl]\n" {
		t.Fatalf("user data = %q", plan.UserData)
	}
	if plan.Labels["owner"] != "platform" || plan.Labels[provider.LabelProject] != "demo" {
		t.Fatalf("labels = %+v", plan.Labels)
	}
}

func TestCreateInstanceSendsOVHShapeAndSignature(t *testing.T) {
	var createBody map[string]any
	var authHeader string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertOVHHeaders(t, r)
		authHeader = r.Header.Get("X-Ovh-Signature")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/cloud/project/project-id/instance":
			decodeJSON(t, r, &createBody)
			writeJSON(t, w, Instance{
				ID:     "instance-1",
				Name:   "ship-demo-production-web-1",
				Region: "GRA11",
				IPAddresses: []IPAddress{
					{IP: "198.51.100.60", Version: 4, Type: "public"},
				},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()

	monthly := true
	client := Client{
		ApplicationKey:    "app-key",
		ApplicationSecret: "app-secret",
		ConsumerKey:       "consumer-key",
		ServiceName:       "project-id",
		BaseURL:           api.URL,
		HTTP:              api.Client(),
		Now:               func() time.Time { return time.Unix(1782880000, 0) },
	}
	instance, err := client.CreateInstance(context.Background(), provider.HostPlan{
		Project:     "demo",
		Environment: "production",
		Name:        "web-1",
		Pool:        "web",
		Location:    "GRA11",
		Size:        "b2-7",
		Image:       "image-id",
		UserData:    "#!/bin/sh\ntrue\n",
	}, config.OVHCloudConfig{
		SSHKeyID:       "key-id",
		MonthlyBilling: &monthly,
	})
	if err != nil {
		t.Fatal(err)
	}
	if instance.ID != "instance-1" {
		t.Fatalf("instance = %+v", instance)
	}
	if createBody["name"] != "ship-demo-production-web-1" ||
		createBody["region"] != "GRA11" ||
		createBody["flavorId"] != "b2-7" ||
		createBody["imageId"] != "image-id" ||
		createBody["sshKeyId"] != "key-id" ||
		createBody["monthlyBilling"] != true ||
		createBody["userData"] == "" {
		t.Fatalf("create body = %+v", createBody)
	}
	bodyBytes, _ := json.Marshal(createBody)
	wantSig := ovhSignature("app-secret", "consumer-key", http.MethodPost, api.URL+"/cloud/project/project-id/instance", string(bodyBytes), "1782880000")
	if authHeader != wantSig {
		t.Fatalf("signature = %q, want %q", authHeader, wantSig)
	}
}

func TestListInstancesFiltersByShipNamePrefixAndHydratesHosts(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertOVHHeaders(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/cloud/project/project-id/instance":
			writeJSON(t, w, []Instance{
				{ID: "instance-1", Name: "ship-demo-production-web-1", Region: "GRA11", IPAddresses: []IPAddress{{IP: "198.51.100.61", Version: 4, Type: "public"}}},
				{ID: "instance-2", Name: "ship-demo-staging-web-1", Region: "GRA11"},
				{ID: "instance-3", Name: "manual-box", Region: "GRA11"},
				{ID: "instance-4", Name: "ship-demo-production-web-2", Region: "BHS5"},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()

	client := Client{
		ApplicationKey:    "app-key",
		ApplicationSecret: "app-secret",
		ConsumerKey:       "consumer-key",
		ServiceName:       "project-id",
		Region:            "GRA11",
		BaseURL:           api.URL,
		HTTP:              api.Client(),
		Now:               func() time.Time { return time.Unix(1782880000, 0) },
	}
	hosts, err := client.List(context.Background(), "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].Name != "web-1" || hosts[0].PublicAddress != "198.51.100.61" {
		t.Fatalf("hosts = %+v", hosts)
	}
}

func TestDeleteInstance(t *testing.T) {
	var path string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertOVHHeaders(t, r)
		path = r.Method + " " + r.URL.Path
		if r.Method != http.MethodDelete || r.URL.Path != "/cloud/project/project-id/instance/instance-1" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer api.Close()

	client := Client{
		ApplicationKey:    "app-key",
		ApplicationSecret: "app-secret",
		ConsumerKey:       "consumer-key",
		ServiceName:       "project-id",
		BaseURL:           api.URL,
		HTTP:              api.Client(),
		Now:               func() time.Time { return time.Unix(1782880000, 0) },
	}
	if err := client.Delete(context.Background(), provider.Host{ID: "instance-1"}); err != nil {
		t.Fatal(err)
	}
	if path != "DELETE /cloud/project/project-id/instance/instance-1" {
		t.Fatalf("path = %q", path)
	}
}

func TestCredentialChecks(t *testing.T) {
	checks := Client{}.CredentialChecks(func(key string) (string, bool) {
		switch key {
		case "OVH_APPLICATION_KEY", "OVH_APPLICATION_SECRET", "OVH_CONSUMER_KEY":
			return "value", true
		default:
			return "", false
		}
	})
	if len(checks) != 1 || !checks[0].Present {
		t.Fatalf("checks = %+v", checks)
	}
}

func TestEndpointBaseMapsOVHAliases(t *testing.T) {
	cases := map[string]string{
		"":                       defaultAPIBase,
		"ovh-eu":                 defaultAPIBase,
		"ovh-us":                 "https://api.us.ovhcloud.com/1.0",
		"ovh-ca":                 "https://ca.api.ovh.com/1.0",
		"https://example.test/":  "https://example.test",
		"https://example.test/v": "https://example.test/v",
	}
	for input, want := range cases {
		if got := endpointBase(input); got != want {
			t.Fatalf("endpointBase(%q) = %q, want %q", input, got, want)
		}
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

func assertOVHHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	for _, header := range []string{"X-Ovh-Application", "X-Ovh-Consumer", "X-Ovh-Timestamp", "X-Ovh-Signature"} {
		if strings.TrimSpace(r.Header.Get(header)) == "" {
			t.Fatalf("missing %s", header)
		}
	}
}

func ovhSignature(secret, consumer, method, fullURL, body, timestamp string) string {
	value := secret + "+" + consumer + "+" + strings.ToUpper(method) + "+" + fullURL + "+" + body + "+" + timestamp
	sum := sha1.Sum([]byte(value))
	return "$1$" + hex.EncodeToString(sum[:])
}
