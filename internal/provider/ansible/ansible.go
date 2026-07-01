package ansible

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
	"gopkg.in/yaml.v3"
)

type Provider struct {
	DryRun     bool
	Env        config.Environment
	Run        CommandRunner
	ReadFile   func(string) ([]byte, error)
	LookupPath func(string) (string, error)
}

type CommandRunner func(ctx context.Context, binary string, args ...string) ([]byte, error)

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
		ReadFile:   os.ReadFile,
		LookupPath: exec.LookPath,
	}
}

func (p Provider) Name() string {
	return config.ProviderAnsible
}

func (p Provider) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.Ansible == nil {
		return nil, fmt.Errorf("environment %q must define provider.ansible", environment)
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
			Location:    "ansible",
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
	cfg := ansibleConfig(p.Env)
	if cfg == nil || len(cfg.Command) == 0 {
		return []provider.CredentialCheck{{
			Name:           "ansible inventory",
			Present:        true,
			Required:       false,
			PresentMessage: "using inventory_file",
			MissingMessage: "using inventory_file",
		}}
	}
	lookup := p.LookupPath
	if lookup == nil {
		lookup = exec.LookPath
	}
	_, err := lookup(cfg.Command[0])
	present := err == nil
	return []provider.CredentialCheck{{
		Name:           "ansible inventory command",
		Present:        present,
		Required:       true,
		PresentMessage: cfg.Command[0] + " is available",
		MissingMessage: "missing " + cfg.Command[0] + " in PATH",
	}}
}

func (p Provider) planAndHosts(ctx context.Context, project, environment string, env config.Environment) ([]provider.HostPlan, []provider.Host, error) {
	if env.Provider.Ansible == nil {
		return nil, nil, fmt.Errorf("environment %q must define provider.ansible", environment)
	}
	ansibleHosts, err := p.Hosts(ctx, project, environment, env)
	if err != nil {
		return nil, nil, err
	}
	plans := make([]provider.HostPlan, 0, len(ansibleHosts))
	hosts := make([]provider.Host, 0, len(ansibleHosts))
	for _, host := range ansibleHosts {
		labels := provider.HostLabels(project, environment, host.Pool, env.Hosts.Labels, poolLabels(env, host.Pool))
		plans = append(plans, provider.HostPlan{
			Project:     project,
			Environment: environment,
			Name:        host.Name,
			Pool:        host.Pool,
			User:        host.User,
			Location:    "ansible",
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
	cfg := ansibleConfig(env)
	if cfg == nil {
		return nil, fmt.Errorf("environment must define provider.ansible")
	}
	data, err := p.inventoryData(ctx, cfg)
	if err != nil {
		return nil, err
	}
	inventory, err := parseInventory(data)
	if err != nil {
		return nil, err
	}
	hosts, err := hostsForEnvironment(inventory, cfg, env)
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

func (p Provider) inventoryData(ctx context.Context, cfg *config.AnsibleConfig) ([]byte, error) {
	if cfg.InventoryFile != "" {
		readFile := p.ReadFile
		if readFile == nil {
			readFile = os.ReadFile
		}
		data, err := readFile(cfg.InventoryFile)
		if err != nil {
			return nil, fmt.Errorf("read ansible inventory_file: %w", err)
		}
		return data, nil
	}
	if len(cfg.Command) == 0 {
		return nil, fmt.Errorf("ansible provider requires inventory_file or command")
	}
	run := p.Run
	if run == nil {
		run = runCommand
	}
	return run(ctx, cfg.Command[0], cfg.Command[1:]...)
}

func ansibleConfig(env config.Environment) *config.AnsibleConfig {
	return env.Provider.Ansible
}

type inventory struct {
	Groups   map[string]inventoryGroup
	HostVars map[string]map[string]any
}

type inventoryGroup struct {
	Hosts    map[string]map[string]any
	Children []string
}

func parseInventory(data []byte) (inventory, error) {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return inventory{}, fmt.Errorf("decode ansible inventory: %w", err)
	}
	inv := inventory{
		Groups:   map[string]inventoryGroup{},
		HostVars: map[string]map[string]any{},
	}
	for name, value := range raw {
		if name == "_meta" {
			inv.HostVars = parseMetaHostVars(value)
			continue
		}
		group, err := parseGroup(value)
		if err != nil {
			return inventory{}, fmt.Errorf("decode ansible group %q: %w", name, err)
		}
		inv.Groups[name] = group
		if err := parseNestedChildren(value, inv.Groups); err != nil {
			return inventory{}, fmt.Errorf("decode ansible group %q children: %w", name, err)
		}
	}
	return inv, nil
}

func parseMetaHostVars(value any) map[string]map[string]any {
	meta, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	hostvars, ok := meta["hostvars"].(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]map[string]any{}
	for host, vars := range hostvars {
		out[host] = mapValue(vars)
	}
	return out
}

func parseGroup(value any) (inventoryGroup, error) {
	group := inventoryGroup{Hosts: map[string]map[string]any{}}
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			name := stringValue(item)
			if name != "" {
				group.Hosts[name] = map[string]any{}
			}
		}
	case map[string]any:
		group.Hosts = parseHosts(typed["hosts"])
		group.Children = parseChildren(typed["children"])
	default:
		name := stringValue(typed)
		if name != "" {
			group.Hosts[name] = map[string]any{}
		}
	}
	return group, nil
}

func parseNestedChildren(value any, groups map[string]inventoryGroup) error {
	mapped, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	children, ok := mapped["children"].(map[string]any)
	if !ok {
		return nil
	}
	for name, childValue := range children {
		group, err := parseGroup(childValue)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		groups[name] = group
		if err := parseNestedChildren(childValue, groups); err != nil {
			return err
		}
	}
	return nil
}

func parseHosts(value any) map[string]map[string]any {
	hosts := map[string]map[string]any{}
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			name := stringValue(item)
			if name != "" {
				hosts[name] = map[string]any{}
			}
		}
	case map[string]any:
		for name, vars := range typed {
			hosts[name] = mapValue(vars)
		}
	}
	return hosts
}

func parseChildren(value any) []string {
	var children []string
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			name := stringValue(item)
			if name != "" {
				children = append(children, name)
			}
		}
	case map[string]any:
		for name := range typed {
			children = append(children, name)
		}
	}
	sort.Strings(children)
	return children
}

func hostsForEnvironment(inv inventory, cfg *config.AnsibleConfig, env config.Environment) ([]Host, error) {
	var hosts []Host
	poolNames := sortedPoolNames(env)
	if len(poolNames) == 1 {
		pool := poolNames[0]
		groupName := pool
		if _, ok := inv.Groups[groupName]; !ok {
			groupName = "all"
		}
		poolHosts := hostsForGroup(inv, groupName, pool, cfg, env)
		if len(poolHosts) == 0 && groupName != "all" {
			poolHosts = hostsForGroup(inv, "all", pool, cfg, env)
		}
		return poolHosts, nil
	}
	for _, pool := range poolNames {
		if _, ok := inv.Groups[pool]; !ok {
			return nil, fmt.Errorf("ansible inventory missing group for pool %q", pool)
		}
		hosts = append(hosts, hostsForGroup(inv, pool, pool, cfg, env)...)
	}
	return hosts, nil
}

func hostsForGroup(inv inventory, groupName, pool string, cfg *config.AnsibleConfig, env config.Environment) []Host {
	names := collectGroupHosts(inv, groupName, map[string]bool{})
	var hosts []Host
	for _, name := range names {
		vars := mergeMaps(inv.HostVars[name], varsForHost(inv, groupName, name))
		address := firstNonEmpty(mapString(vars, "ansible_host"), mapString(vars, "ansible_ssh_host"), name)
		user := firstNonEmpty(mapString(vars, "ansible_user"), mapString(vars, "ansible_ssh_user"), env.Hosts.Pools[pool].User, cfg.User)
		sshPort := firstNonZero(mapInt(vars, "ansible_port"), mapInt(vars, "ansible_ssh_port"))
		hosts = append(hosts, Host{
			ID:            name,
			Name:          name,
			Pool:          pool,
			User:          user,
			SSHPort:       sshPort,
			PublicAddress: address,
		})
	}
	return hosts
}

func collectGroupHosts(inv inventory, groupName string, seen map[string]bool) []string {
	if seen[groupName] {
		return nil
	}
	seen[groupName] = true
	group, ok := inv.Groups[groupName]
	if !ok {
		return nil
	}
	names := make([]string, 0, len(group.Hosts))
	for name := range group.Hosts {
		names = append(names, name)
	}
	for _, child := range group.Children {
		names = append(names, collectGroupHosts(inv, child, seen)...)
	}
	sort.Strings(names)
	return uniqueStrings(names)
}

func varsForHost(inv inventory, groupName, host string) map[string]any {
	group, ok := inv.Groups[groupName]
	if !ok {
		return nil
	}
	if vars, ok := group.Hosts[host]; ok {
		return vars
	}
	for _, child := range group.Children {
		if vars := varsForHost(inv, child, host); vars != nil {
			return vars
		}
	}
	return nil
}

func sortedPoolNames(env config.Environment) []string {
	names := make([]string, 0, len(env.Hosts.Pools))
	for name := range env.Hosts.Pools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func poolLabels(env config.Environment, pool string) map[string]string {
	if env.Hosts.Pools == nil {
		return nil
	}
	return env.Hosts.Pools[pool].Labels
}

func mapValue(value any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	if mapped, ok := value.(map[string]any); ok {
		return mapped
	}
	return map[string]any{}
}

func mergeMaps(groups ...map[string]any) map[string]any {
	merged := map[string]any{}
	for _, group := range groups {
		for key, value := range group {
			merged[key] = value
		}
	}
	return merged
}

func mapString(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	return stringValue(values[key])
}

func mapInt(values map[string]any, key string) int {
	if values == nil {
		return 0
	}
	switch typed := values[key].(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(typed))
		if err == nil {
			return n
		}
	}
	return 0
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		if value == nil {
			return ""
		}
		return fmt.Sprint(typed)
	}
}

func runCommand(ctx context.Context, binary string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
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

func firstNonZero(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
