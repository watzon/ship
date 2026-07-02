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
	file := GenerateCaddyfile(cfg, nil, []scheduler.Placement{
		{Service: "web", Replica: 1, Host: scheduler.Host{Name: "web-1"}},
		{Service: "web", Replica: 2, Host: scheduler.Host{Name: "web-2"}},
	})
	for _, needle := range []string{
		"example.com {",
		"handle /_ship/health {",
		"respond \"ok\" 200",
		"reverse_proxy web-1:3000 web-2:3000",
		"lb_policy round_robin",
		"lb_try_duration 5s",
		"fail_duration 30s",
		"max_fails 1",
		"unhealthy_status 5xx",
		"health_uri /up",
	} {
		if !strings.Contains(file, needle) {
			t.Fatalf("missing %q:\n%s", needle, file)
		}
	}
	if strings.Contains(file, "handle /_ship/health { respond") {
		t.Fatalf("health handler must be multiline:\n%s", file)
	}
}

func TestGenerateCaddyfileUsesConfiguredProxyHealth(t *testing.T) {
	enabled := true
	cfg := &config.Config{Services: map[string]config.Service{
		"web": {
			Ports: []int{3000},
			Ingress: &config.Ingress{
				Domains: []string{"example.com"},
				Health: config.IngressHealth{
					Enabled:                    &enabled,
					Path:                       "/ready",
					IntervalSeconds:            7,
					TimeoutSeconds:             2,
					Passes:                     2,
					Fails:                      3,
					TryDurationSeconds:         4,
					PassiveFailDurationSeconds: 12,
					PassiveMaxFails:            5,
					UnhealthyStatus:            []string{"5xx", "429"},
				},
			},
		},
	}}
	file := GenerateCaddyfile(cfg, nil, []scheduler.Placement{{Service: "web", Replica: 1, Host: scheduler.Host{Name: "web-1"}}})
	for _, needle := range []string{
		"lb_try_duration 4s",
		"fail_duration 12s",
		"max_fails 5",
		"unhealthy_status 5xx 429",
		"health_uri /ready",
		"health_interval 7s",
		"health_timeout 2s",
		"health_passes 2",
		"health_fails 3",
	} {
		if !strings.Contains(file, needle) {
			t.Fatalf("missing %q:\n%s", needle, file)
		}
	}
}

func TestGenerateCaddyfileCanDisableProxyHealth(t *testing.T) {
	disabled := false
	cfg := &config.Config{Services: map[string]config.Service{
		"web": {
			Ports:  []int{3000},
			Health: config.HealthCheck{HTTP: "/up"},
			Ingress: &config.Ingress{
				Domains: []string{"example.com"},
				Health:  config.IngressHealth{Enabled: &disabled},
			},
		},
	}}
	file := GenerateCaddyfile(cfg, nil, []scheduler.Placement{{Service: "web", Replica: 1, Host: scheduler.Host{Name: "web-1"}}})
	for _, unexpected := range []string{"health_uri", "fail_duration", "unhealthy_status", "lb_try_duration"} {
		if strings.Contains(file, unexpected) {
			t.Fatalf("unexpected %q:\n%s", unexpected, file)
		}
	}
}

func TestGenerateCaddyfileIncludesIngressRedirects(t *testing.T) {
	cfg := &config.Config{Services: map[string]config.Service{
		"web": {
			Ports: []int{3000},
			Ingress: &config.Ingress{
				Domains: []string{"example.com"},
				Redirects: []config.IngressRedirect{
					{From: []string{"www.example.com"}, To: "https://example.com"},
					{From: []string{"old.example.com", "legacy.example.com"}, To: "https://example.com/new", Code: 301},
				},
			},
		},
	}}
	file := GenerateCaddyfile(cfg, nil, []scheduler.Placement{{Service: "web", Replica: 1, Host: scheduler.Host{Name: "web-1"}}})
	for _, needle := range []string{
		"example.com {",
		"reverse_proxy web:3000",
		"legacy.example.com, old.example.com {",
		"redir https://example.com/new{uri} 301",
		"www.example.com {",
		"redir https://example.com{uri} 308",
	} {
		if !strings.Contains(file, needle) {
			t.Fatalf("missing %q:\n%s", needle, file)
		}
	}
}

func TestGenerateCaddyfileCanDisableRedirectURIPreservation(t *testing.T) {
	preserveURI := false
	cfg := &config.Config{Services: map[string]config.Service{
		"web": {
			Ingress: &config.Ingress{
				Redirects: []config.IngressRedirect{
					{From: []string{"www.example.com"}, To: "https://example.com/start", PreserveURI: &preserveURI},
				},
			},
		},
	}}
	file := GenerateCaddyfileFromReplicas(cfg, nil)
	if !strings.Contains(file, "redir https://example.com/start 308") {
		t.Fatalf("redirect target should not preserve uri:\n%s", file)
	}
	if strings.Contains(file, "{uri}") {
		t.Fatalf("uri placeholder should be omitted:\n%s", file)
	}
}

func TestGenerateCaddyfileUsesHostContactForDedicatedIngress(t *testing.T) {
	cfg := &config.Config{Services: map[string]config.Service{
		"web": {
			Ports:   []int{3000},
			Ingress: &config.Ingress{Domains: []string{"example.com"}},
		},
	}}
	ingressHosts := []scheduler.Host{{Name: "ingress-1", Pool: "ingress"}}
	file := GenerateCaddyfile(cfg, ingressHosts, []scheduler.Placement{
		{Service: "web", Replica: 1, Host: scheduler.Host{Name: "web-1", Contact: "198.51.100.10"}},
	})
	if !strings.Contains(file, "reverse_proxy 198.51.100.10:3000") {
		t.Fatalf("contact upstream missing:\n%s", file)
	}
	if strings.Contains(file, "web-1:3000") {
		t.Fatalf("logical host name leaked into upstream:\n%s", file)
	}
}

func TestGenerateCaddyfileUsesServiceNameForCoLocatedIngress(t *testing.T) {
	cfg := &config.Config{Services: map[string]config.Service{
		"web": {
			Ports:   []int{4185},
			Ingress: &config.Ingress{Domains: []string{"api.example.com"}},
		},
	}}
	host := scheduler.Host{Name: "npxray-staging", Pool: "web", Contact: "npxray-staging"}
	file := GenerateCaddyfile(cfg, []scheduler.Host{host}, []scheduler.Placement{
		{Service: "web", Replica: 1, Host: host},
	})
	if !strings.Contains(file, "reverse_proxy web:4185") {
		t.Fatalf("docker service alias upstream missing:\n%s", file)
	}
	if strings.Contains(file, "npxray-staging:4185") {
		t.Fatalf("ssh host alias leaked into upstream:\n%s", file)
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

func TestGenerateMaintenanceCaddyfile(t *testing.T) {
	cfg := &config.Config{Services: map[string]config.Service{
		"web": {
			Ingress: &config.Ingress{Domains: []string{"www.example.com", "example.com"}},
		},
		"api": {
			Ingress: &config.Ingress{Domains: []string{"api.example.com"}},
		},
		"worker": {},
	}}
	file := GenerateMaintenanceCaddyfile(cfg, `deploying "v2"`)
	apiAt := strings.Index(file, "api.example.com {")
	webAt := strings.Index(file, "example.com, www.example.com {")
	if apiAt < 0 || webAt < 0 || apiAt > webAt {
		t.Fatalf("maintenance site order is wrong:\n%s", file)
	}
	for _, needle := range []string{
		`header Cache-Control "no-store"`,
		`respond "deploying \"v2\"" 503`,
	} {
		if !strings.Contains(file, needle) {
			t.Fatalf("maintenance file missing %q:\n%s", needle, file)
		}
	}
	if strings.Contains(file, "worker") || strings.Contains(file, "reverse_proxy") {
		t.Fatalf("maintenance file included non-ingress or upstream content:\n%s", file)
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
