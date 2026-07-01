package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	DefaultConfigFile = "ship.yml"
	LocalStateDir     = ".ship"
	RemoteStateDir    = "/var/lib/ship"
	RemoteBinaryPath  = "/usr/local/bin/ship"
	DefaultCaddyImage = "caddy:2"

	ProviderHetzner = "hetzner"
	ProviderVultr   = "vultr"
)

const (
	SSHFirewallManaged  = "managed"
	SSHFirewallExternal = "external"
	SSHFirewallDisabled = "disabled"
)

type Config struct {
	Project      string                 `yaml:"project"`
	Registry     string                 `yaml:"registry"`
	Ingress      IngressConfig          `yaml:"ingress"`
	Environments map[string]Environment `yaml:"environments"`
	Services     map[string]Service     `yaml:"services"`
	Accessories  map[string]Accessory   `yaml:"accessories"`
	Secrets      []string               `yaml:"secrets"`
}

type Environment struct {
	Provider    ProviderConfig       `yaml:"provider"`
	Hosts       HostsConfig          `yaml:"hosts"`
	Ingress     IngressConfig        `yaml:"ingress"`
	Services    map[string]Service   `yaml:"services"`
	Accessories map[string]Accessory `yaml:"accessories"`
	Secrets     []string             `yaml:"secrets"`
}

type ProviderConfig struct {
	Hetzner *HetznerConfig `yaml:"hetzner"`
	Vultr   *VultrConfig   `yaml:"vultr"`
	Unknown []string       `yaml:"-"`
}

type HetznerConfig struct {
	Location        string                `yaml:"location"`
	ServerType      string                `yaml:"server_type"`
	Image           string                `yaml:"image"`
	SSHKeys         []string              `yaml:"ssh_keys"`
	Network         HetznerNetworkConfig  `yaml:"network"`
	Firewall        HetznerFirewallConfig `yaml:"firewall"`
	SSHAllowedCIDRs []string              `yaml:"ssh_allowed_cidrs"`
	SSHFirewall     string                `yaml:"ssh_firewall"`
}

type HetznerNetworkConfig struct {
	Enabled *bool  `yaml:"enabled"`
	Name    string `yaml:"name"`
	IPRange string `yaml:"ip_range"`
}

type HetznerFirewallConfig struct {
	Enabled *bool  `yaml:"enabled"`
	Name    string `yaml:"name"`
}

type VultrConfig struct {
	Region     string   `yaml:"region"`
	Plan       string   `yaml:"plan"`
	OSID       int      `yaml:"os_id"`
	ImageID    string   `yaml:"image_id"`
	SnapshotID string   `yaml:"snapshot_id"`
	AppID      int      `yaml:"app_id"`
	SSHKeyIDs  []string `yaml:"ssh_key_ids"`
}

func (p *ProviderConfig) UnmarshalYAML(value *yaml.Node) error {
	type providerConfig ProviderConfig
	var decoded providerConfig
	if err := value.Decode(&decoded); err != nil {
		return err
	}
	*p = ProviderConfig(decoded)
	p.Unknown = nil
	if value.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		name := value.Content[i].Value
		switch name {
		case ProviderHetzner, ProviderVultr:
		default:
			p.Unknown = append(p.Unknown, name)
		}
	}
	sort.Strings(p.Unknown)
	return nil
}

func (p ProviderConfig) Name() string {
	if p.Hetzner != nil {
		return ProviderHetzner
	}
	if p.Vultr != nil {
		return ProviderVultr
	}
	return ""
}

func (p ProviderConfig) Validate(envName string) []string {
	var errs []string
	blocks := p.blocks()
	if len(blocks) == 0 {
		return []string{fmt.Sprintf("environment %q must define exactly one provider", envName)}
	}
	if len(blocks) > 1 {
		errs = append(errs, fmt.Sprintf("environment %q must define exactly one provider (found %s)", envName, strings.Join(blocks, ", ")))
	}
	if len(p.Unknown) > 0 {
		errs = append(errs, fmt.Sprintf("environment %q defines unsupported provider(s): %s", envName, strings.Join(p.Unknown, ", ")))
	}
	if p.Hetzner != nil {
		if p.Hetzner.Location == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.hetzner.location is required", envName))
		}
		if p.Hetzner.ServerType == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.hetzner.server_type is required", envName))
		}
		if p.Hetzner.Image == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.hetzner.image is required", envName))
		}
		sshFirewall := p.Hetzner.EffectiveSSHFirewall()
		switch sshFirewall {
		case SSHFirewallManaged:
			if p.Hetzner.Firewall.EnabledValue(true) && len(p.Hetzner.SSHAllowedCIDRs) == 0 {
				errs = append(errs, fmt.Sprintf("environment %q provider.hetzner.ssh_allowed_cidrs is required when managed firewall SSH is enabled", envName))
			}
		case SSHFirewallExternal, SSHFirewallDisabled:
		default:
			errs = append(errs, fmt.Sprintf("environment %q provider.hetzner.ssh_firewall must be managed, external, or disabled", envName))
		}
	}
	if p.Vultr != nil {
		if p.Vultr.Region == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.vultr.region is required", envName))
		}
		if p.Vultr.Plan == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.vultr.plan is required", envName))
		}
		sources := p.Vultr.sourceBlocks()
		if len(sources) != 1 {
			errs = append(errs, fmt.Sprintf("environment %q provider.vultr must define exactly one source (found %s)", envName, strings.Join(sources, ", ")))
		}
	}
	return errs
}

func (h HetznerConfig) EffectiveSSHFirewall() string {
	value := strings.TrimSpace(h.SSHFirewall)
	if value == "" {
		return SSHFirewallManaged
	}
	return value
}

func (n HetznerNetworkConfig) EnabledValue(def bool) bool {
	if n.Enabled == nil {
		return def
	}
	return *n.Enabled
}

func (f HetznerFirewallConfig) EnabledValue(def bool) bool {
	if f.Enabled == nil {
		return def
	}
	return *f.Enabled
}

func (p ProviderConfig) blocks() []string {
	var blocks []string
	if p.Hetzner != nil {
		blocks = append(blocks, ProviderHetzner)
	}
	if p.Vultr != nil {
		blocks = append(blocks, ProviderVultr)
	}
	blocks = append(blocks, p.Unknown...)
	sort.Strings(blocks)
	return blocks
}

func (v VultrConfig) sourceBlocks() []string {
	var blocks []string
	if v.OSID != 0 {
		blocks = append(blocks, "os_id")
	}
	if v.ImageID != "" {
		blocks = append(blocks, "image_id")
	}
	if v.SnapshotID != "" {
		blocks = append(blocks, "snapshot_id")
	}
	if v.AppID != 0 {
		blocks = append(blocks, "app_id")
	}
	return blocks
}

type HostsConfig struct {
	Pools map[string]Pool `yaml:"pools"`
}

type Pool struct {
	Count int      `yaml:"count"`
	Hosts []string `yaml:"hosts"`
	User  string   `yaml:"user"`
}

type Service struct {
	Image   ImageSpec   `yaml:"image"`
	Command string      `yaml:"command"`
	Pool    string      `yaml:"pool"`
	Scale   int         `yaml:"scale"`
	Ports   []int       `yaml:"ports"`
	Health  HealthCheck `yaml:"health"`
	Ingress *Ingress    `yaml:"ingress"`
	Env     []string    `yaml:"env"`
	Secrets []string    `yaml:"secrets"`
	Rolling Rolling     `yaml:"rolling"`
}

type ImageSpec struct {
	Build      string            `yaml:"build"`
	Dockerfile string            `yaml:"dockerfile"`
	Ref        string            `yaml:"ref"`
	BuildArgs  map[string]string `yaml:"build_args"`
	Target     string            `yaml:"target"`
	Platform   string            `yaml:"platform"`
}

type HealthCheck struct {
	HTTP    string `yaml:"http"`
	Command string `yaml:"command"`
}

type Ingress struct {
	Domains []string `yaml:"domains"`
}

type IngressConfig struct {
	Caddy CaddyConfig `yaml:"caddy"`
}

type CaddyConfig struct {
	Image string `yaml:"image"`
	Email string `yaml:"email"`
}

type Rolling struct {
	MaxUnavailable       int `yaml:"max_unavailable"`
	MaxSurge             int `yaml:"max_surge"`
	DrainTimeoutSeconds  int `yaml:"drain_timeout_seconds"`
	HealthTimeoutSeconds int `yaml:"health_timeout_seconds"`
}

type Accessory struct {
	Image       string     `yaml:"image"`
	Pool        string     `yaml:"pool"`
	Primary     bool       `yaml:"primary"`
	Volumes     []string   `yaml:"volumes"`
	VolumeOwner string     `yaml:"volume_owner"`
	Ports       []int      `yaml:"ports"`
	Env         []string   `yaml:"env"`
	Secrets     []string   `yaml:"secrets"`
	Backup      BackupSpec `yaml:"backup"`
}

type BackupSpec struct {
	Command        string `yaml:"command"`
	RestoreCommand string `yaml:"restore_command"`
	ArtifactDir    string `yaml:"artifact_dir"`
	Required       bool   `yaml:"required"`
	RestoreCheck   bool   `yaml:"restore_check"`
}

func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigFile
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) ResolveEnvironment(name string) (*Config, Environment, error) {
	if c == nil {
		return nil, Environment{}, errors.New("config is required")
	}
	env, err := c.Environment(name)
	if err != nil {
		return nil, Environment{}, err
	}
	resolved := *c
	resolved.Environments = map[string]Environment{name: env}
	resolved.Services = copyServices(c.Services)
	for serviceName, svc := range env.Services {
		if resolved.Services == nil {
			resolved.Services = map[string]Service{}
		}
		resolved.Services[serviceName] = svc
	}
	resolved.Accessories = copyAccessories(c.Accessories)
	for accessoryName, acc := range env.Accessories {
		if resolved.Accessories == nil {
			resolved.Accessories = map[string]Accessory{}
		}
		resolved.Accessories[accessoryName] = acc
	}
	resolved.Secrets = mergeNames(c.Secrets, env.Secrets)
	resolved.Ingress = mergeIngressConfig(c.Ingress, env.Ingress)
	if resolved.Ingress.Caddy.Image == "" {
		resolved.Ingress.Caddy.Image = DefaultCaddyImage
	}
	env.Services = nil
	env.Accessories = nil
	env.Secrets = nil
	env.Ingress = resolved.Ingress
	resolved.Environments[name] = env
	if err := resolved.ValidateResolved(name); err != nil {
		return nil, Environment{}, err
	}
	return &resolved, env, nil
}

func (c *Config) ValidateResolved(envName string) error {
	if c == nil {
		return errors.New("config is required")
	}
	if len(c.Environments) != 1 {
		return fmt.Errorf("resolved config for %q must contain exactly one environment", envName)
	}
	return c.Validate()
}

func (c *Config) Validate() error {
	var errs []string
	if strings.TrimSpace(c.Project) == "" {
		errs = append(errs, "project is required")
	}
	if strings.TrimSpace(c.Registry) == "" {
		errs = append(errs, "registry is required")
	}
	if len(c.Environments) == 0 {
		errs = append(errs, "at least one environment is required")
	}
	if totalServiceCount(c) == 0 {
		errs = append(errs, "at least one service is required")
	}
	for envName, env := range c.Environments {
		errs = append(errs, env.Provider.Validate(envName)...)
		if len(env.Hosts.Pools) == 0 {
			errs = append(errs, fmt.Sprintf("environment %q must define hosts.pools", envName))
		}
		for poolName, pool := range env.Hosts.Pools {
			if pool.Count < 0 {
				errs = append(errs, fmt.Sprintf("environment %q pool %q count cannot be negative", envName, poolName))
			}
		}
	}
	errs = append(errs, validateServices("", c.Services)...)
	errs = append(errs, validateAccessories("", c.Accessories)...)
	errs = append(errs, validateSecretNames("root", c.Secrets)...)
	for envName, env := range c.Environments {
		errs = append(errs, validateSecretNames(fmt.Sprintf("environment %q", envName), env.Secrets)...)
		errs = append(errs, validateServices(fmt.Sprintf("environment %q ", envName), env.Services)...)
		errs = append(errs, validateAccessories(fmt.Sprintf("environment %q ", envName), env.Accessories)...)
	}
	for envName := range c.Environments {
		resolved, env, err := c.resolvedForValidation(envName)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if len(resolved.Services) == 0 {
			errs = append(errs, fmt.Sprintf("environment %q must resolve at least one service", envName))
		}
		for svcName, svc := range resolved.Services {
			if _, ok := env.Hosts.Pools[svc.Pool]; !ok {
				errs = append(errs, fmt.Sprintf("service %q references missing pool %q in environment %q", svcName, svc.Pool, envName))
			}
		}
		for accName, acc := range resolved.Accessories {
			if _, ok := env.Hosts.Pools[acc.Pool]; !ok {
				errs = append(errs, fmt.Sprintf("accessory %q references missing pool %q in environment %q", accName, acc.Pool, envName))
			}
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return errors.New(strings.Join(errs, "\n"))
	}
	return nil
}

func totalServiceCount(c *Config) int {
	if c == nil {
		return 0
	}
	total := len(c.Services)
	for _, env := range c.Environments {
		total += len(env.Services)
	}
	return total
}

func validateServices(prefix string, services map[string]Service) []string {
	var errs []string
	for name, svc := range services {
		label := prefix + fmt.Sprintf("service %q", name)
		errs = append(errs, validateSecretNames(label, svc.Secrets)...)
		if svc.Pool == "" {
			errs = append(errs, fmt.Sprintf("%s pool is required", label))
		}
		if svc.Scale < 0 {
			errs = append(errs, fmt.Sprintf("%s scale cannot be negative", label))
		}
		if svc.Rolling.MaxUnavailable < 0 {
			errs = append(errs, fmt.Sprintf("%s rolling.max_unavailable cannot be negative", label))
		}
		if svc.Rolling.MaxSurge < 0 {
			errs = append(errs, fmt.Sprintf("%s rolling.max_surge cannot be negative", label))
		}
		if svc.Rolling.DrainTimeoutSeconds < 0 {
			errs = append(errs, fmt.Sprintf("%s rolling.drain_timeout_seconds cannot be negative", label))
		}
		if svc.Rolling.HealthTimeoutSeconds < 0 {
			errs = append(errs, fmt.Sprintf("%s rolling.health_timeout_seconds cannot be negative", label))
		}
		if svc.Image.Build == "" && svc.Image.Ref == "" {
			errs = append(errs, fmt.Sprintf("%s image.build or image.ref is required", label))
		}
		if svc.Image.Build == "" {
			if svc.Image.Dockerfile != "" {
				errs = append(errs, fmt.Sprintf("%s image.dockerfile requires image.build", label))
			}
			if len(svc.Image.BuildArgs) > 0 {
				errs = append(errs, fmt.Sprintf("%s image.build_args requires image.build", label))
			}
			if svc.Image.Target != "" {
				errs = append(errs, fmt.Sprintf("%s image.target requires image.build", label))
			}
			if svc.Image.Platform != "" {
				errs = append(errs, fmt.Sprintf("%s image.platform requires image.build", label))
			}
		}
	}
	return errs
}

func validateAccessories(prefix string, accessories map[string]Accessory) []string {
	var errs []string
	for name, acc := range accessories {
		label := prefix + fmt.Sprintf("accessory %q", name)
		errs = append(errs, validateSecretNames(label, acc.Secrets)...)
		if acc.Image == "" {
			errs = append(errs, fmt.Sprintf("%s image is required", label))
		}
		if acc.Pool == "" {
			errs = append(errs, fmt.Sprintf("%s pool is required", label))
		}
		if acc.Backup.Required && acc.Backup.Command == "" {
			errs = append(errs, fmt.Sprintf("%s requires backup.command", label))
		}
		if acc.Backup.RestoreCheck && !acc.Backup.Required {
			errs = append(errs, fmt.Sprintf("%s backup.restore_check requires backup.required", label))
		}
	}
	return errs
}

func (c *Config) resolvedForValidation(envName string) (*Config, Environment, error) {
	env, err := c.Environment(envName)
	if err != nil {
		return nil, Environment{}, err
	}
	resolved := *c
	resolved.Environments = map[string]Environment{envName: env}
	resolved.Services = copyServices(c.Services)
	for serviceName, svc := range env.Services {
		if resolved.Services == nil {
			resolved.Services = map[string]Service{}
		}
		resolved.Services[serviceName] = svc
	}
	resolved.Accessories = copyAccessories(c.Accessories)
	for accessoryName, acc := range env.Accessories {
		if resolved.Accessories == nil {
			resolved.Accessories = map[string]Accessory{}
		}
		resolved.Accessories[accessoryName] = acc
	}
	resolved.Secrets = mergeNames(c.Secrets, env.Secrets)
	resolved.Ingress = mergeIngressConfig(c.Ingress, env.Ingress)
	return &resolved, env, nil
}

func copyServices(in map[string]Service) map[string]Service {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]Service, len(in))
	for name, svc := range in {
		out[name] = svc
	}
	return out
}

func copyAccessories(in map[string]Accessory) map[string]Accessory {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]Accessory, len(in))
	for name, acc := range in {
		out[name] = acc
	}
	return out
}

func mergeIngressConfig(base, override IngressConfig) IngressConfig {
	out := base
	if override.Caddy.Image != "" {
		out.Caddy.Image = override.Caddy.Image
	}
	if override.Caddy.Email != "" {
		out.Caddy.Email = override.Caddy.Email
	}
	return out
}

func mergeNames(groups ...[]string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, group := range groups {
		for _, raw := range group {
			name := strings.TrimSpace(raw)
			if name == "" {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func validateSecretNames(scope string, names []string) []string {
	var errs []string
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !validEnvName(name) {
			errs = append(errs, fmt.Sprintf("%s secret name %q is invalid", scope, name))
		}
	}
	return errs
}

func validEnvName(name string) bool {
	for i, r := range name {
		if i == 0 {
			if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
				return false
			}
			continue
		}
		if r != '_' &&
			(r < 'A' || r > 'Z') &&
			(r < 'a' || r > 'z') &&
			(r < '0' || r > '9') {
			return false
		}
	}
	return name != ""
}

func (c *Config) Environment(name string) (Environment, error) {
	env, ok := c.Environments[name]
	if !ok {
		return Environment{}, fmt.Errorf("unknown environment %q", name)
	}
	return env, nil
}

func ProjectRoot(start string) (string, error) {
	if start == "" {
		var err error
		start, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, DefaultConfigFile)); err == nil {
			return dir, nil
		}
		next := filepath.Dir(dir)
		if next == dir {
			return "", fmt.Errorf("could not find %s", DefaultConfigFile)
		}
		dir = next
	}
}

func Sample() string {
	return `project: example
registry: ghcr.io/acme/example

ingress:
  caddy:
    image: caddy:2

environments:
  staging:
    provider:
      hetzner:
        location: hel1
        server_type: cx23
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    hosts:
      pools:
        web:
          count: 1
        worker:
          count: 1
    services:
      web:
        image:
          build: .
          dockerfile: Dockerfile
        command: ./bin/server
        pool: web
        scale: 1
        ports: [3000]
        health:
          http: /up
        ingress:
          domains:
            - staging.example.com
        secrets:
          - DATABASE_URL

  production:
    provider:
      hetzner:
        location: hel1
        server_type: cx23
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    hosts:
      pools:
        web:
          count: 3
        worker:
          count: 2
        ingress:
          count: 2

services:
  web:
    image:
      build: .
      dockerfile: Dockerfile
    command: ./bin/server
    pool: web
    scale: 6
    ports: [3000]
    health:
      http: /up
    ingress:
      domains:
        - example.com
    secrets:
      - DATABASE_URL

  worker:
    image:
      build: .
    command: ./bin/worker
    pool: worker
    scale: 2
    health:
      command: ./bin/health-worker
    secrets:
      - JOB_SECRET

accessories:
  postgres:
    image: postgres:17
    pool: worker
    primary: true
    volumes:
      - postgres-data:/var/lib/postgresql/data
    backup:
      command: pg_dumpall
      restore_command: psql -f "$SHIP_BACKUP_ARTIFACT"
      required: true
      restore_check: true
    secrets:
      - POSTGRES_PASSWORD

secrets:
  - SESSION_SECRET
`
}
