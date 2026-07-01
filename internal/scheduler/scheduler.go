package scheduler

import (
	"fmt"
	"sort"
	"strings"

	"github.com/watzon/ship/internal/config"
)

type Host struct {
	Name    string
	Pool    string
	User    string
	Contact string
}

type Placement struct {
	Service string
	Replica int
	Host    Host
}

func HostsForEnvironment(env config.Environment) []Host {
	var hosts []Host
	poolNames := make([]string, 0, len(env.Hosts.Pools))
	for poolName := range env.Hosts.Pools {
		poolNames = append(poolNames, poolName)
	}
	sort.Strings(poolNames)
	for _, poolName := range poolNames {
		pool := env.Hosts.Pools[poolName]
		user := pool.User
		if user == "" {
			user = "root"
		}
		if len(pool.Hosts) > 0 {
			names := append([]string(nil), pool.Hosts...)
			sort.Strings(names)
			for _, name := range names {
				hosts = append(hosts, Host{Name: name, Pool: poolName, User: user})
			}
			continue
		}
		for i := 1; i <= pool.Count; i++ {
			hosts = append(hosts, Host{Name: fmt.Sprintf("%s-%d", poolName, i), Pool: poolName, User: user})
		}
	}
	return hosts
}

func (h Host) ContactTarget() string {
	if contact := strings.TrimSpace(h.Contact); contact != "" {
		return contact
	}
	return h.Name
}

func PlaceServices(cfg *config.Config, env config.Environment) ([]Placement, error) {
	return PlaceServicesOnHosts(cfg, HostsForEnvironment(env))
}

func PlaceServicesOnHosts(cfg *config.Config, hosts []Host) ([]Placement, error) {
	byPool := map[string][]Host{}
	for _, host := range hosts {
		byPool[host.Pool] = append(byPool[host.Pool], host)
	}
	serviceNames := make([]string, 0, len(cfg.Services))
	for name := range cfg.Services {
		serviceNames = append(serviceNames, name)
	}
	sort.Strings(serviceNames)
	var placements []Placement
	for _, serviceName := range serviceNames {
		svc := cfg.Services[serviceName]
		poolHosts := byPool[svc.Pool]
		if svc.Scale > 0 && len(poolHosts) == 0 {
			return nil, fmt.Errorf("service %q has scale %d but pool %q has no hosts", serviceName, svc.Scale, svc.Pool)
		}
		for replica := 1; replica <= svc.Scale; replica++ {
			host := poolHosts[(replica-1)%len(poolHosts)]
			placements = append(placements, Placement{Service: serviceName, Replica: replica, Host: host})
		}
	}
	return placements, nil
}
