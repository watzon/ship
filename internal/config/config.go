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
)

type Config struct {
	Project      string                 `yaml:"project"`
	Registry     string                 `yaml:"registry"`
	Environments map[string]Environment `yaml:"environments"`
	Services     map[string]Service     `yaml:"services"`
	Accessories  map[string]Accessory   `yaml:"accessories"`
	Secrets      []string               `yaml:"secrets"`
}

type Environment struct {
	Provider ProviderConfig `yaml:"provider"`
	Hosts    HostsConfig    `yaml:"hosts"`
}

type ProviderConfig struct {
	Hetzner *HetznerConfig `yaml:"hetzner"`
}

type HetznerConfig struct {
	Location   string   `yaml:"location"`
	ServerType string   `yaml:"server_type"`
	Image      string   `yaml:"image"`
	SSHKeys    []string `yaml:"ssh_keys"`
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
	if len(c.Services) == 0 {
		errs = append(errs, "at least one service is required")
	}
	for envName, env := range c.Environments {
		if env.Provider.Hetzner == nil {
			errs = append(errs, fmt.Sprintf("environment %q must define provider.hetzner", envName))
		} else {
			if env.Provider.Hetzner.Location == "" {
				errs = append(errs, fmt.Sprintf("environment %q provider.hetzner.location is required", envName))
			}
			if env.Provider.Hetzner.ServerType == "" {
				errs = append(errs, fmt.Sprintf("environment %q provider.hetzner.server_type is required", envName))
			}
			if env.Provider.Hetzner.Image == "" {
				errs = append(errs, fmt.Sprintf("environment %q provider.hetzner.image is required", envName))
			}
		}
		if len(env.Hosts.Pools) == 0 {
			errs = append(errs, fmt.Sprintf("environment %q must define hosts.pools", envName))
		}
		for poolName, pool := range env.Hosts.Pools {
			if pool.Count < 0 {
				errs = append(errs, fmt.Sprintf("environment %q pool %q count cannot be negative", envName, poolName))
			}
		}
	}
	for name, svc := range c.Services {
		if svc.Pool == "" {
			errs = append(errs, fmt.Sprintf("service %q pool is required", name))
		}
		if svc.Scale < 0 {
			errs = append(errs, fmt.Sprintf("service %q scale cannot be negative", name))
		}
		if svc.Rolling.MaxUnavailable < 0 {
			errs = append(errs, fmt.Sprintf("service %q rolling.max_unavailable cannot be negative", name))
		}
		if svc.Rolling.MaxSurge < 0 {
			errs = append(errs, fmt.Sprintf("service %q rolling.max_surge cannot be negative", name))
		}
		if svc.Rolling.DrainTimeoutSeconds < 0 {
			errs = append(errs, fmt.Sprintf("service %q rolling.drain_timeout_seconds cannot be negative", name))
		}
		if svc.Rolling.HealthTimeoutSeconds < 0 {
			errs = append(errs, fmt.Sprintf("service %q rolling.health_timeout_seconds cannot be negative", name))
		}
		if svc.Image.Build == "" && svc.Image.Ref == "" {
			errs = append(errs, fmt.Sprintf("service %q image.build or image.ref is required", name))
		}
		if svc.Image.Build == "" {
			if svc.Image.Dockerfile != "" {
				errs = append(errs, fmt.Sprintf("service %q image.dockerfile requires image.build", name))
			}
			if len(svc.Image.BuildArgs) > 0 {
				errs = append(errs, fmt.Sprintf("service %q image.build_args requires image.build", name))
			}
			if svc.Image.Target != "" {
				errs = append(errs, fmt.Sprintf("service %q image.target requires image.build", name))
			}
			if svc.Image.Platform != "" {
				errs = append(errs, fmt.Sprintf("service %q image.platform requires image.build", name))
			}
		}
	}
	for name, acc := range c.Accessories {
		if acc.Image == "" {
			errs = append(errs, fmt.Sprintf("accessory %q image is required", name))
		}
		if acc.Pool == "" {
			errs = append(errs, fmt.Sprintf("accessory %q pool is required", name))
		}
		if acc.Backup.Required && acc.Backup.Command == "" {
			errs = append(errs, fmt.Sprintf("accessory %q requires backup.command", name))
		}
		if acc.Backup.RestoreCheck && !acc.Backup.Required {
			errs = append(errs, fmt.Sprintf("accessory %q backup.restore_check requires backup.required", name))
		}
	}
	for envName := range c.Environments {
		env := c.Environments[envName]
		for svcName, svc := range c.Services {
			if _, ok := env.Hosts.Pools[svc.Pool]; !ok {
				errs = append(errs, fmt.Sprintf("service %q references missing pool %q in environment %q", svcName, svc.Pool, envName))
			}
		}
		for accName, acc := range c.Accessories {
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

environments:
  production:
    provider:
      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
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

  worker:
    image:
      build: .
    command: ./bin/worker
    pool: worker
    scale: 2
    health:
      command: ./bin/health-worker

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
  - DATABASE_URL
`
}
