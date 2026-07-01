package provider

import (
	"context"
	"fmt"
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
	existingByName := map[string]Host{}
	for _, host := range existing {
		existingByName[host.Name] = host
	}

	desiredNames := map[string]bool{}
	for _, plan := range desired {
		desiredNames[plan.Name] = true
		if host, ok := existingByName[plan.Name]; ok {
			result.Existing = append(result.Existing, host)
			continue
		}
		host, err := backend.Create(ctx, plan)
		if err != nil {
			return ReconcileResult{}, err
		}
		result.Created = append(result.Created, host)
	}

	for _, host := range existing {
		if !desiredNames[host.Name] {
			result.Extra = append(result.Extra, host)
		}
	}

	sortHosts(result.Existing)
	sortHosts(result.Created)
	sortHosts(result.Extra)
	return result, nil
}

func sortHosts(hosts []Host) {
	sort.SliceStable(hosts, func(i, j int) bool {
		if hosts[i].Pool != hosts[j].Pool {
			return hosts[i].Pool < hosts[j].Pool
		}
		return hosts[i].Name < hosts[j].Name
	})
}
