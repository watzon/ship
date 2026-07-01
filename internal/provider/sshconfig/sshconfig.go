package sshconfig

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

const defaultConfigPath = "~/.ssh/config"

type Provider struct {
	DryRun   bool
	Env      config.Environment
	ReadFile func(string) ([]byte, error)
	Glob     func(string) ([]string, error)
	HomeDir  func() (string, error)
}

type Host struct {
	ID             string
	Name           string
	Pool           string
	User           string
	PublicAddress  string
	SSHPort        int
	IdentityFile   string
	KnownHostsFile string
	JumpHost       string
	SSHOptions     map[string]string
}

type stanza struct {
	Patterns []string
	Options  map[string]string
}

type hostOptions struct {
	HostName       string
	User           string
	Port           int
	IdentityFile   string
	KnownHostsFile string
	JumpHost       string
	Extra          map[string]string
}

func New(dryRun bool, env config.Environment) Provider {
	return Provider{
		DryRun:   dryRun,
		Env:      env,
		ReadFile: os.ReadFile,
		Glob:     filepath.Glob,
		HomeDir:  os.UserHomeDir,
	}
}

func (p Provider) Name() string {
	return config.ProviderSSHConfig
}

func (p Provider) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.SSHConfig == nil {
		return nil, fmt.Errorf("environment %q must define provider.ssh_config", environment)
	}
	hosts, err := p.Hosts(env)
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
			Location:    "ssh_config",
			Size:        "existing",
			Image:       "existing",
			Labels:      provider.HostLabels(project, environment, host.Pool, env.Hosts.Labels, poolLabels(env, host.Pool)),
		})
	}
	return plans, nil
}

func (p Provider) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	_ = ctx
	plans, hosts, err := p.planAndHosts(project, environment, env)
	if err != nil {
		return provider.ReconcileResult{}, err
	}
	if strings.TrimSpace(project) == "" {
		return provider.ReconcileResult{}, fmt.Errorf("project is required")
	}
	if strings.TrimSpace(environment) == "" {
		return provider.ReconcileResult{}, fmt.Errorf("environment is required")
	}
	return provider.ReconcileResult{Desired: plans, Existing: hosts}, nil
}

func (p Provider) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	_ = ctx
	_, hosts, err := p.planAndHosts(project, environment, p.Env)
	return hosts, err
}

func (p Provider) Delete(ctx context.Context, host provider.Host) error {
	_ = ctx
	_ = host
	return nil
}

func (p Provider) CredentialChecks(lookupEnv func(string) (string, bool)) []provider.CredentialCheck {
	_ = lookupEnv
	return []provider.CredentialCheck{{
		Name:           "ssh config inventory",
		Present:        true,
		Required:       false,
		PresentMessage: "using OpenSSH config inventory",
		MissingMessage: "using OpenSSH config inventory",
	}}
}

func (p Provider) planAndHosts(project, environment string, env config.Environment) ([]provider.HostPlan, []provider.Host, error) {
	if env.Provider.SSHConfig == nil {
		return nil, nil, fmt.Errorf("environment %q must define provider.ssh_config", environment)
	}
	sshHosts, err := p.Hosts(env)
	if err != nil {
		return nil, nil, err
	}
	plans := make([]provider.HostPlan, 0, len(sshHosts))
	hosts := make([]provider.Host, 0, len(sshHosts))
	for _, host := range sshHosts {
		labels := provider.HostLabels(project, environment, host.Pool, env.Hosts.Labels, poolLabels(env, host.Pool))
		plans = append(plans, provider.HostPlan{
			Project:     project,
			Environment: environment,
			Name:        host.Name,
			Pool:        host.Pool,
			User:        host.User,
			Location:    "ssh_config",
			Size:        "existing",
			Image:       "existing",
			Labels:      labels,
		})
		hosts = append(hosts, provider.Host{
			ID:             firstNonEmpty(host.ID, host.Name),
			Name:           host.Name,
			Pool:           host.Pool,
			PublicAddress:  firstNonEmpty(host.PublicAddress, host.Name),
			SSHPort:        host.SSHPort,
			IdentityFile:   host.IdentityFile,
			KnownHostsFile: host.KnownHostsFile,
			JumpHost:       host.JumpHost,
			SSHOptions:     copyStringMap(host.SSHOptions),
			Labels:         labels,
		})
	}
	return plans, hosts, nil
}

func (p Provider) Hosts(env config.Environment) ([]Host, error) {
	cfg := env.Provider.SSHConfig
	if cfg == nil {
		return nil, fmt.Errorf("environment must define provider.ssh_config")
	}
	path, err := p.configPath(cfg.Path)
	if err != nil {
		return nil, err
	}
	stanzas, err := p.parseFiles([]string{path}, map[string]bool{})
	if err != nil {
		return nil, err
	}
	var hosts []Host
	poolNames := sortedPoolNames(env)
	for _, poolName := range poolNames {
		pool := env.Hosts.Pools[poolName]
		names := append([]string(nil), pool.Hosts...)
		sort.Strings(names)
		for _, alias := range names {
			opts := optionsForHost(stanzas, alias)
			user := firstNonEmpty(opts.User, pool.User, cfg.User, "root")
			hosts = append(hosts, Host{
				ID:             alias,
				Name:           alias,
				Pool:           poolName,
				User:           user,
				PublicAddress:  firstNonEmpty(resolveToken(opts.HostName, alias), alias),
				SSHPort:        opts.Port,
				IdentityFile:   opts.IdentityFile,
				KnownHostsFile: opts.KnownHostsFile,
				JumpHost:       opts.JumpHost,
				SSHOptions:     opts.Extra,
			})
		}
	}
	return hosts, nil
}

func (p Provider) parseFiles(paths []string, seen map[string]bool) ([]stanza, error) {
	var out []stanza
	for _, rawPath := range paths {
		path, err := p.expandPath(rawPath)
		if err != nil {
			return nil, err
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		readFile := p.ReadFile
		if readFile == nil {
			readFile = os.ReadFile
		}
		data, err := readFile(path)
		if err != nil {
			return nil, fmt.Errorf("read ssh config %s: %w", path, err)
		}
		parsed, err := p.parseConfig(path, string(data), seen)
		if err != nil {
			return nil, err
		}
		out = append(out, parsed...)
	}
	return out, nil
}

func (p Provider) parseConfig(path, data string, seen map[string]bool) ([]stanza, error) {
	var stanzas []stanza
	current := stanza{Patterns: []string{"*"}, Options: map[string]string{}}
	hasCurrent := false
	scanner := bufio.NewScanner(strings.NewReader(data))
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		tokens, err := tokensForLine(scanner.Text())
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, lineNo, err)
		}
		if len(tokens) == 0 {
			continue
		}
		key := strings.ToLower(tokens[0])
		values := tokens[1:]
		if strings.Contains(key, "=") {
			parts := strings.SplitN(tokens[0], "=", 2)
			key = strings.ToLower(parts[0])
			values = append([]string{parts[1]}, values...)
		}
		switch key {
		case "include":
			if hasCurrent || len(current.Options) > 0 {
				stanzas = append(stanzas, current)
				current = stanza{Patterns: current.Patterns, Options: map[string]string{}}
				hasCurrent = false
			}
			included, err := p.includeFiles(filepath.Dir(path), values)
			if err != nil {
				return nil, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			includedStanzas, err := p.parseFiles(included, seen)
			if err != nil {
				return nil, err
			}
			stanzas = append(stanzas, includedStanzas...)
		case "host":
			if hasCurrent || len(current.Options) > 0 {
				stanzas = append(stanzas, current)
			}
			current = stanza{Patterns: values, Options: map[string]string{}}
			hasCurrent = true
		default:
			if len(values) == 0 {
				continue
			}
			current.Options[key] = strings.Join(values, " ")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if hasCurrent || len(current.Options) > 0 {
		stanzas = append(stanzas, current)
	}
	return stanzas, nil
}

func (p Provider) includeFiles(base string, patterns []string) ([]string, error) {
	glob := p.Glob
	if glob == nil {
		glob = filepath.Glob
	}
	var out []string
	for _, pattern := range patterns {
		expanded, err := p.expandPath(pattern)
		if err != nil {
			return nil, err
		}
		if !filepath.IsAbs(expanded) {
			expanded = filepath.Join(base, expanded)
		}
		matches, err := glob(expanded)
		if err != nil {
			return nil, err
		}
		sort.Strings(matches)
		out = append(out, matches...)
	}
	return out, nil
}

func (p Provider) configPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		path = defaultConfigPath
	}
	return p.expandPath(path)
}

func (p Provider) expandPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("ssh config path is required")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		homeDir := p.HomeDir
		if homeDir == nil {
			homeDir = os.UserHomeDir
		}
		home, err := homeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	return path, nil
}

func optionsForHost(stanzas []stanza, alias string) hostOptions {
	opts := hostOptions{Extra: map[string]string{}}
	seen := map[string]bool{}
	for _, stanza := range stanzas {
		if !matchesHost(stanza.Patterns, alias) {
			continue
		}
		for rawKey, value := range stanza.Options {
			key := strings.ToLower(rawKey)
			if seen[key] {
				continue
			}
			seen[key] = true
			switch key {
			case "hostname":
				opts.HostName = value
			case "user":
				opts.User = value
			case "port":
				if port, err := strconv.Atoi(value); err == nil {
					opts.Port = port
				}
			case "identityfile":
				opts.IdentityFile = value
			case "userknownhostsfile":
				opts.KnownHostsFile = value
			case "proxyjump":
				if strings.TrimSpace(value) != "none" {
					opts.JumpHost = value
				}
			case "host", "match", "include":
			default:
				opts.Extra[canonicalOption(rawKey)] = value
			}
		}
	}
	if len(opts.Extra) == 0 {
		opts.Extra = nil
	}
	return opts
}

func matchesHost(patterns []string, alias string) bool {
	matched := false
	for _, pattern := range patterns {
		negated := strings.HasPrefix(pattern, "!")
		pattern = strings.TrimPrefix(pattern, "!")
		ok, err := filepath.Match(pattern, alias)
		if err != nil {
			continue
		}
		if ok && negated {
			return false
		}
		if ok {
			matched = true
		}
	}
	return matched
}

func tokensForLine(line string) ([]string, error) {
	var tokens []string
	var b strings.Builder
	var quote rune
	escaped := false
	for _, r := range line {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
				continue
			}
			b.WriteRune(r)
			continue
		}
		if r == '"' || r == '\'' {
			quote = r
			continue
		}
		if r == '#' {
			break
		}
		if r == ' ' || r == '\t' {
			if b.Len() > 0 {
				tokens = append(tokens, b.String())
				b.Reset()
			}
			continue
		}
		b.WriteRune(r)
	}
	if escaped {
		b.WriteRune('\\')
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}
	if b.Len() > 0 {
		tokens = append(tokens, b.String())
	}
	return tokens, nil
}

func canonicalOption(value string) string {
	switch strings.ToLower(value) {
	case "forwardagent":
		return "ForwardAgent"
	case "identitiesonly":
		return "IdentitiesOnly"
	case "identityagent":
		return "IdentityAgent"
	case "proxycommand":
		return "ProxyCommand"
	case "certificatefile":
		return "CertificateFile"
	case "serveraliveinterval":
		return "ServerAliveInterval"
	case "serveralivecountmax":
		return "ServerAliveCountMax"
	default:
		return value
	}
}

func resolveToken(value, alias string) string {
	return strings.ReplaceAll(value, "%h", alias)
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

func copyStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
