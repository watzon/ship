package planner

import (
	"fmt"
	"sort"
	"strings"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/scheduler"
)

type Action struct {
	Kind    string
	Target  string
	Details string
}

type Plan struct {
	Environment string
	Actions     []Action
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
	return b.String()
}

func DeploymentPlan(cfg *config.Config, envName string) (Plan, error) {
	env, err := cfg.Environment(envName)
	if err != nil {
		return Plan{}, err
	}
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
	accessoryNames := make([]string, 0, len(cfg.Accessories))
	for name := range cfg.Accessories {
		accessoryNames = append(accessoryNames, name)
	}
	sort.Strings(accessoryNames)
	for _, name := range accessoryNames {
		acc := cfg.Accessories[name]
		plan.Actions = append(plan.Actions, Action{Kind: "accessory", Target: name, Details: fmt.Sprintf("ensure %s on pool %s", acc.Image, acc.Pool)})
		if acc.Backup.Required {
			plan.Actions = append(plan.Actions, Action{Kind: "backup-check", Target: name, Details: acc.Backup.Command})
		}
	}
	return plan, nil
}

func ProvisionPlan(cfg *config.Config, envName string) (Plan, error) {
	env, err := cfg.Environment(envName)
	if err != nil {
		return Plan{}, err
	}
	hosts := scheduler.HostsForEnvironment(env)
	plan := Plan{Environment: envName}
	for _, host := range hosts {
		plan.Actions = append(plan.Actions, Action{Kind: "provision", Target: host.Name, Details: fmt.Sprintf("pool=%s", host.Pool)})
	}
	return plan, nil
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
