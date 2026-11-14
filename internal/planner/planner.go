package planner

import (
	"fmt"
	"sort"
	"strings"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/ipaddr"
	"github.com/watzon/ship/internal/provider"
	"github.com/watzon/ship/internal/scheduler"
)

type Action struct {
	Kind    string `json:"kind"`
	Target  string `json:"target"`
	Details string `json:"details,omitempty"`
}

type Plan struct {
	Environment string   `json:"environment"`
	Actions     []Action `json:"actions"`
	// Summary counts actions by kind. Provision plans set it so that "this
	// will create N servers" is visible without reading every action.
	Summary map[string]int `json:"summary,omitempty"`
	Notes   []string       `json:"notes,omitempty"`
}

func (p Plan) Empty() bool {
	return len(p.Actions) == 0
}

func (p Plan) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", p.Environment)
	for _, action := range p.Actions {
		if action.Details == "" {
			fmt.Fprintf(&b, "- %s %s\n", action.Kind, action.Target)
		} else {
			fmt.Fprintf(&b, "- %s %s: %s\n", action.Kind, action.Target, action.Details)
		}
	}
	if len(p.Actions) == 0 {
		b.WriteString("- no changes\n")
	}
	for _, note := range p.Notes {
		fmt.Fprintf(&b, "%s\n", note)
	}
	return b.String()
}

func DeploymentPlan(cfg *config.Config, envName string) (Plan, error) {
	resolved, env, err := cfg.ResolveEnvironment(envName)
	if err != nil {
		return Plan{}, err
	}
	cfg = resolved
	placements, err := scheduler.PlaceServices(cfg, env)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{Environment: envName}
	serviceNames := make([]string, 0, len(cfg.Services))
	for name := range cfg.Services {
		serviceNames = append(serviceNames, name)
	}
	sort.Strings(serviceNames)
	for _, name := range serviceNames {
		svc := cfg.Services[name]
		if svc.Image.Ref != "" {
			plan.Actions = append(plan.Actions, Action{Kind: "resolve", Target: name, Details: svc.Image.Ref + " -> immutable digest"})
			continue
		}
		image := imageDescription(cfg.Registry, name, svc)
		plan.Actions = append(plan.Actions, Action{Kind: "build", Target: name, Details: image})
		plan.Actions = append(plan.Actions, Action{Kind: "push", Target: name, Details: cfg.Registry})
		plan.Actions = append(plan.Actions, Action{Kind: "resolve", Target: name, Details: "pushed image -> immutable digest"})
	}
	// Actions are listed in the order `ship deploy` executes them: images,
	// accessories, release commands, then rollout (start) and ingress.
	accessoryNames := make([]string, 0, len(cfg.Accessories))
	for name := range cfg.Accessories {
		accessoryNames = append(accessoryNames, name)
	}
	sort.Strings(accessoryNames)
	for _, name := range accessoryNames {
		acc := cfg.Accessories[name]
		plan.Actions = append(plan.Actions, Action{Kind: "accessory", Target: name, Details: fmt.Sprintf("ensure %s on pool %s", acc.Image, acc.Pool)})
		if acc.Backup.BackupRequired() {
			plan.Actions = append(plan.Actions, Action{Kind: "backup-check", Target: name, Details: acc.Backup.Command})
		}
	}
	for _, name := range serviceNames {
		release := cfg.Services[name].Release
		command := strings.TrimSpace(release.Command)
		if command == "" {
			continue
		}
		replica := release.Replica
		if replica == 0 {
			replica = 1
		}
		details := fmt.Sprintf("%s (replica %d", command, replica)
		if release.TimeoutSeconds > 0 {
			details += fmt.Sprintf(", timeout %ds", release.TimeoutSeconds)
		}
		details += ")"
		plan.Actions = append(plan.Actions, Action{Kind: "release", Target: name, Details: details})
	}
	for _, placement := range placements {
		plan.Actions = append(plan.Actions, Action{
			Kind:    "start",
			Target:  fmt.Sprintf("%s.%d", placement.Service, placement.Replica),
			Details: fmt.Sprintf("on %s", placement.Host.Name),
		})
	}
	for _, name := range serviceNames {
		svc := cfg.Services[name]
		if svc.Ingress != nil && len(svc.Ingress.Domains) > 0 {
			plan.Actions = append(plan.Actions, Action{Kind: "ingress", Target: name, Details: strings.Join(svc.Ingress.Domains, ", ")})
		}
	}
	return plan, nil
}

// Provision action kinds.
const (
	// ProvisionCreate: apply creates a new server (this costs money).
	ProvisionCreate = "create"
	// ProvisionExists: a server for the host already exists; apply reuses it
	// and re-runs the idempotent bootstrap.
	ProvisionExists = "exists"
	// ProvisionPinned: the host is pinned by address but is not in local host
	// facts. Apply never creates a pinned host; it fails if the provider has
	// no server for it.
	ProvisionPinned = "pinned"
	// ProvisionConflict: a pin disagrees with known servers. Apply refuses to
	// run until the pin is fixed.
	ProvisionConflict = "conflict"
	// ProvisionExtra: a known server that is not in the config. Apply leaves
	// it running; decommission is explicit.
	ProvisionExtra = "extra"
)

// KnownHost is a server Ship already knows about, from local host facts or
// from a read-only provider listing.
type KnownHost struct {
	Name       string
	Pool       string
	Address    string
	ProviderID string
}

// ProvisionInput is everything ProvisionPlan needs. It never talks to a
// provider itself.
type ProvisionInput struct {
	// Desired are the hosts the config asks for.
	Desired []provider.HostPlan
	// Known are the servers that already exist.
	Known []KnownHost
	// Source names where Known came from, such as "local host facts".
	Source string
	// SourceNote, when set, replaces Source in the closing plan note.
	SourceNote string
	// Authoritative is true when Known came from the provider itself rather
	// than from local host facts.
	Authoritative bool
	// CreatesServers is false for inventory providers that never create
	// servers (manual, ssh_config, terraform, pulumi, ansible).
	CreatesServers bool
	// Provider is the provider name, for inventory provider details.
	Provider string
}

// ProvisionPlan shows, per configured host, whether `ship provision apply`
// would create a server, reuse an existing one, or refuse because of a pin.
func ProvisionPlan(envName string, in ProvisionInput) Plan {
	plan := Plan{Environment: envName, Summary: map[string]int{}}
	if !in.CreatesServers {
		for _, desired := range in.Desired {
			plan.add(Action{Kind: ProvisionExists, Target: desired.Name, Details: fmt.Sprintf("pool=%s inventory=%s", desired.Pool, in.Provider)})
		}
		plan.Notes = append(plan.Notes, fmt.Sprintf("provider %s uses existing hosts; apply never creates servers", in.Provider))
		return plan
	}

	// The first known server for a logical name wins; later duplicates (for
	// example an old server kept by `ship migrate --keep-server`) are extra.
	known := map[string]int{}
	byAddress := map[string]KnownHost{}
	for i, host := range in.Known {
		if _, ok := known[host.Name]; !ok {
			known[host.Name] = i
		}
		if address, ok := addressKey(host.Address); ok {
			if _, taken := byAddress[address]; !taken {
				byAddress[address] = host
			}
		}
	}
	used := map[int]bool{}
	var creates []string
	for _, desired := range in.Desired {
		pin := strings.TrimSpace(desired.PinnedAddress)
		index, found := known[desired.Name]
		switch {
		case found:
			used[index] = true
			host := in.Known[index]
			if pin != "" {
				address := strings.TrimSpace(host.Address)
				switch {
				case address != "" && ipaddr.Is(address) && !ipaddr.Equal(address, pin):
					plan.add(Action{Kind: ProvisionConflict, Target: desired.Name, Details: fmt.Sprintf("pool=%s pinned to %s but %s reports %s; apply refuses until the pin matches", desired.Pool, pin, in.Source, host.Address)})
					continue
				case address == "" || !ipaddr.Is(address):
					reported := "no address"
					if address != "" {
						reported = fmt.Sprintf("address %q, which is not an IP address", address)
					}
					if in.Authoritative {
						plan.add(Action{Kind: ProvisionConflict, Target: desired.Name, Details: fmt.Sprintf("pool=%s pinned to %s but %s reports %s, so the pin cannot be verified; apply refuses", desired.Pool, pin, in.Source, reported)})
						continue
					}
					plan.add(Action{Kind: ProvisionPinned, Target: desired.Name, Details: fmt.Sprintf("pool=%s address=%s (%s has %s for it; apply verifies the pin against the provider and refuses if it cannot)", desired.Pool, pin, in.Source, reported)})
					continue
				}
			}
			plan.add(Action{Kind: ProvisionExists, Target: desired.Name, Details: knownDetails(desired.Pool, host)})
		case pin != "":
			if other, ok := lookupAddress(byAddress, pin); ok {
				plan.add(Action{Kind: ProvisionConflict, Target: desired.Name, Details: fmt.Sprintf("pool=%s pinned to %s, which belongs to host %s; pin that host name instead", desired.Pool, pin, other.Name)})
				continue
			}
			if in.Authoritative {
				plan.add(Action{Kind: ProvisionConflict, Target: desired.Name, Details: fmt.Sprintf("pool=%s pinned to %s but %s has no server for it; apply refuses and never creates a pinned host", desired.Pool, pin, in.Source)})
				continue
			}
			plan.add(Action{Kind: ProvisionPinned, Target: desired.Name, Details: fmt.Sprintf("pool=%s address=%s (not in %s; apply never creates a pinned host)", desired.Pool, pin, in.Source)})
		default:
			if other, ok := lookupAddress(byAddress, desired.Name); ok {
				// hosts: [<ip>] for a provider-created server.
				plan.add(Action{Kind: ProvisionConflict, Target: desired.Name, Details: fmt.Sprintf("pool=%s is the address of existing host %s; list host %s and pin it with hosts.pools.%s.pin.%s: %s instead (apply refuses to create a second server)", desired.Pool, other.Name, other.Name, desired.Pool, other.Name, desired.Name)})
				used[known[other.Name]] = true
				continue
			}
			if ipaddr.Is(desired.Name) {
				// Apply never creates a server named after an address, whether
				// or not a known server has that address.
				plan.add(Action{Kind: ProvisionConflict, Target: desired.Name, Details: fmt.Sprintf("pool=%s is an IP address; apply refuses to create a server named after it (for an existing server, list its host name and set hosts.pools.%s.pin.<host>: %s)", desired.Pool, desired.Pool, desired.Name)})
				continue
			}
			plan.add(Action{Kind: ProvisionCreate, Target: desired.Name, Details: fmt.Sprintf("pool=%s new server", desired.Pool)})
			creates = append(creates, desired.Name)
		}
	}
	var extras []KnownHost
	for i, host := range in.Known {
		if !used[i] {
			extras = append(extras, host)
		}
	}
	sort.Slice(extras, func(i, j int) bool {
		if extras[i].Pool != extras[j].Pool {
			return extras[i].Pool < extras[j].Pool
		}
		return extras[i].Name < extras[j].Name
	})
	for _, host := range extras {
		plan.add(Action{Kind: ProvisionExtra, Target: host.Name, Details: knownDetails(host.Pool, host) + " not in config; apply leaves it running"})
	}

	conflicts := plan.Summary[ProvisionConflict] > 0
	if in.Authoritative {
		switch {
		case conflicts:
			plan.Notes = append(plan.Notes, "apply will refuse to run and will not create any servers until the conflicts above are fixed in ship.yml")
		case len(creates) == 0:
			plan.Notes = append(plan.Notes, "apply will not create any servers")
		default:
			plan.Notes = append(plan.Notes, "apply will create "+serverCount(creates))
		}
	} else {
		// Local host facts can be stale: a server may have been deleted or
		// re-addressed at the provider. Never promise what apply will do.
		source := in.Source
		if source == "" {
			source = "local host facts"
		}
		switch {
		case conflicts:
			plan.Notes = append(plan.Notes, fmt.Sprintf("based on %s, apply would refuse to run because of the conflicts above; apply re-checks pins and host names against the provider before changing anything and creates nothing while any conflict remains there (run with --refresh to see the provider's view)", source))
		case len(creates) == 0:
			plan.Notes = append(plan.Notes, fmt.Sprintf("based on %s, apply would not create any servers; not confirmed against the provider, so run with --refresh before apply", source))
		default:
			plan.Notes = append(plan.Notes, fmt.Sprintf("based on %s, apply would create %s; run with --refresh to confirm against the provider", source, serverCount(creates)))
		}
	}
	if in.SourceNote != "" {
		plan.Notes = append(plan.Notes, "existing servers from "+in.SourceNote)
	} else if in.Source != "" {
		plan.Notes = append(plan.Notes, "existing servers from "+in.Source)
	}
	return plan
}

func serverCount(names []string) string {
	if len(names) == 1 {
		return "1 server: " + names[0]
	}
	return fmt.Sprintf("%d servers: %s", len(names), strings.Join(names, ", "))
}

// addressKey canonicalizes an IP address so equivalent spellings match.
func addressKey(value string) (string, bool) {
	return ipaddr.Canonical(value)
}

func lookupAddress(byAddress map[string]KnownHost, value string) (KnownHost, bool) {
	key, ok := addressKey(value)
	if !ok {
		return KnownHost{}, false
	}
	host, ok := byAddress[key]
	return host, ok
}

func (p *Plan) add(action Action) {
	p.Actions = append(p.Actions, action)
	if p.Summary != nil {
		p.Summary[action.Kind]++
	}
}

func knownDetails(pool string, host KnownHost) string {
	details := "pool=" + pool
	if host.ProviderID != "" {
		details += " provider_id=" + host.ProviderID
	}
	if host.Address != "" {
		details += " address=" + host.Address
	}
	return details
}

func imageDescription(registry, serviceName string, svc config.Service) string {
	if svc.Image.Ref != "" {
		return svc.Image.Ref
	}
	dockerfile := svc.Image.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	details := fmt.Sprintf("%s:%s-<release> from %s/%s", registry, serviceName, svc.Image.Build, dockerfile)
	if svc.Image.Target != "" {
		details += " target=" + svc.Image.Target
	}
	if svc.Image.Platform != "" {
		details += " platform=" + svc.Image.Platform
	}
	return details
}
