package ingress

import (
	"fmt"
	"sort"
	"strings"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/scheduler"
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
		b.WriteString("  }\n")
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
