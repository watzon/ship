package ingress

import (
	"fmt"
	"sort"
	"strings"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/scheduler"
)

const (
	defaultProxyTryDurationSeconds         = 5
	defaultProxyPassiveFailDurationSeconds = 30
	defaultProxyPassiveMaxFails            = 1
	defaultProxyUnhealthyStatus            = "5xx"
	defaultRedirectCode                    = 308
)

type Upstream struct {
	Service string
	Host    string
	Port    int
}

type Replica struct {
	Service string
	Host    string
	Port    int
}

type redirectBlock struct {
	Service string
	From    []string
	To      string
	Code    int
}

func GenerateCaddyfile(cfg *config.Config, placements []scheduler.Placement) string {
	return GenerateCaddyfileFromReplicas(cfg, ReplicasFromPlacements(cfg, placements))
}

func ReplicasFromPlacements(cfg *config.Config, placements []scheduler.Placement) []Replica {
	var replicas []Replica
	for _, placement := range placements {
		svc := cfg.Services[placement.Service]
		if svc.Ingress == nil || len(svc.Ingress.Domains) == 0 || len(svc.Ports) == 0 {
			continue
		}
		replicas = append(replicas, Replica{
			Service: placement.Service,
			Host:    placement.Host.ContactTarget(),
			Port:    svc.Ports[0],
		})
	}
	return replicas
}

func GenerateCaddyfileFromReplicas(cfg *config.Config, replicas []Replica) string {
	upstreams := map[string][]Upstream{}
	serviceNames := make([]string, 0, len(cfg.Services))
	for name := range cfg.Services {
		serviceNames = append(serviceNames, name)
	}
	sort.Strings(serviceNames)
	for _, replica := range replicas {
		svc := cfg.Services[replica.Service]
		if svc.Ingress == nil || len(svc.Ingress.Domains) == 0 || len(svc.Ports) == 0 {
			continue
		}
		port := replica.Port
		if port == 0 {
			port = svc.Ports[0]
		}
		upstreams[replica.Service] = append(upstreams[replica.Service], Upstream{
			Service: replica.Service,
			Host:    replica.Host,
			Port:    port,
		})
	}
	for serviceName := range upstreams {
		sort.Slice(upstreams[serviceName], func(i, j int) bool {
			if upstreams[serviceName][i].Host != upstreams[serviceName][j].Host {
				return upstreams[serviceName][i].Host < upstreams[serviceName][j].Host
			}
			return upstreams[serviceName][i].Port < upstreams[serviceName][j].Port
		})
	}
	var b strings.Builder
	for _, serviceName := range serviceNames {
		svc := cfg.Services[serviceName]
		if svc.Ingress == nil || len(svc.Ingress.Domains) == 0 {
			continue
		}
		if len(upstreams[serviceName]) == 0 {
			continue
		}
		domains := append([]string(nil), svc.Ingress.Domains...)
		sort.Strings(domains)
		fmt.Fprintf(&b, "%s {\n", strings.Join(domains, ", "))
		b.WriteString("  encode zstd gzip\n")
		if svc.Health.HTTP != "" {
			fmt.Fprintf(&b, "  handle /_ship/health { respond \"ok\" 200 }\n")
		}
		b.WriteString("  reverse_proxy")
		for _, upstream := range upstreams[serviceName] {
			fmt.Fprintf(&b, " %s:%d", upstream.Host, upstream.Port)
		}
		b.WriteString(" {\n")
		b.WriteString("    lb_policy round_robin\n")
		writeProxyHealth(&b, svc)
		b.WriteString("  }\n")
		b.WriteString("}\n\n")
	}
	if b.Len() == 0 {
		writeRedirects(&b, cfg)
		if b.Len() == 0 {
			return ""
		}
		return strings.TrimSpace(b.String()) + "\n"
	}
	writeRedirects(&b, cfg)
	return strings.TrimSpace(b.String()) + "\n"
}

func writeRedirects(b *strings.Builder, cfg *config.Config) {
	redirects := redirectBlocks(cfg)
	for _, redirect := range redirects {
		fmt.Fprintf(b, "%s {\n", strings.Join(redirect.From, ", "))
		fmt.Fprintf(b, "  redir %s %d\n", redirect.To, redirect.Code)
		b.WriteString("}\n\n")
	}
}

func redirectBlocks(cfg *config.Config) []redirectBlock {
	var out []redirectBlock
	serviceNames := make([]string, 0, len(cfg.Services))
	for name := range cfg.Services {
		serviceNames = append(serviceNames, name)
	}
	sort.Strings(serviceNames)
	for _, serviceName := range serviceNames {
		svc := cfg.Services[serviceName]
		if svc.Ingress == nil {
			continue
		}
		for _, redirect := range svc.Ingress.Redirects {
			from := compactStrings(redirect.From)
			if len(from) == 0 || strings.TrimSpace(redirect.To) == "" {
				continue
			}
			sort.Strings(from)
			code := redirect.Code
			if code == 0 {
				code = defaultRedirectCode
			}
			out = append(out, redirectBlock{
				Service: serviceName,
				From:    from,
				To:      redirectTarget(redirect),
				Code:    code,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].From[0] != out[j].From[0] {
			return out[i].From[0] < out[j].From[0]
		}
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].To < out[j].To
	})
	return out
}

func redirectTarget(redirect config.IngressRedirect) string {
	target := strings.TrimSpace(redirect.To)
	if redirect.PreserveURI != nil && !*redirect.PreserveURI {
		return target
	}
	if strings.Contains(target, "{uri}") {
		return target
	}
	return strings.TrimRight(target, "/") + "{uri}"
}

func compactStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func writeProxyHealth(b *strings.Builder, svc config.Service) {
	if !proxyHealthEnabled(svc) {
		return
	}
	health := svc.Ingress.Health
	if seconds := durationSeconds(health.TryDurationSeconds, defaultProxyTryDurationSeconds); seconds > 0 {
		fmt.Fprintf(b, "    lb_try_duration %ds\n", seconds)
	}
	if seconds := durationSeconds(health.PassiveFailDurationSeconds, defaultProxyPassiveFailDurationSeconds); seconds > 0 {
		fmt.Fprintf(b, "    fail_duration %ds\n", seconds)
	}
	if maxFails := intDefault(health.PassiveMaxFails, defaultProxyPassiveMaxFails); maxFails > 0 {
		fmt.Fprintf(b, "    max_fails %d\n", maxFails)
	}
	statuses := health.UnhealthyStatus
	if len(statuses) == 0 {
		statuses = []string{defaultProxyUnhealthyStatus}
	}
	fmt.Fprintf(b, "    unhealthy_status %s\n", strings.Join(statuses, " "))
	path := strings.TrimSpace(health.Path)
	if path == "" {
		path = strings.TrimSpace(svc.Health.HTTP)
	}
	if path == "" {
		return
	}
	fmt.Fprintf(b, "    health_uri %s\n", path)
	if health.IntervalSeconds > 0 {
		fmt.Fprintf(b, "    health_interval %ds\n", health.IntervalSeconds)
	}
	if health.TimeoutSeconds > 0 {
		fmt.Fprintf(b, "    health_timeout %ds\n", health.TimeoutSeconds)
	}
	if health.Passes > 0 {
		fmt.Fprintf(b, "    health_passes %d\n", health.Passes)
	}
	if health.Fails > 0 {
		fmt.Fprintf(b, "    health_fails %d\n", health.Fails)
	}
}

func proxyHealthEnabled(svc config.Service) bool {
	if svc.Ingress == nil {
		return false
	}
	if svc.Ingress.Health.Enabled != nil {
		return *svc.Ingress.Health.Enabled
	}
	return true
}

func durationSeconds(configured, fallback int) int {
	if configured > 0 {
		return configured
	}
	return fallback
}

func intDefault(configured, fallback int) int {
	if configured > 0 {
		return configured
	}
	return fallback
}

func GenerateMaintenanceCaddyfile(cfg *config.Config, message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "Service temporarily unavailable for maintenance."
	}
	serviceNames := make([]string, 0, len(cfg.Services))
	for name := range cfg.Services {
		serviceNames = append(serviceNames, name)
	}
	sort.Strings(serviceNames)
	var b strings.Builder
	for _, serviceName := range serviceNames {
		svc := cfg.Services[serviceName]
		if svc.Ingress == nil || len(svc.Ingress.Domains) == 0 {
			continue
		}
		domains := append([]string(nil), svc.Ingress.Domains...)
		sort.Strings(domains)
		fmt.Fprintf(&b, "%s {\n", strings.Join(domains, ", "))
		b.WriteString("  encode zstd gzip\n")
		b.WriteString("  header Cache-Control \"no-store\"\n")
		fmt.Fprintf(&b, "  respond %q 503\n", message)
		b.WriteString("}\n\n")
	}
	if b.Len() == 0 {
		return ""
	}
	return strings.TrimSpace(b.String()) + "\n"
}

func HostsForEnvironment(cfg *config.Config, env config.Environment, placements []scheduler.Placement) []scheduler.Host {
	return HostsFor(cfg, scheduler.HostsForEnvironment(env), placements)
}

func HostsFor(cfg *config.Config, hosts []scheduler.Host, placements []scheduler.Placement) []scheduler.Host {
	var dedicated []scheduler.Host
	for _, host := range hosts {
		if host.Pool == "ingress" {
			dedicated = append(dedicated, host)
		}
	}
	if len(dedicated) > 0 {
		return dedicated
	}

	byName := map[string]scheduler.Host{}
	for _, placement := range placements {
		svc := cfg.Services[placement.Service]
		if svc.Ingress == nil || len(svc.Ingress.Domains) == 0 {
			continue
		}
		byName[placement.Host.Name] = placement.Host
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	fallback := make([]scheduler.Host, 0, len(names))
	for _, name := range names {
		fallback = append(fallback, byName[name])
	}
	return fallback
}
