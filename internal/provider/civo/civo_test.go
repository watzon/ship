package civo

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

func TestDesiredInstancesUsesProviderDefaultsAndPoolOverrides(t *testing.T) {
	env := config.Environment{
		Provider: config.ProviderConfig{Civo: &config.CivoConfig{
			Region:   "lon1",
			Size:     "g3.small",
			Image:    "ubuntu-noble",
			UserData: "#!/bin/sh\necho provider\n",
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {
				Count:    1,
				Size:     "g3.medium",
				UserData: "#!/bin/sh\necho pool\n",
				Labels:   map[string]string{"owner": "platform"},
			},
		}},
	}

	plans := DesiredInstancesFor("demo", "production", env)
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(plans))
	}
	plan := plans[0]
	if plan.Location != "lon1" || plan.Size != "g3.medium" || plan.Image != "ubuntu-noble" {
		t.Fatalf("plan shape = %+v", plan)
	}
	if plan.UserData != "#!/bin/sh\necho pool\n" {
		t.Fatalf("user data = %q", plan.UserData)
	}
	if plan.Labels["owner"] != "platform" || plan.Labels[provider.LabelProject] != "demo" {
		t.Fatalf("labels = %+v", plan.Labels)
	}
}

func TestCreateInstanceSendsCivoForm(t *testing.T) {
	var form url.Values
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/instances" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "bearer token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		form = r.PostForm
		writeJSON(t, w, Instance{
			ID:       "instance-1",
			Hostname: "web-1",
			PublicIP: "198.51.100.40",
			Tags:     strings.Fields(r.PostForm.Get("tags")),
		})
	}))
	defer api.Close()

	publicIP := false
	client := Client{Token: "token", BaseURL: api.URL, HTTP: api.Client()}
	instance, err := client.CreateInstance(context.Background(), provider.HostPlan{
		Project:     "demo",
		Environment: "production",
		Name:        "web-1",
		Pool:        "web",
		Location:    "lon1",
		Size:        "g3.small",
		Image:       "ubuntu-noble",
		UserData:    "#!/bin/sh\necho hi\n",
		Labels:      provider.ShipLabels("demo", "production", "web"),
	}, config.CivoConfig{
		NetworkID:             "network-1",
		SSHKeyID:              "ssh-key-1",
		InitialUser:           "deploy",
		PublicIP:              &publicIP,
		AllowedIPs:            []string{"10.0.0.10"},
		NetworkBandwidthLimit: 100,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if instance.ID != "instance-1" || instance.PublicIP != "198.51.100.40" {
		t.Fatalf("instance = %+v", instance)
	}
	for key, want := range map[string]string{
		"hostname":                "web-1",
		"size":                    "g3.small",
		"template_id":             "ubuntu-noble",
		"network_id":              "network-1",
		"region":                  "lon1",
		"public_ip":               "none",
		"ssh_key_id":              "ssh-key-1",
		"initial_user":            "deploy",
		"script":                  "#!/bin/sh\necho hi\n",
		"network_bandwidth_limit": "100",
	} {
		if got := form.Get(key); got != want {
			t.Fatalf("%s = %q, want %q (form=%v)", key, got, want, form)
		}
	}
	if form.Get("allowed_ips") != "10.0.0.10" {
		t.Fatalf("allowed_ips = %v", form["allowed_ips"])
	}
	if !strings.Contains(form.Get("tags"), "ship:managed-by:ship") {
		t.Fatalf("tags = %q", form.Get("tags"))
	}
}

func TestListInstancesFiltersShipTags(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/instances" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("region") != "lon1" {
			t.Fatalf("region = %q", r.URL.Query().Get("region"))
		}
		writeJSON(t, w, listInstancesResponse{
			Page:  1,
			Pages: 1,
			Items: []Instance{
				{ID: "instance-1", Hostname: "web-1", PublicIP: "198.51.100.40", Tags: tagsFromLabels(provider.ShipLabels("demo", "production", "web"))},
				{ID: "instance-2", Hostname: "web-2", Tags: tagsFromLabels(provider.ShipLabels("demo", "staging", "web"))},
			},
		})
	}))
	defer api.Close()

	client := Client{Token: "token", Region: "lon1", BaseURL: api.URL, HTTP: api.Client()}
	hosts, err := client.List(context.Background(), "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].Name != "web-1" || hosts[0].PublicAddress != "198.51.100.40" {
		t.Fatalf("hosts = %+v", hosts)
	}
}

func TestEnsureFirewallCreatesFirewallAndMissingRules(t *testing.T) {
	var firewallForm url.Values
	var createdRules []url.Values
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/firewalls":
			writeJSON(t, w, []Firewall{})
		case r.Method == http.MethodPost && r.URL.Path == "/firewalls":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			firewallForm = r.PostForm
			writeJSON(t, w, Firewall{ID: "firewall-1", Name: r.PostForm.Get("name"), NetworkID: r.PostForm.Get("network_id")})
		case r.Method == http.MethodGet && r.URL.Path == "/firewalls/firewall-1/rules":
			writeJSON(t, w, []FirewallRule{{Protocol: "tcp", StartPort: "80", Direction: "ingress", CIDR: []string{"0.0.0.0/0"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/firewalls/firewall-1/rules":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			createdRules = append(createdRules, r.PostForm)
			writeJSON(t, w, FirewallRule{ID: "rule-1", Protocol: r.PostForm.Get("protocol"), StartPort: r.PostForm.Get("start_port"), Direction: r.PostForm.Get("direction"), CIDR: []string{r.PostForm.Get("cidr")}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()

	client := Client{Token: "token", BaseURL: api.URL, HTTP: api.Client()}
	firewall, err := client.EnsureFirewall(context.Background(), "demo", "production", config.CivoConfig{
		Region:          "lon1",
		NetworkID:       "network-1",
		SSHAllowedCIDRs: []string{"203.0.113.0/24"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if firewall.ID != "firewall-1" {
		t.Fatalf("firewall = %+v", firewall)
	}
	if firewallForm.Get("name") != "ship-demo-production-firewall" || firewallForm.Get("network_id") != "network-1" || firewallForm.Get("region") != "lon1" {
		t.Fatalf("firewall form = %+v", firewallForm)
	}
	if len(createdRules) != 6 {
		t.Fatalf("created rules = %+v", createdRules)
	}
	if !hasRule(createdRules, "tcp", "203.0.113.0/24", "22") || !hasRule(createdRules, "tcp", "::/0", "443") || !hasRule(createdRules, "udp", "::/0", "443") {
		t.Fatalf("created rules missing expected entries: %+v", createdRules)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func tagsFromLabels(labels map[string]string) []string {
	tags := make([]string, 0, len(labels))
	for key, value := range labels {
		tags = append(tags, tagForLabel(key, value))
	}
	return tags
}

func hasRule(rules []url.Values, protocol, cidr, port string) bool {
	for _, rule := range rules {
		if rule.Get("protocol") == protocol && rule.Get("cidr") == cidr && rule.Get("start_port") == port {
			return true
		}
	}
	return false
}
