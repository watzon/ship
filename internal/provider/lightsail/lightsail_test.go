package lightsail

import (
	"context"
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

func TestDesiredInstancesForUsesProviderDefaultsAndPoolOverrides(t *testing.T) {
	env := testEnvironment(1)
	env.Hosts.Pools["web"] = config.Pool{
		Count:    1,
		Location: "us-east-1b",
		Size:     "small_3_0",
		Image:    "ubuntu_24_04",
		UserData: "#cloud-config\npackages: [curl]\n",
	}

	plans := DesiredInstancesFor("demo", "production", env)
	if len(plans) != 1 {
		t.Fatalf("plans = %+v", plans)
	}
	plan := plans[0]
	if plan.Location != "us-east-1b" || plan.Size != "small_3_0" || plan.Image != "ubuntu_24_04" {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.UserData != "#cloud-config\npackages: [curl]\n" {
		t.Fatalf("user data = %q", plan.UserData)
	}
	if plan.Labels[provider.LabelProject] != "demo" || plan.Labels[provider.LabelPool] != "web" {
		t.Fatalf("labels = %+v", plan.Labels)
	}
}

func TestCreateInstanceSendsSignedJSONPayloadAndWaitsForAddress(t *testing.T) {
	var createBody map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertSignedLightsailRequest(t, r)
		switch r.Header.Get("X-Amz-Target") {
		case apiTargetPrefix + "CreateInstances":
			decodeJSON(t, r, &createBody)
			writeJSON(t, w, map[string]any{"operations": []Operation{{ResourceName: "web-1", Status: "Started"}}})
		case apiTargetPrefix + "GetInstances":
			writeJSON(t, w, getInstancesResponse{Instances: []Instance{{
				Name:            "web-1",
				PublicIPAddress: "198.51.100.20",
				Tags:            tagsForPlan(testPlan()),
			}}})
		default:
			t.Fatalf("target = %q", r.Header.Get("X-Amz-Target"))
		}
	}))
	defer api.Close()

	client := testClient(api)
	instance, err := client.CreateInstance(context.Background(), testPlan(), lightsailConfig())
	if err != nil {
		t.Fatal(err)
	}
	if instance.PublicIPAddress != "198.51.100.20" {
		t.Fatalf("instance = %+v", instance)
	}
	if createBody["availabilityZone"] != "us-east-1a" ||
		createBody["blueprintId"] != "ubuntu_24_04" ||
		createBody["bundleId"] != "nano_3_0" ||
		createBody["keyPairName"] != "ship-key" ||
		createBody["ipAddressType"] != "dualstack" ||
		createBody["userData"] != "#cloud-config\npackages: [htop]\n" {
		t.Fatalf("create body = %+v", createBody)
	}
	names := createBody["instanceNames"].([]any)
	if len(names) != 1 || names[0] != "web-1" {
		t.Fatalf("instance names = %+v", names)
	}
	tags := tagsMap(createBody["tags"].([]any))
	if tags[LabelManagedBy] != "ship" || tags[LabelPool] != "web" {
		t.Fatalf("tags = %+v", tags)
	}
	addOns := createBody["addOns"].([]any)
	if len(addOns) != 1 || addOns[0].(map[string]any)["addOnType"] != "AutoSnapshot" {
		t.Fatalf("addOns = %+v", addOns)
	}
}

func TestListInstancesPaginatesAndFiltersShipTags(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertSignedLightsailRequest(t, r)
		if r.Header.Get("X-Amz-Target") != apiTargetPrefix+"GetInstances" {
			t.Fatalf("target = %q", r.Header.Get("X-Amz-Target"))
		}
		var body map[string]string
		decodeJSON(t, r, &body)
		if body["pageToken"] == "" {
			writeJSON(t, w, getInstancesResponse{
				Instances: []Instance{
					{Name: "other", Tags: []Tag{{Key: LabelManagedBy, Value: "else"}}},
					{Name: "web-1", PublicIPAddress: "198.51.100.21", Tags: labels("demo", "production", "web")},
				},
				NextPageToken: "next",
			})
			return
		}
		writeJSON(t, w, getInstancesResponse{Instances: []Instance{
			{Name: "staging", Tags: labels("demo", "staging", "web")},
			{Name: "worker-1", IPv6Addresses: []string{"2001:db8::21"}, Tags: labels("demo", "production", "worker")},
		}})
	}))
	defer api.Close()

	hosts, err := testClient(api).List(context.Background(), "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 {
		t.Fatalf("hosts = %+v", hosts)
	}
	if hosts[0].Name != "web-1" || hosts[0].PublicAddress != "198.51.100.21" {
		t.Fatalf("host[0] = %+v", hosts[0])
	}
	if hosts[1].Name != "worker-1" || hosts[1].PublicAddress != "2001:db8::21" {
		t.Fatalf("host[1] = %+v", hosts[1])
	}
}

func TestReconcileCreatesAndAppliesManagedPorts(t *testing.T) {
	var portBody map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertSignedLightsailRequest(t, r)
		switch r.Header.Get("X-Amz-Target") {
		case apiTargetPrefix + "GetInstances":
			writeJSON(t, w, getInstancesResponse{Instances: []Instance{{
				Name:            "web-1",
				PublicIPAddress: "198.51.100.22",
				Tags:            labels("demo", "production", "web"),
			}}})
		case apiTargetPrefix + "CreateInstances":
			writeJSON(t, w, map[string]any{"operations": []Operation{{ResourceName: "web-1", Status: "Started"}}})
		case apiTargetPrefix + "PutInstancePublicPorts":
			decodeJSON(t, r, &portBody)
			writeJSON(t, w, map[string]any{"operation": Operation{ResourceName: "web-1", Status: "Succeeded"}})
		default:
			t.Fatalf("target = %q", r.Header.Get("X-Amz-Target"))
		}
	}))
	defer api.Close()

	result, err := testClient(api).Reconcile(context.Background(), "demo", "production", testEnvironment(1))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Existing) != 1 || result.Existing[0].PublicAddress != "198.51.100.22" {
		t.Fatalf("result = %+v", result)
	}
	if portBody["instanceName"] != "web-1" {
		t.Fatalf("port body = %+v", portBody)
	}
	ports := portBody["portInfos"].([]any)
	if !hasPort(ports, 22, "203.0.113.0/24") || !hasPort(ports, 80, "0.0.0.0/0") || !hasPort(ports, 443, "::/0") {
		t.Fatalf("ports = %+v", ports)
	}
}

func TestDeleteInstanceUsesNameAndForceDeleteAddOns(t *testing.T) {
	var body map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertSignedLightsailRequest(t, r)
		if r.Header.Get("X-Amz-Target") != apiTargetPrefix+"DeleteInstance" {
			t.Fatalf("target = %q", r.Header.Get("X-Amz-Target"))
		}
		decodeJSON(t, r, &body)
		writeJSON(t, w, map[string]any{"operation": Operation{ResourceName: "web-1", Status: "Succeeded"}})
	}))
	defer api.Close()

	force := false
	client := testClient(api)
	client.ForceDeleteAddOns = &force
	if err := client.Delete(context.Background(), provider.Host{ID: "web-1"}); err != nil {
		t.Fatal(err)
	}
	if body["instanceName"] != "web-1" || body["forceDeleteAddOns"] != false {
		t.Fatalf("delete body = %+v", body)
	}
}

func TestCredentialChecks(t *testing.T) {
	checks := (Client{}).CredentialChecks(func(key string) (string, bool) {
		return "", key == "AWS_ACCESS_KEY_ID"
	})
	if len(checks) != 2 || !checks[0].Present || checks[1].Present {
		t.Fatalf("checks = %+v", checks)
	}
}

func testClient(api *httptest.Server) Client {
	return Client{
		AccessKeyID:     "AKID",
		SecretAccessKey: "SECRET",
		SessionToken:    "TOKEN",
		Region:          "us-east-1",
		HTTP:            api.Client(),
		BaseURL:         api.URL,
		Now: func() time.Time {
			return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
		},
		Sleep: func(context.Context, time.Duration) error {
			return nil
		},
	}
}

func testEnvironment(count int) config.Environment {
	return config.Environment{
		Provider: config.ProviderConfig{Lightsail: &config.LightsailConfig{
			Region:           "us-east-1",
			AvailabilityZone: "us-east-1a",
			BundleID:         "nano_3_0",
			BlueprintID:      "ubuntu_24_04",
			KeyPairName:      "ship-key",
			UserData:         "#cloud-config\npackages: [htop]\n",
			IPAddressType:    "dualstack",
			AddOns: []config.LightsailAddOn{{
				Type:              "AutoSnapshot",
				SnapshotTimeOfDay: "06:00",
			}},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {Count: count},
		}},
	}
}

func lightsailConfig() config.LightsailConfig {
	return *testEnvironment(1).Provider.Lightsail
}

func testPlan() provider.HostPlan {
	return DesiredInstancesFor("demo", "production", testEnvironment(1))[0]
}

func labels(project, environment, pool string) []Tag {
	out := make([]Tag, 0, 4)
	for key, value := range provider.ShipLabels(project, environment, pool) {
		out = append(out, Tag{Key: key, Value: value})
	}
	return out
}

func assertSignedLightsailRequest(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Method != http.MethodPost || r.URL.Path != "/" {
		t.Fatalf("request = %s %s", r.Method, r.URL.String())
	}
	if r.Header.Get("Content-Type") != "application/x-amz-json-1.1" {
		t.Fatalf("content-type = %q", r.Header.Get("Content-Type"))
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKID/20260701/us-east-1/lightsail/aws4_request") {
		t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
	}
	if !strings.Contains(r.Header.Get("Authorization"), "SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date;x-amz-security-token;x-amz-target") {
		t.Fatalf("authorization signed headers = %q", r.Header.Get("Authorization"))
	}
	if r.Header.Get("X-Amz-Security-Token") != "TOKEN" {
		t.Fatalf("session token = %q", r.Header.Get("X-Amz-Security-Token"))
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
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

func tagsMap(values []any) map[string]string {
	out := map[string]string{}
	for _, raw := range values {
		item := raw.(map[string]any)
		out[item["key"].(string)] = item["value"].(string)
	}
	return out
}

func hasPort(values []any, port int, cidr string) bool {
	for _, raw := range values {
		item := raw.(map[string]any)
		if int(item["fromPort"].(float64)) != port || int(item["toPort"].(float64)) != port {
			continue
		}
		for _, key := range []string{"cidrs", "ipv6Cidrs"} {
			if list, ok := item[key].([]any); ok {
				for _, value := range list {
					if value == cidr {
						return true
					}
				}
			}
		}
	}
	return false
}
