package oci

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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
		Provider: config.ProviderConfig{OCI: &config.OCIConfig{
			Region:             "us-ashburn-1",
			CompartmentID:      "ocid1.compartment.oc1..aaaa",
			AvailabilityDomain: "Uocm:US-ASHBURN-AD-1",
			Shape:              "VM.Standard.E4.Flex",
			ImageID:            "ocid1.image.oc1..image",
			SubnetID:           "ocid1.subnet.oc1..subnet",
			SSHAuthorizedKeys:  []string{"ssh-ed25519 AAAA..."},
			UserData:           "#cloud-config\npackages: [htop]\n",
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {
				Count:    1,
				Size:     "VM.Standard.A1.Flex",
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
	if plan.Location != "Uocm:US-ASHBURN-AD-1" || plan.Size != "VM.Standard.A1.Flex" || plan.Image != "ocid1.image.oc1..image" {
		t.Fatalf("plan shape = %+v", plan)
	}
	if plan.UserData != "#cloud-config\npackages: [curl]\n" {
		t.Fatalf("user data = %q", plan.UserData)
	}
	if plan.Labels["owner"] != "platform" || plan.Labels[provider.LabelProject] != "demo" {
		t.Fatalf("labels = %+v", plan.Labels)
	}
}

func TestLaunchInstanceSendsShapeMetadataTagsAndHydratesPublicIP(t *testing.T) {
	privateKey := testPrivateKey(t)
	privateKeyPEM := pemForKey(privateKey)
	var createBody map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertOCISignature(t, r, privateKey)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/20160918/instances":
			decodeJSON(t, r, &createBody)
			writeJSON(t, w, Instance{
				ID:          "ocid1.instance.oc1..instance",
				DisplayName: "web-1",
				FreeformTags: map[string]string{
					LabelManagedBy:   "ship",
					LabelProject:     "demo",
					LabelEnvironment: "production",
					LabelPool:        "web",
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/20160918/vnicAttachments":
			if r.URL.Query().Get("instanceId") != "ocid1.instance.oc1..instance" {
				t.Fatalf("vnic attachment query = %s", r.URL.RawQuery)
			}
			writeJSON(t, w, []VNICAttachment{{VNICID: "ocid1.vnic.oc1..vnic", LifecycleState: "ATTACHED"}})
		case r.Method == http.MethodGet && r.URL.Path == "/20160918/vnics/ocid1.vnic.oc1..vnic":
			writeJSON(t, w, VNIC{ID: "ocid1.vnic.oc1..vnic", PublicIP: "198.51.100.70"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer api.Close()

	assignPublicIP := false
	client := testClient(api, privateKeyPEM)
	instance, err := client.LaunchInstance(context.Background(), provider.HostPlan{
		Project:     "demo",
		Environment: "production",
		Name:        "web-1",
		Pool:        "web",
		Location:    "Uocm:US-ASHBURN-AD-1",
		Size:        "VM.Standard.E4.Flex",
		Image:       "ocid1.image.oc1..image",
		UserData:    "#!/bin/sh\ntrue\n",
		Labels:      provider.ShipLabels("demo", "production", "web"),
	}, config.OCIConfig{
		CompartmentID:     "ocid1.compartment.oc1..aaaa",
		SubnetID:          "ocid1.subnet.oc1..subnet",
		SSHAuthorizedKeys: []string{"ssh-ed25519 AAAA..."},
		AssignPublicIP:    &assignPublicIP,
		BootVolumeSizeGB:  80,
		ShapeConfig:       config.OCIShapeConfig{OCPUs: 2, MemoryGB: 16},
		Metadata:          map[string]string{"startup": "ship"},
		FreeformTags:      map[string]string{"owner": "platform"},
	}, []string{"ocid1.nsg.oc1..nsg"})
	if err != nil {
		t.Fatal(err)
	}
	if instance.PublicIP != "198.51.100.70" {
		t.Fatalf("public ip = %q", instance.PublicIP)
	}
	if createBody["availabilityDomain"] != "Uocm:US-ASHBURN-AD-1" ||
		createBody["shape"] != "VM.Standard.E4.Flex" ||
		createBody["compartmentId"] != "ocid1.compartment.oc1..aaaa" {
		t.Fatalf("create body = %+v", createBody)
	}
	source := createBody["sourceDetails"].(map[string]any)
	if source["sourceType"] != "image" || source["imageId"] != "ocid1.image.oc1..image" || int(source["bootVolumeSizeInGBs"].(float64)) != 80 {
		t.Fatalf("source = %+v", source)
	}
	vnic := createBody["createVnicDetails"].(map[string]any)
	if vnic["subnetId"] != "ocid1.subnet.oc1..subnet" || vnic["assignPublicIp"] != false {
		t.Fatalf("vnic = %+v", vnic)
	}
	if got := vnic["nsgIds"].([]any)[0]; got != "ocid1.nsg.oc1..nsg" {
		t.Fatalf("nsg ids = %+v", vnic["nsgIds"])
	}
	metadata := createBody["metadata"].(map[string]any)
	if metadata["ssh_authorized_keys"] != "ssh-ed25519 AAAA..." || metadata["startup"] != "ship" {
		t.Fatalf("metadata = %+v", metadata)
	}
	decodedUserData, err := base64.StdEncoding.DecodeString(metadata["user_data"].(string))
	if err != nil || string(decodedUserData) != "#!/bin/sh\ntrue\n" {
		t.Fatalf("user_data = %q err=%v", metadata["user_data"], err)
	}
	tags := createBody["freeformTags"].(map[string]any)
	if tags[LabelManagedBy] != "ship" || tags["owner"] != "platform" {
		t.Fatalf("tags = %+v", tags)
	}
	shapeConfig := createBody["shapeConfig"].(map[string]any)
	if shapeConfig["ocpus"] != float64(2) || shapeConfig["memoryInGBs"] != float64(16) {
		t.Fatalf("shape config = %+v", shapeConfig)
	}
}

func TestListInstancesFiltersByTagsAndHydratesPublicIP(t *testing.T) {
	privateKey := testPrivateKey(t)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertOCISignature(t, r, privateKey)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/20160918/instances":
			writeJSON(t, w, []Instance{
				{ID: "instance-1", DisplayName: "web-1", LifecycleState: "RUNNING", FreeformTags: provider.ShipLabels("demo", "production", "web")},
				{ID: "instance-2", DisplayName: "staging", LifecycleState: "RUNNING", FreeformTags: provider.ShipLabels("demo", "staging", "web")},
				{ID: "instance-3", DisplayName: "old", LifecycleState: "TERMINATED", FreeformTags: provider.ShipLabels("demo", "production", "web")},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/20160918/vnicAttachments":
			writeJSON(t, w, []VNICAttachment{{VNICID: "vnic-1", LifecycleState: "ATTACHED"}})
		case r.Method == http.MethodGet && r.URL.Path == "/20160918/vnics/vnic-1":
			writeJSON(t, w, VNIC{ID: "vnic-1", PublicIP: "198.51.100.71"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer api.Close()

	client := testClient(api, pemForKey(privateKey))
	hosts, err := client.List(context.Background(), "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].Name != "web-1" || hosts[0].PublicAddress != "198.51.100.71" {
		t.Fatalf("hosts = %+v", hosts)
	}
}

func TestEnsureNetworkSecurityGroupCreatesRules(t *testing.T) {
	privateKey := testPrivateKey(t)
	var createdGroup map[string]any
	var addedRules map[string][]SecurityRule
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertOCISignature(t, r, privateKey)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/20160918/networkSecurityGroups":
			writeJSON(t, w, []NetworkSecurityGroup{})
		case r.Method == http.MethodPost && r.URL.Path == "/20160918/networkSecurityGroups":
			decodeJSON(t, r, &createdGroup)
			writeJSON(t, w, NetworkSecurityGroup{
				ID:           "nsg-1",
				DisplayName:  "ship-demo-production",
				FreeformTags: provider.ShipLabels("demo", "production", "network"),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/20160918/networkSecurityGroups/nsg-1/securityRules":
			writeJSON(t, w, []SecurityRule{})
		case r.Method == http.MethodPost && r.URL.Path == "/20160918/networkSecurityGroups/nsg-1/actions/addSecurityRules":
			decodeJSON(t, r, &addedRules)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer api.Close()

	client := testClient(api, pemForKey(privateKey))
	group, err := client.EnsureNetworkSecurityGroup(context.Background(), "demo", "production", config.OCIConfig{
		CompartmentID: "compartment-1",
		NetworkSecurityGroup: config.OCINetworkSecurityGroup{
			VCNID: "vcn-1",
		},
		SSHAllowedCIDRs: []string{"203.0.113.0/24"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if group.ID != "nsg-1" {
		t.Fatalf("group = %+v", group)
	}
	if createdGroup["vcnId"] != "vcn-1" || createdGroup["displayName"] != "ship-demo-production" {
		t.Fatalf("created group = %+v", createdGroup)
	}
	rules := addedRules["securityRules"]
	if !hasSecurityRule(rules, "INGRESS", "203.0.113.0/24", 22) ||
		!hasSecurityRule(rules, "INGRESS", "0.0.0.0/0", 443) ||
		!hasEgressRule(rules) {
		t.Fatalf("rules = %+v", rules)
	}
}

func TestDeleteTerminatesInstance(t *testing.T) {
	privateKey := testPrivateKey(t)
	var path string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertOCISignature(t, r, privateKey)
		path = r.URL.String()
		if r.Method != http.MethodDelete || r.URL.Path != "/20160918/instances/instance-1" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer api.Close()

	client := testClient(api, pemForKey(privateKey))
	if err := client.Delete(context.Background(), provider.Host{ID: "instance-1"}); err != nil {
		t.Fatal(err)
	}
	if path != "/20160918/instances/instance-1?preserveBootVolume=false" {
		t.Fatalf("path = %q", path)
	}
}

func TestCredentialChecks(t *testing.T) {
	checks := Client{}.CredentialChecks(func(key string) (string, bool) {
		switch key {
		case "OCI_TENANCY_OCID", "OCI_USER_OCID", "OCI_FINGERPRINT", "OCI_PRIVATE_KEY":
			return "value", true
		default:
			return "", false
		}
	})
	if len(checks) != 2 || !checks[0].Present || !checks[1].Present {
		t.Fatalf("checks = %+v", checks)
	}
}

func testClient(api *httptest.Server, privateKeyPEM string) Client {
	return Client{
		TenancyOCID:   "ocid1.tenancy.oc1..tenancy",
		UserOCID:      "ocid1.user.oc1..user",
		Fingerprint:   "aa:bb:cc",
		PrivateKeyPEM: privateKeyPEM,
		Region:        "us-ashburn-1",
		CompartmentID: "ocid1.compartment.oc1..aaaa",
		BaseURL:       api.URL,
		HTTP:          api.Client(),
		Now:           func() time.Time { return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC) },
	}
}

func testPrivateKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func pemForKey(key *rsa.PrivateKey) string {
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	return string(pem.EncodeToMemory(block))
}

func assertOCISignature(t *testing.T, r *http.Request, key *rsa.PrivateKey) {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	auth := r.Header.Get("Authorization")
	if !strings.Contains(auth, `keyId="ocid1.tenancy.oc1..tenancy/ocid1.user.oc1..user/aa:bb:cc"`) ||
		!strings.Contains(auth, `algorithm="rsa-sha256"`) {
		t.Fatalf("authorization = %q", auth)
	}
	headers := signatureField(auth, "headers")
	signature := signatureField(auth, "signature")
	if headers == "" || signature == "" {
		t.Fatalf("authorization = %q", auth)
	}
	signingString := Client{}.signingString(r, strings.Split(headers, " "))
	hash := sha256.Sum256([]byte(signingString))
	decoded, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		t.Fatal(err)
	}
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, hash[:], decoded); err != nil {
		t.Fatalf("invalid OCI signature: %v\n%s", err, signingString)
	}
}

func signatureField(auth, name string) string {
	prefix := name + `="`
	start := strings.Index(auth, prefix)
	if start < 0 {
		return ""
	}
	start += len(prefix)
	end := strings.Index(auth[start:], `"`)
	if end < 0 {
		return ""
	}
	return auth[start : start+end]
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

func hasSecurityRule(rules []SecurityRule, direction, cidr string, port int) bool {
	for _, rule := range rules {
		if rule.Direction == direction && rule.Source == cidr && rule.TCPOptions != nil &&
			rule.TCPOptions.DestinationPortRange.Min == port &&
			rule.TCPOptions.DestinationPortRange.Max == port {
			return true
		}
	}
	return false
}

func hasEgressRule(rules []SecurityRule) bool {
	for _, rule := range rules {
		if rule.Direction == "EGRESS" && rule.Protocol == "all" && rule.Destination == "0.0.0.0/0" {
			return true
		}
	}
	return false
}
