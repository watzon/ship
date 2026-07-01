package ingress

import (
	"strings"
	"testing"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/scheduler"
)

func TestGenerateCaddyfile(t *testing.T) {
	cfg := &config.Config{Services: map[string]config.Service{
		"web": {
			Ports:   []int{3000},
			Health:  config.HealthCheck{HTTP: "/up"},
			Ingress: &config.Ingress{Domains: []string{"example.com"}},
		},
	}}
	file := GenerateCaddyfile(cfg, []scheduler.Placement{
		{Service: "web", Replica: 1, Host: scheduler.Host{Name: "web-1"}},
		{Service: "web", Replica: 2, Host: scheduler.Host{Name: "web-2"}},
	})
	for _, needle := range []string{"example.com {", "reverse_proxy web-1:3000 web-2:3000", "lb_policy round_robin"} {
		if !strings.Contains(file, needle) {
			t.Fatalf("missing %q:\n%s", needle, file)
		}
	}
}

func TestGenerateCaddyfileUsesHostContactForUpstreams(t *testing.T) {
	cfg := &config.Config{Services: map[string]config.Service{
		"web": {
			Ports:   []int{3000},
			Ingress: &config.Ingress{Domains: []string{"example.com"}},
		},
	}}
	file := GenerateCaddyfile(cfg, []scheduler.Placement{
		{Service: "web", Replica: 1, Host: scheduler.Host{Name: "web-1", Contact: "198.51.100.10"}},
	})
	if !strings.Contains(file, "reverse_proxy 198.51.100.10:3000") {
		t.Fatalf("contact upstream missing:\n%s", file)
	}
	if strings.Contains(file, "web-1:3000") {
		t.Fatalf("logical host name leaked into upstream:\n%s", file)
	}
}

func TestGenerateCaddyfileSortsDomainsServicesAndUpstreams(t *testing.T) {
	cfg := &config.Config{Services: map[string]config.Service{
		"web": {
			Ports:   []int{3000},
			Ingress: &config.Ingress{Domains: []string{"www.example.com", "example.com"}},
		},
		"api": {
			Ports:   []int{4000},
			Ingress: &config.Ingress{Domains: []string{"api.example.com"}},
		},
	}}
	file := GenerateCaddyfileFromReplicas(cfg, []Replica{
		{Service: "web", Host: "web-b", Port: 3000},
		{Service: "api", Host: "api-1", Port: 4000},
		{Service: "web", Host: "web-a", Port: 3000},
	})
	apiAt := strings.Index(file, "api.example.com {")
	webAt := strings.Index(file, "example.com, www.example.com {")
	if apiAt < 0 || webAt < 0 || apiAt > webAt {
		t.Fatalf("service/domain order is wrong:\n%s", file)
	}
	if !strings.Contains(file, "reverse_proxy web-a:3000 web-b:3000") {
		t.Fatalf("upstreams are not sorted:\n%s", file)
	}
	if strings.Contains(file, "http://example.com") {
		t.Fatalf("site address should leave Caddy automatic HTTPS enabled:\n%s", file)
	}
}

func TestGenerateCaddyfileUsesOnlyProvidedHealthyReplicas(t *testing.T) {
	cfg := &config.Config{Services: map[string]config.Service{
		"web": {
			Ports:   []int{3000},
			Ingress: &config.Ingress{Domains: []string{"example.com"}},
		},
	}}
	file := GenerateCaddyfileFromReplicas(cfg, []Replica{
		{Service: "web", Host: "web-healthy", Port: 3000},
	})
	if !strings.Contains(file, "web-healthy:3000") {
		t.Fatalf("healthy upstream missing:\n%s", file)
	}
	if strings.Contains(file, "web-unhealthy") {
		t.Fatalf("unhealthy upstream should not appear:\n%s", file)
	}
}

func TestHostsForEnvironmentPrefersIngressPool(t *testing.T) {
	cfg := &config.Config{Services: map[string]config.Service{
		"web": {Ingress: &config.Ingress{Domains: []string{"example.com"}}},
	}}
	env := config.Environment{Hosts: config.HostsConfig{Pools: map[string]config.Pool{
		"web":     {Count: 1},
		"ingress": {Count: 2},
	}}}
	hosts := HostsForEnvironment(cfg, env, []scheduler.Placement{
		{Service: "web", Host: scheduler.Host{Name: "web-1", Pool: "web"}},
	})
	if len(hosts) != 2 || hosts[0].Name != "ingress-1" || hosts[1].Name != "ingress-2" {
		t.Fatalf("hosts = %+v", hosts)
	}
}
