package scheduler

import (
	"fmt"
	"sort"
	"strings"

	"github.com/watzon/ship/internal/config"
)

type Host struct {
	Name           string
	Pool           string
	User           string
	Contact        string
	SSHPort        int
	IdentityFile   string
	KnownHostsFile string
	JumpHost       string
	SSHOptions     map[string]string
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
		ssh := mergeSSHConfig(env.SSH, pool.SSH)
		if len(pool.Hosts) > 0 {
			names := append([]string(nil), pool.Hosts...)
			sort.Strings(names)
			for _, name := range names {
				hosts = append(hosts, hostWithSSH(Host{Name: name, Pool: poolName, User: user}, ssh))
			}
			continue
		}
		for i := 1; i <= pool.Count; i++ {
			hosts = append(hosts, hostWithSSH(Host{Name: fmt.Sprintf("%s-%d", poolName, i), Pool: poolName, User: user}, ssh))
		}
	}
	return hosts
}

func hostWithSSH(host Host, ssh config.SSHConfig) Host {
	host.SSHPort = ssh.Port
	host.IdentityFile = ssh.IdentityFile
	host.KnownHostsFile = ssh.KnownHostsFile
	host.JumpHost = ssh.JumpHost
	if len(ssh.Options) > 0 {
		host.SSHOptions = make(map[string]string, len(ssh.Options))
		for key, value := range ssh.Options {
			host.SSHOptions[key] = value
		}
	}
	return host
}

func mergeSSHConfig(base, override config.SSHConfig) config.SSHConfig {
	out := base
	if override.Port != 0 {
		out.Port = override.Port
	}
	if override.IdentityFile != "" {
		out.IdentityFile = override.IdentityFile
	}
	if override.KnownHostsFile != "" {
		out.KnownHostsFile = override.KnownHostsFile
	}
	if override.JumpHost != "" {
		out.JumpHost = override.JumpHost
	}
	if len(override.Options) > 0 {
		out.Options = make(map[string]string, len(base.Options)+len(override.Options))
		for key, value := range base.Options {
			out.Options[key] = value
		}
		for key, value := range override.Options {
			out.Options[key] = value
		}
	}
	return out
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
