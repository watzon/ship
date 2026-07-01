package manual

import (
	"context"
	"fmt"
	"strings"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

type Provider struct {
	DryRun bool
	Env    config.Environment
}

func New(dryRun bool, env config.Environment) Provider {
	return Provider{DryRun: dryRun, Env: env}
}

func (p Provider) Name() string {
	return config.ProviderManual
}

func (p Provider) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.Manual == nil {
		return nil, fmt.Errorf("environment %q must define provider.manual", environment)
	}
	return provider.HostPlans(project, environment, env, provider.HostPlanOptions{
		Location: "manual",
		Size:     "existing",
		Image:    "existing",
	}), nil
}

func (p Provider) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	_ = ctx
	plans, err := p.PlanHosts(project, environment, env)
	if err != nil {
		return provider.ReconcileResult{}, err
	}
	if strings.TrimSpace(project) == "" {
		return provider.ReconcileResult{}, fmt.Errorf("project is required")
	}
	if strings.TrimSpace(environment) == "" {
		return provider.ReconcileResult{}, fmt.Errorf("environment is required")
	}
	hosts := hostsFromPlans(plans)
	return provider.ReconcileResult{
		Desired:  plans,
		Existing: hosts,
	}, nil
}

func (p Provider) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	_ = ctx
	if p.Env.Provider.Manual == nil {
		return nil, fmt.Errorf("environment %q must define provider.manual", environment)
	}
	return HostsForEnvironment(project, environment, p.Env), nil
}

func (p Provider) Delete(ctx context.Context, host provider.Host) error {
	_ = ctx
	_ = host
	return nil
}

func (p Provider) CredentialChecks(lookupEnv func(string) (string, bool)) []provider.CredentialCheck {
	_ = lookupEnv
	return []provider.CredentialCheck{{
		Name:           "manual provider",
		Present:        true,
		Required:       false,
		PresentMessage: "using existing SSH hosts",
		MissingMessage: "using existing SSH hosts",
	}}
}

func HostsForEnvironment(project, environment string, env config.Environment) []provider.Host {
	return hostsFromPlans(provider.HostPlans(project, environment, env, provider.HostPlanOptions{
		Location: "manual",
		Size:     "existing",
		Image:    "existing",
	}))
}

func hostsFromPlans(plans []provider.HostPlan) []provider.Host {
	hosts := make([]provider.Host, 0, len(plans))
	for _, plan := range plans {
		hosts = append(hosts, provider.Host{
			ID:            plan.Name,
			Name:          plan.Name,
			Pool:          plan.Pool,
			PublicAddress: plan.Name,
			Labels:        plan.Labels,
		})
	}
	return hosts
}
