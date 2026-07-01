package terraform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

type Provider struct {
	DryRun     bool
	Env        config.Environment
	Run        CommandRunner
	LookupPath func(string) (string, error)
}

type CommandRunner func(ctx context.Context, workDir, binary string, env []string, args ...string) ([]byte, error)

type Host struct {
	ID            string
	Name          string
	Pool          string
	User          string
	SSHPort       int
	PublicAddress string
}

func New(dryRun bool, env config.Environment) Provider {
	return Provider{
		DryRun:     dryRun,
		Env:        env,
		Run:        runCommand,
		LookupPath: exec.LookPath,
	}
}

func (p Provider) Name() string {
	return config.ProviderTerraform
}

func (p Provider) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.Terraform == nil {
		return nil, fmt.Errorf("environment %q must define provider.terraform", environment)
	}
	hosts, err := p.Hosts(context.Background(), project, environment, env)
	if err != nil {
		return nil, err
	}
	plans := make([]provider.HostPlan, 0, len(hosts))
	for _, host := range hosts {
		plans = append(plans, provider.HostPlan{
			Project:     project,
			Environment: environment,
			Name:        host.Name,
			Pool:        host.Pool,
			User:        host.User,
			Location:    "terraform",
			Size:        "existing",
			Image:       "existing",
			Labels:      provider.HostLabels(project, environment, host.Pool, env.Hosts.Labels, poolLabels(env, host.Pool)),
		})
	}
	return plans, nil
}

func (p Provider) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	plans, hosts, err := p.planAndHosts(ctx, project, environment, env)
	if err != nil {
		return provider.ReconcileResult{}, err
	}
	if strings.TrimSpace(project) == "" {
		return provider.ReconcileResult{}, fmt.Errorf("project is required")
	}
	if strings.TrimSpace(environment) == "" {
		return provider.ReconcileResult{}, fmt.Errorf("environment is required")
	}
	return provider.ReconcileResult{
		Desired:  plans,
		Existing: hosts,
	}, nil
}

func (p Provider) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	_, hosts, err := p.planAndHosts(ctx, project, environment, p.Env)
	return hosts, err
}

func (p Provider) Delete(ctx context.Context, host provider.Host) error {
	_ = ctx
	_ = host
	return nil
}

func (p Provider) CredentialChecks(lookupEnv func(string) (string, bool)) []provider.CredentialCheck {
	_ = lookupEnv
	cfg := terraformConfig(p.Env)
	binary := terraformBinary(cfg)
	lookup := p.LookupPath
	if lookup == nil {
		lookup = exec.LookPath
	}
	_, err := lookup(binary)
	present := err == nil
	return []provider.CredentialCheck{{
		Name:           "terraform binary",
		Present:        present,
		Required:       true,
		PresentMessage: binary + " is available",
		MissingMessage: "missing " + binary + " in PATH",
	}}
}

func (p Provider) planAndHosts(ctx context.Context, project, environment string, env config.Environment) ([]provider.HostPlan, []provider.Host, error) {
	if env.Provider.Terraform == nil {
		return nil, nil, fmt.Errorf("environment %q must define provider.terraform", environment)
	}
	terraformHosts, err := p.Hosts(ctx, project, environment, env)
	if err != nil {
		return nil, nil, err
	}
	plans := make([]provider.HostPlan, 0, len(terraformHosts))
	hosts := make([]provider.Host, 0, len(terraformHosts))
	for _, host := range terraformHosts {
		labels := provider.HostLabels(project, environment, host.Pool, env.Hosts.Labels, poolLabels(env, host.Pool))
		plans = append(plans, provider.HostPlan{
			Project:     project,
			Environment: environment,
			Name:        host.Name,
			Pool:        host.Pool,
			User:        host.User,
			Location:    "terraform",
			Size:        "existing",
			Image:       "existing",
			Labels:      labels,
		})
		hosts = append(hosts, provider.Host{
			ID:            firstNonEmpty(host.ID, host.Name),
			Name:          host.Name,
			Pool:          host.Pool,
			PublicAddress: firstNonEmpty(host.PublicAddress, host.Name),
			SSHPort:       host.SSHPort,
			Labels:        labels,
		})
	}
	return plans, hosts, nil
}

func (p Provider) Hosts(ctx context.Context, project, environment string, env config.Environment) ([]Host, error) {
	_ = project
	_ = environment
	cfg := terraformConfig(env)
	if cfg == nil {
		return nil, fmt.Errorf("environment must define provider.terraform")
	}
	run := p.Run
	if run == nil {
		run = runCommand
	}
	binary := terraformBinary(cfg)
	args := []string{}
	if cfg.WorkingDir != "" {
		args = append(args, "-chdir="+cfg.WorkingDir)
	}
	args = append(args, "output", "-json")
	var commandEnv []string
	if cfg.Workspace != "" {
		commandEnv = append(commandEnv, "TF_WORKSPACE="+cfg.Workspace)
	}
	data, err := run(ctx, "", binary, commandEnv, args...)
	if err != nil {
		return nil, err
	}
	output, err := terraformOutputValue(data, cfg.Output)
	if err != nil {
		return nil, err
	}
	hosts, err := parseHosts(output, cfg, env)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(hosts, func(i, j int) bool {
		if hosts[i].Pool != hosts[j].Pool {
			return hosts[i].Pool < hosts[j].Pool
		}
		return hosts[i].Name < hosts[j].Name
	})
	return hosts, nil
}

func terraformConfig(env config.Environment) *config.TerraformConfig {
	return env.Provider.Terraform
}

func terraformBinary(cfg *config.TerraformConfig) string {
	if cfg != nil && cfg.Binary != "" {
		return cfg.Binary
	}
	return "terraform"
}

func terraformOutputValue(data []byte, output string) (json.RawMessage, error) {
	if strings.TrimSpace(output) == "" {
		return nil, fmt.Errorf("terraform output name is required")
	}
	var outputs map[string]struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(data, &outputs); err != nil {
		return nil, fmt.Errorf("decode terraform output -json: %w", err)
	}
	value, ok := outputs[output]
	if !ok {
		return nil, fmt.Errorf("terraform output %q not found", output)
	}
	if len(value.Value) == 0 || bytes.Equal(value.Value, []byte("null")) {
		return nil, fmt.Errorf("terraform output %q is empty", output)
	}
	return value.Value, nil
}

func parseHosts(raw json.RawMessage, cfg *config.TerraformConfig, env config.Environment) ([]Host, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("decode terraform host inventory: %w", err)
	}
	hosts, err := parseHostValue(value, "", cfg, env)
	if err != nil {
		return nil, err
	}
	for i := range hosts {
		if hosts[i].Pool == "" {
			hosts[i].Pool = singlePool(env)
		}
		if hosts[i].Pool == "" {
			return nil, fmt.Errorf("terraform host %q must define pool when more than one pool exists", hosts[i].Name)
		}
		if _, ok := env.Hosts.Pools[hosts[i].Pool]; !ok {
			return nil, fmt.Errorf("terraform host %q references unknown pool %q", hosts[i].Name, hosts[i].Pool)
		}
		if hosts[i].Name == "" {
			hosts[i].Name = hosts[i].PublicAddress
		}
		if hosts[i].PublicAddress == "" {
			hosts[i].PublicAddress = hosts[i].Name
		}
		if hosts[i].Name == "" {
			return nil, fmt.Errorf("terraform host entry must define name or address")
		}
		if hosts[i].User == "" {
			hosts[i].User = firstNonEmpty(env.Hosts.Pools[hosts[i].Pool].User, cfg.User)
		}
	}
	return hosts, nil
}

func parseHostValue(value any, pool string, cfg *config.TerraformConfig, env config.Environment) ([]Host, error) {
	switch typed := value.(type) {
	case []any:
		hosts := make([]Host, 0, len(typed))
		for _, item := range typed {
			itemHosts, err := parseHostValue(item, pool, cfg, env)
			if err != nil {
				return nil, err
			}
			hosts = append(hosts, itemHosts...)
		}
		return hosts, nil
	case map[string]any:
		if looksLikeHostObject(typed) {
			return []Host{hostFromObject(typed, pool)}, nil
		}
		var hosts []Host
		keys := sortedKeys(typed)
		for _, key := range keys {
			itemHosts, err := parseHostValue(typed[key], key, cfg, env)
			if err != nil {
				return nil, err
			}
			hosts = append(hosts, itemHosts...)
		}
		return hosts, nil
	case string:
		return []Host{{Name: typed, Pool: pool, PublicAddress: typed}}, nil
	default:
		return nil, fmt.Errorf("terraform host inventory must be a string, object, list, or pool map")
	}
}

func looksLikeHostObject(value map[string]any) bool {
	for _, key := range []string{"address", "public_address", "ip", "host", "hostname", "name"} {
		if _, ok := value[key]; ok {
			return true
		}
	}
	return false
}

func hostFromObject(value map[string]any, pool string) Host {
	hostPool := stringValue(value, "pool")
	if hostPool == "" {
		hostPool = pool
	}
	address := firstNonEmpty(
		stringValue(value, "address"),
		stringValue(value, "public_address"),
		stringValue(value, "ip"),
		stringValue(value, "host"),
		stringValue(value, "hostname"),
	)
	name := firstNonEmpty(stringValue(value, "name"), stringValue(value, "hostname"), address)
	return Host{
		ID:            stringValue(value, "id"),
		Name:          name,
		Pool:          hostPool,
		User:          stringValue(value, "user"),
		SSHPort:       intValue(value, "port"),
		PublicAddress: address,
	}
}

func intValue(value map[string]any, key string) int {
	raw, ok := value[key]
	if !ok || raw == nil {
		return 0
	}
	switch typed := raw.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case string:
		var out int
		if _, err := fmt.Sscanf(strings.TrimSpace(typed), "%d", &out); err == nil {
			return out
		}
	}
	return 0
}

func stringValue(value map[string]any, key string) string {
	raw, ok := value[key]
	if !ok || raw == nil {
		return ""
	}
	switch typed := raw.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return fmt.Sprint(typed)
	}
}

func sortedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func singlePool(env config.Environment) string {
	if len(env.Hosts.Pools) != 1 {
		return ""
	}
	for name := range env.Hosts.Pools {
		return name
	}
	return ""
}

func poolLabels(env config.Environment, pool string) map[string]string {
	if env.Hosts.Pools == nil {
		return nil
	}
	return env.Hosts.Pools[pool].Labels
}

func runCommand(ctx context.Context, workDir, binary string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = workDir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			message := strings.TrimSpace(string(exitErr.Stderr))
			if message != "" {
				return nil, fmt.Errorf("%s %s failed: %s", binary, strings.Join(args, " "), message)
			}
		}
		return nil, fmt.Errorf("%s %s failed: %w", binary, strings.Join(args, " "), err)
	}
	return out, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
