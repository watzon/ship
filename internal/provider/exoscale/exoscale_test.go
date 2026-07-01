package exoscale

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
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
	env := config.Environment{
		Provider: config.ProviderConfig{Exoscale: &config.ExoscaleConfig{
			Zone:         "ch-gva-2",
			InstanceType: "standard.medium",
			Template:     "template-base",
			UserData:     "#cloud-config\npackages: [htop]\n",
			SSHKeys:      []string{"deploy"},
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {
				Count:    1,
				Size:     "standard.large",
				Image:    "template-pool",
				UserData: "#cloud-config\npackages: [curl]\n",
				Labels:   map[string]string{"owner": "platform"},
			},
		}},
	}

	plans := DesiredInstancesFor("demo", "production", env)
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(plans))
	}
	plan := plans[0]
	if plan.Location != "ch-gva-2" || plan.Size != "standard.large" || plan.Image != "template-pool" {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.UserData != "#cloud-config\npackages: [curl]\n" {
		t.Fatalf("user data = %q", plan.UserData)
	}
	if plan.Labels["owner"] != "platform" || plan.Labels[provider.LabelProject] != "demo" {
		t.Fatalf("labels = %+v", plan.Labels)
	}
}

func TestCreateInstanceSendsSignedRichPayload(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var createBody map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := assertSigned(t, r, "secret", now)
		if r.Method != http.MethodPost || r.URL.Path != "/v2/instance" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		if err := json.Unmarshal(body, &createBody); err != nil {
			t.Fatal(err)
		}
		writeJSON(t, w, Operation{Reference: struct {
			ID      string `json:"id"`
			Link    string `json:"link"`
			Command string `json:"command"`
		}{ID: "instance-1"}})
	}))
	defer api.Close()

	enable := true
	disable := false
	client := testClient(api, now)
	instance, err := client.CreateInstance(context.Background(), provider.HostPlan{
		Project:     "demo",
		Environment: "production",
		Name:        "web-1",
		Pool:        "web",
		Location:    "ch-gva-2",
		Size:        "type-1",
		Image:       "template-1",
		UserData:    "#!/bin/sh\ntrue\n",
		Labels:      provider.ShipLabels("demo", "production", "web"),
	}, config.ExoscaleConfig{
		DiskSizeGB:                    50,
		SSHKeys:                       []string{"deploy", "ops"},
		PublicIPAssignment:            "dual",
		AntiAffinityGroups:            []string{"anti-1"},
		DeployTarget:                  "target-1",
		AutoStart:                     &disable,
		SecureBoot:                    &enable,
		TPM:                           &enable,
		ApplicationConsistentSnapshot: &enable,
	}, []string{"sg-1"})
	if err != nil {
		t.Fatal(err)
	}
	if instance.ID != "instance-1" {
		t.Fatalf("instance id = %q", instance.ID)
	}
	if createBody["name"] != "web-1" || createBody["disk-size"] != float64(50) || createBody["public-ip-assignment"] != "dual" {
		t.Fatalf("create body = %+v", createBody)
	}
	if createBody["auto-start"] != false || createBody["secureboot-enabled"] != true || createBody["tpm-enabled"] != true ||
		createBody["application-consistent-snapshot-enabled"] != true {
		t.Fatalf("feature flags = %+v", createBody)
	}
	assertRef(t, createBody["instance-type"], "type-1")
	assertRef(t, createBody["template"], "template-1")
	assertRef(t, createBody["deploy-target"], "target-1")
	assertFirstRef(t, createBody["security-groups"], "sg-1")
	assertFirstRef(t, createBody["anti-affinity-groups"], "anti-1")
	sshKeys := createBody["ssh-keys"].([]any)
	if sshKeys[0].(map[string]any)["name"] != "deploy" || sshKeys[1].(map[string]any)["name"] != "ops" {
		t.Fatalf("ssh keys = %+v", sshKeys)
	}
	decodedUserData, err := base64.StdEncoding.DecodeString(createBody["user-data"].(string))
	if err != nil || string(decodedUserData) != "#!/bin/sh\ntrue\n" {
		t.Fatalf("user-data = %q err=%v", createBody["user-data"], err)
	}
	labels := createBody["labels"].(map[string]any)
	if labels[LabelManagedBy] != "ship" || labels[LabelPool] != "web" {
		t.Fatalf("labels = %+v", labels)
	}
}

func TestListInstancesFiltersByShipLabels(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertSigned(t, r, "secret", now)
		if r.Method != http.MethodGet || r.URL.Path != "/v2/instance" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		writeJSON(t, w, listInstancesResponse{Instances: []Instance{
			{ID: "instance-1", Name: "web-1", PublicIP: "198.51.100.80", Labels: provider.ShipLabels("demo", "production", "web")},
			{ID: "instance-2", Name: "staging", PublicIP: "198.51.100.81", Labels: provider.ShipLabels("demo", "staging", "web")},
			{ID: "instance-3", Name: "unmanaged", PublicIP: "198.51.100.82", Labels: map[string]string{"owner": "ops"}},
		}})
	}))
	defer api.Close()

	hosts, err := testClient(api, now).List(context.Background(), "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].ID != "instance-1" || hosts[0].PublicAddress != "198.51.100.80" {
		t.Fatalf("hosts = %+v", hosts)
	}
}

func TestEnsureSecurityGroupCreatesHTTPHTTPSAndRestrictedSSHRules(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var rules []SecurityRule
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := assertSigned(t, r, "secret", now)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/security-group":
			writeJSON(t, w, listSecurityGroupsResponse{})
		case r.Method == http.MethodPost && r.URL.Path == "/v2/security-group":
			var groupBody map[string]string
			decodeBytes(t, body, &groupBody)
			if groupBody["name"] != "ship-demo-production" {
				t.Fatalf("security group body = %+v", groupBody)
			}
			writeJSON(t, w, Operation{Reference: struct {
				ID      string `json:"id"`
				Link    string `json:"link"`
				Command string `json:"command"`
			}{ID: "sg-1"}})
		case r.Method == http.MethodPost && r.URL.Path == "/v2/security-group/sg-1/rules":
			var rule SecurityRule
			decodeBytes(t, body, &rule)
			rules = append(rules, rule)
			writeJSON(t, w, Operation{State: "success"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer api.Close()

	group, err := testClient(api, now).EnsureSecurityGroup(context.Background(), "demo", "production", config.ExoscaleConfig{
		SSHAllowedCIDRs: []string{"203.0.113.0/24"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if group.ID != "sg-1" {
		t.Fatalf("group = %+v", group)
	}
	if len(rules) != 4 {
		t.Fatalf("rules = %+v", rules)
	}
	if rules[0].StartPort != 80 || rules[1].StartPort != 443 || rules[2].Protocol != "udp" || rules[2].StartPort != 443 || rules[3].StartPort != 22 || rules[3].Network != "203.0.113.0/24" {
		t.Fatalf("rules = %+v", rules)
	}
}

func TestDeleteInstanceUsesInstanceEndpoint(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertSigned(t, r, "secret", now)
		if r.Method != http.MethodDelete || r.URL.Path != "/v2/instance/instance-1" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		writeJSON(t, w, Operation{State: "success"})
	}))
	defer api.Close()

	if err := testClient(api, now).Delete(context.Background(), provider.Host{ID: "instance-1"}); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialChecksRequireKeyAndSecret(t *testing.T) {
	checks := (Client{}).CredentialChecks(func(key string) (string, bool) {
		return "", key == "EXOSCALE_API_KEY"
	})
	if len(checks) != 1 {
		t.Fatalf("checks = %+v", checks)
	}
	if checks[0].Present {
		t.Fatal("expected missing credentials")
	}
	if !strings.Contains(checks[0].MissingMessage, "EXOSCALE_API_KEY/EXOSCALE_API_SECRET") {
		t.Fatalf("missing message = %q", checks[0].MissingMessage)
	}
}

func testClient(api *httptest.Server, now time.Time) Client {
	return Client{
		APIKey:    "key",
		APISecret: "secret",
		Zone:      "ch-gva-2",
		HTTP:      api.Client(),
		BaseURL:   api.URL + "/v2",
		Now:       func() time.Time { return now },
	}
}

func assertSigned(t *testing.T, r *http.Request, secret string, now time.Time) []byte {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	expires := now.Add(5 * time.Minute).Unix()
	message := strings.Join([]string{
		r.Method + " " + r.URL.EscapedPath(),
		string(body),
		"",
		"",
		fmt.Sprintf("%d", expires),
	}, "\n")
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(message))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	want := fmt.Sprintf("EXO2-HMAC-SHA256 credential=key,expires=%d,signature=%s", expires, signature)
	if got := r.Header.Get("Authorization"); got != want {
		t.Fatalf("authorization = %q, want %q", got, want)
	}
	return body
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

func decodeBytes(t *testing.T, data []byte, out any) {
	t.Helper()
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatal(err)
	}
}

func assertRef(t *testing.T, value any, want string) {
	t.Helper()
	ref := value.(map[string]any)
	if ref["id"] != want {
		t.Fatalf("ref = %+v, want id %q", ref, want)
	}
}

func assertFirstRef(t *testing.T, value any, want string) {
	t.Helper()
	refs := value.([]any)
	if refs[0].(map[string]any)["id"] != want {
		t.Fatalf("refs = %+v, want first id %q", refs, want)
	}
}
