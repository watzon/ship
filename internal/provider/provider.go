package provider

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/scheduler"
)

const (
	LabelManagedBy   = "managed-by"
	LabelProject     = "project"
	LabelEnvironment = "environment"
	LabelPool        = "pool"
	// LabelHost records the logical host name a server fulfills. It lets a
	// replacement server created by `ship migrate` keep a unique provider-level
	// name while still reconciling against its logical host.
	LabelHost = "host"
)

type HostPlan struct {
	Project     string
	Environment string
	Name        string
	Pool        string
	User        string
	Location    string
	Size        string
	Image       string
	UserData    string
	Labels      map[string]string
	// PinnedAddress is set when the host is pinned by address in
	// hosts.pools.<pool>.pin. Reconciliation never creates a pinned host.
	PinnedAddress string
}

type Host struct {
	ID             string
	Name           string
	Pool           string
	PublicAddress  string
	SSHPort        int
	IdentityFile   string
	KnownHostsFile string
	JumpHost       string
	SSHOptions     map[string]string
	Labels         map[string]string
	NetworkIDs     []int64
	FirewallIDs    []int64
}

type ReconcileResult struct {
	Desired  []HostPlan
	Existing []Host
	Created  []Host
	Extra    []Host
}

type CredentialCheck struct {
	Name           string
	Present        bool
	Required       bool
	PresentMessage string
	MissingMessage string
}

type Provider interface {
	Name() string
	PlanHosts(project, environment string, env config.Environment) ([]HostPlan, error)
	Reconcile(ctx context.Context, project, environment string, env config.Environment) (ReconcileResult, error)
	List(ctx context.Context, project, environment string) ([]Host, error)
	Delete(ctx context.Context, host Host) error
	CredentialChecks(lookupEnv func(string) (string, bool)) []CredentialCheck
}

// HostCreator is an optional capability for providers that can create a single
// server outside full reconciliation. `ship migrate` uses it to provision a
// replacement server while the server it replaces still exists.
type HostCreator interface {
	CreateHost(ctx context.Context, project, environment string, env config.Environment, plan HostPlan) (Host, error)
}

// LogicalName returns the logical host name a provider server fulfills: the
// LabelHost label when present, otherwise the server name.
func LogicalName(host Host) string {
	if name := strings.TrimSpace(host.Labels[LabelHost]); name != "" {
		return name
	}
	return host.Name
}

type HostPlanOptions struct {
	Location string
	Size     string
	Image    string
	UserData string
}

type ReconcileBackend interface {
	List(ctx context.Context, project, environment string) ([]Host, error)
	Create(ctx context.Context, plan HostPlan) (Host, error)
}

func HostPlans(project, environment string, env config.Environment, opts HostPlanOptions) []HostPlan {
	var plans []HostPlan
	for _, host := range scheduler.HostsForEnvironment(env) {
		pool := env.Hosts.Pools[host.Pool]
		hostOpts := opts
		if pool.Location != "" {
			hostOpts.Location = pool.Location
		}
		if pool.Size != "" {
			hostOpts.Size = pool.Size
		}
		if pool.Image != "" {
			hostOpts.Image = pool.Image
		}
		if pool.UserData != "" {
			hostOpts.UserData = pool.UserData
		}
		labels := HostLabels(project, environment, host.Pool, env.Hosts.Labels, pool.Labels)
		labels[LabelHost] = host.Name
		plans = append(plans, HostPlan{
			Project:     project,
			Environment: environment,
			Name:        host.Name,
			Pool:        host.Pool,
			User:        host.User,
			Location:    hostOpts.Location,
			Size:        hostOpts.Size,
			Image:       hostOpts.Image,
			UserData:    hostOpts.UserData,
			Labels:      labels,

			PinnedAddress: host.PinnedAddress,
		})
	}
	return plans
}

func HostLabels(project, environment, pool string, groups ...map[string]string) map[string]string {
	labels := map[string]string{}
	for _, group := range groups {
		for key, value := range group {
			if key == "" || value == "" {
				continue
			}
			labels[key] = value
		}
	}
	for key, value := range ShipLabels(project, environment, pool) {
		labels[key] = value
	}
	return labels
}

func ShipLabels(project, environment, pool string) map[string]string {
	labels := map[string]string{
		LabelManagedBy: "ship",
		LabelPool:      pool,
	}
	if project != "" {
		labels[LabelProject] = project
	}
	if environment != "" {
		labels[LabelEnvironment] = environment
	}
	return labels
}

func ReconcileHosts(ctx context.Context, project, environment string, desired []HostPlan, backend ReconcileBackend) (ReconcileResult, error) {
	if strings.TrimSpace(project) == "" {
		return ReconcileResult{}, fmt.Errorf("project is required")
	}
	if strings.TrimSpace(environment) == "" {
		return ReconcileResult{}, fmt.Errorf("environment is required")
	}
	result := ReconcileResult{Desired: desired}
	existing, err := backend.List(ctx, project, environment)
	if err != nil {
		return ReconcileResult{}, err
	}
	existingByLogical := map[string]int{}
	for i, host := range existing {
		logical := LogicalName(host)
		if j, ok := existingByLogical[logical]; ok {
			// Two servers claim the same logical host (for example an old
			// server plus its migrate replacement). Prefer the one whose
			// provider name matches the logical name so reconciliation stays
			// stable; the other is reported as extra.
			if existing[j].Name == logical {
				continue
			}
		}
		existingByLogical[logical] = i
	}

	if err := CheckBeforeCreate(environment, desired, existing); err != nil {
		return ReconcileResult{}, err
	}

	matched := map[int]bool{}
	for _, plan := range desired {
		if i, ok := existingByLogical[plan.Name]; ok {
			result.Existing = append(result.Existing, existing[i])
			matched[i] = true
			continue
		}
		host, err := backend.Create(ctx, plan)
		if err != nil {
			return ReconcileResult{}, err
		}
		result.Created = append(result.Created, host)
	}

	for i, host := range existing {
		if !matched[i] {
			result.Extra = append(result.Extra, host)
		}
	}

	sortHosts(result.Existing)
	sortHosts(result.Created)
	sortHosts(result.Extra)
	return result, nil
}

// HostLister lists the servers a provider already has for an environment. It
// must be read-only.
type HostLister interface {
	List(ctx context.Context, project, environment string) ([]Host, error)
}

// PreflightHosts lists the provider's servers (read-only) and runs
// CheckBeforeCreate against them. Providers call it from Reconcile before any
// provider-side setup (networks, firewalls, tags, keys), so a bad pin or host
// name leaves nothing behind. ReconcileHosts checks again right before it
// creates servers.
func PreflightHosts(ctx context.Context, lister HostLister, project, environment string, desired []HostPlan) error {
	existing, err := lister.List(ctx, project, environment)
	if err != nil {
		return err
	}
	if problems := checkHosts(desired, existing); len(problems) > 0 {
		return fmt.Errorf("configured hosts for %s do not match provider servers: %s; Ship never creates or replaces a pinned host, and nothing was changed (checked with a read-only server listing before any provider changes)", environment, strings.Join(problems, "; "))
	}
	return nil
}

// CheckBeforeCreate verifies the desired hosts against the servers the
// provider reports, before reconciliation creates anything:
//
//   - A pinned host must already exist at its pinned address. It is an error
//     for it to be missing (reconciliation would otherwise create a new server),
//     to be reported at a different address, or to be reported without an
//     address that can be compared with the pin (the pin cannot be verified,
//     so Ship fails closed).
//   - An unpinned host whose name is an IP address must already exist under
//     that name. Creating a server named after an address is always a config
//     mistake (hosts: [<ip>] for a provider-created server), whether or not
//     the provider still reports a server at that address.
//
// Any problem refuses the whole apply: no server is created, including hosts
// that are fine on their own.
func CheckBeforeCreate(environment string, desired []HostPlan, existing []Host) error {
	problems := checkHosts(desired, existing)
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("configured hosts for %s do not match provider servers: %s; Ship never creates or replaces a pinned host, and no servers were created", environment, strings.Join(problems, "; "))
}

func checkHosts(desired []HostPlan, existing []Host) []string {
	byLogical := map[string]Host{}
	for _, host := range existing {
		logical := LogicalName(host)
		if current, ok := byLogical[logical]; ok && current.Name == logical {
			continue
		}
		byLogical[logical] = host
	}
	serverAt := func(address string) (Host, bool) {
		for _, other := range existing {
			if SameAddress(other.PublicAddress, address) {
				return other, true
			}
		}
		return Host{}, false
	}
	var problems []string
	for _, plan := range desired {
		pin := strings.TrimSpace(plan.PinnedAddress)
		host, ok := byLogical[plan.Name]
		if pin == "" {
			if ok || !IsIPAddress(plan.Name) {
				continue
			}
			if other, found := serverAt(plan.Name); found {
				problems = append(problems, fmt.Sprintf("host %s pool=%s is the address of existing server %s (host %s), and apply would create a second server for it; list host %s in the pool and set hosts.pools.%s.pin.%s: %s instead", plan.Name, plan.Pool, other.Name, LogicalName(other), LogicalName(other), plan.Pool, LogicalName(other), plan.Name))
				continue
			}
			problems = append(problems, fmt.Sprintf("host %s pool=%s is an IP address, and apply would create a new server named after it; if it is an existing server, list its host name in the pool and set hosts.pools.%s.pin.<host>: %s instead", plan.Name, plan.Pool, plan.Pool, plan.Name))
			continue
		}
		if !ok {
			problem := fmt.Sprintf("host %s pool=%s is pinned to %s but the provider reports no server for it", plan.Name, plan.Pool, pin)
			if other, found := serverAt(pin); found {
				problem += fmt.Sprintf("; %s belongs to server %s (host %s), so pin that host name instead", pin, other.Name, LogicalName(other))
			}
			problems = append(problems, problem)
			continue
		}
		address := strings.TrimSpace(host.PublicAddress)
		switch {
		case address == "":
			problems = append(problems, fmt.Sprintf("host %s pool=%s is pinned to %s but the provider reports no address for it, so the pin cannot be verified", plan.Name, plan.Pool, pin))
		case !IsIPAddress(address):
			problems = append(problems, fmt.Sprintf("host %s pool=%s is pinned to %s but the provider reports address %q, which is not an IP address, so the pin cannot be verified", plan.Name, plan.Pool, pin, address))
		case !SameAddress(address, pin):
			problems = append(problems, fmt.Sprintf("host %s pool=%s is pinned to %s but the provider reports %s; update hosts.pools.%s.pin.%s in ship.yml", plan.Name, plan.Pool, pin, address, plan.Pool, plan.Name))
		}
	}
	sort.Strings(problems)
	return problems
}

// IsIPAddress reports whether value is an IPv4 or IPv6 address.
func IsIPAddress(value string) bool {
	_, err := netip.ParseAddr(strings.TrimSpace(value))
	return err == nil
}

// SameAddress reports whether a and b are the same IP address, comparing
// parsed addresses so equivalent forms (such as IPv6 spellings) match. Values
// that are not IP addresses never match.
func SameAddress(a, b string) bool {
	left, err := netip.ParseAddr(strings.TrimSpace(a))
	if err != nil {
		return false
	}
	right, err := netip.ParseAddr(strings.TrimSpace(b))
	if err != nil {
		return false
	}
	return left.Unmap() == right.Unmap()
}

func sortHosts(hosts []Host) {
	sort.SliceStable(hosts, func(i, j int) bool {
		if hosts[i].Pool != hosts[j].Pool {
			return hosts[i].Pool < hosts[j].Pool
		}
		return hosts[i].Name < hosts[j].Name
	})
}
