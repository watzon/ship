package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/docker"
	"github.com/watzon/ship/internal/provider"
	"github.com/watzon/ship/internal/provider/providers"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/secrets"
	"github.com/watzon/ship/internal/state"
	"github.com/watzon/ship/internal/transport"
)

type Status string

const (
	StatusPass Status = "pass"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

type Check struct {
	Name    string            `json:"name"`
	Status  Status            `json:"status"`
	Message string            `json:"message,omitempty"`
	Details map[string]string `json:"details,omitempty"`
}

type Summary struct {
	Passed   int `json:"passed"`
	Warnings int `json:"warnings"`
	Failed   int `json:"failed"`
}

type Report struct {
	Checks  []Check `json:"checks"`
	Summary Summary `json:"summary"`
}

type Docker interface {
	Available(context.Context) error
	BuildKitSupported(context.Context) error
	RegistryLoggedIn(context.Context, string) error
}

type RemoteRunner interface {
	Run(context.Context, scheduler.Host, string) (string, error)
}

type Options struct {
	ConfigPath   string
	Docker       Docker
	SSHAvailable func(context.Context) error
	Remote       RemoteRunner
	LookupEnv    func(string) (string, bool)
	ProviderFor  func(config.Environment, bool) (provider.Provider, error)
}

func NewReport(checks []Check) Report {
	report := Report{Checks: checks}
	for _, check := range checks {
		switch check.Status {
		case StatusPass:
			report.Summary.Passed++
		case StatusWarn:
			report.Summary.Warnings++
		case StatusFail:
			report.Summary.Failed++
		}
	}
	return report
}

func ConfigLoadError(err error) Report {
	return NewReport([]Check{fail("config", err.Error())})
}

func (r Report) Failed() bool {
	return r.Summary.Failed > 0
}

func (r Report) WriteText(w io.Writer) {
	for _, check := range r.Checks {
		label := "ok"
		switch check.Status {
		case StatusWarn:
			label = "warn"
		case StatusFail:
			label = "fail"
		}
		fmt.Fprintf(w, "%-4s %s", label, check.Name)
		if check.Message != "" {
			fmt.Fprintf(w, ": %s", check.Message)
		}
		if len(check.Details) > 0 {
			keys := make([]string, 0, len(check.Details))
			for key := range check.Details {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				fmt.Fprintf(w, " %s=%s", key, check.Details[key])
			}
		}
		fmt.Fprintln(w)
	}
}

func (r Report) WriteJSON(w io.Writer) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(r)
}

func Run(ctx context.Context, cfg *config.Config, opts Options) Report {
	if opts.Docker == nil {
		opts.Docker = docker.Client{}
	}
	if opts.SSHAvailable == nil {
		opts.SSHAvailable = transport.Available
	}
	if opts.Remote == nil {
		opts.Remote = sshRemote{}
	}
	if opts.LookupEnv == nil {
		opts.LookupEnv = os.LookupEnv
	}
	if opts.ProviderFor == nil {
		opts.ProviderFor = providers.ForEnvironment
	}

	checks := []Check{pass("config", "configuration is valid")}

	hasBuilds := hasBuildServices(cfg)
	checks = append(checks, errCheck("docker", opts.Docker.Available(ctx), true))
	checks = append(checks, errCheck("docker buildkit", opts.Docker.BuildKitSupported(ctx), hasBuilds))
	checks = append(checks, errCheck("registry auth", opts.Docker.RegistryLoggedIn(ctx, cfg.Registry), hasBuilds))
	checks = append(checks, errCheck("ssh", opts.SSHAvailable(ctx), true))
	checks = append(checks, providerCredentialChecks(cfg, opts.LookupEnv, opts.ProviderFor)...)
	checks = append(checks, secretChecks(cfg)...)
	checks = append(checks, buildPathChecks(cfg, opts.ConfigPath)...)
	checks = append(checks, remoteChecks(ctx, cfg, opts.ConfigPath, opts.Remote)...)

	return NewReport(checks)
}

func errCheck(name string, err error, hard bool) Check {
	if err == nil {
		return pass(name, "available")
	}
	if hard {
		return fail(name, err.Error())
	}
	return warn(name, err.Error())
}

func providerCredentialChecks(cfg *config.Config, lookupEnv func(string) (string, bool), providerFor func(config.Environment, bool) (provider.Provider, error)) []Check {
	var checks []Check
	seen := map[string]bool{}
	for _, envName := range sortedEnvironmentNames(cfg) {
		env := cfg.Environments[envName]
		prov, err := providerFor(env, true)
		if err != nil {
			checks = append(checks, fail("provider:"+envName, err.Error()))
			continue
		}
		if seen[prov.Name()] {
			continue
		}
		seen[prov.Name()] = true
		for _, credential := range prov.CredentialChecks(lookupEnv) {
			checks = append(checks, credentialCheck(credential))
		}
	}
	if len(checks) == 0 {
		checks = append(checks, warn("provider credentials", "no provider environments configured"))
	}
	return checks
}

func credentialCheck(check provider.CredentialCheck) Check {
	if check.Present {
		return pass(check.Name, check.PresentMessage)
	}
	if check.Required {
		return fail(check.Name, check.MissingMessage)
	}
	return warn(check.Name, check.MissingMessage)
}

func secretChecks(cfg *config.Config) []Check {
	verified, _ := secrets.Verify(cfg)
	checks := make([]Check, 0, len(verified))
	for _, check := range verified {
		name := "secret:" + check.Name
		if check.Present {
			checks = append(checks, Check{
				Name:    name,
				Status:  StatusPass,
				Message: "present",
				Details: map[string]string{"digest": check.Digest},
			})
			continue
		}
		checks = append(checks, fail(name, "missing"))
	}
	return checks
}

func buildPathChecks(cfg *config.Config, configPath string) []Check {
	base := "."
	if configPath != "" {
		base = filepath.Dir(configPath)
	}
	serviceNames := sortedServiceNames(cfg)
	var checks []Check
	for _, serviceName := range serviceNames {
		svc := cfg.Services[serviceName]
		if strings.TrimSpace(svc.Image.Build) == "" {
			continue
		}
		contextPath := resolvePath(base, svc.Image.Build)
		checks = append(checks, pathCheck("build context:"+serviceName, contextPath, true))

		dockerfile := svc.Image.Dockerfile
		if dockerfile == "" {
			dockerfile = "Dockerfile"
		}
		dockerfilePath := dockerfile
		if !filepath.IsAbs(dockerfilePath) {
			dockerfilePath = filepath.Join(contextPath, dockerfilePath)
		}
		checks = append(checks, pathCheck("dockerfile:"+serviceName, dockerfilePath, false))
	}
	return checks
}

func pathCheck(name, path string, wantDir bool) Check {
	info, err := os.Stat(path)
	if err != nil {
		return fail(name, fmt.Sprintf("%s does not exist", path))
	}
	if wantDir && !info.IsDir() {
		return fail(name, fmt.Sprintf("%s is not a directory", path))
	}
	if !wantDir && info.IsDir() {
		return fail(name, fmt.Sprintf("%s is a directory", path))
	}
	return Check{Name: name, Status: StatusPass, Message: "found", Details: map[string]string{"path": path}}
}

func remoteChecks(ctx context.Context, cfg *config.Config, configPath string, runner RemoteRunner) []Check {
	hosts, diagnostics := remoteHosts(cfg, configPath)
	checks := append([]Check(nil), diagnostics...)
	if len(hosts) == 0 {
		return append(checks, warn("remote hosts", "no explicit or provisioned hosts configured; remote checks skipped"))
	}
	for _, host := range hosts {
		prefix := "remote:" + host.Environment + "/" + host.Host.Name
		if _, err := runner.Run(ctx, host.Host, "true"); err != nil {
			checks = append(checks, fail(prefix+" ssh", err.Error()))
			continue
		}
		checks = append(checks, pass(prefix+" ssh", "reachable"))
		checks = append(checks, remoteLinuxCheck(ctx, runner, host, prefix))
		checks = append(checks, remoteCommandCheck(ctx, runner, host, prefix+" docker", "command -v docker >/dev/null", "docker is installed"))
		checks = append(checks, remoteCommandCheck(ctx, runner, host, prefix+" systemd", "command -v systemctl >/dev/null && test -d /run/systemd/system", "systemd is available"))
		checks = append(checks, remoteCommandCheck(ctx, runner, host, prefix+" docker boot", "systemctl is-enabled docker >/dev/null && systemctl is-active docker >/dev/null", "docker.service is enabled and active"))
		checks = append(checks, remoteCommandCheck(ctx, runner, host, prefix+" state dir", "test -d "+config.RemoteStateDir+" && test -w "+config.RemoteStateDir, config.RemoteStateDir+" is writable"))
		checks = append(checks, remoteCommandCheck(ctx, runner, host, prefix+" ship binary", "test -x "+config.RemoteBinaryPath, config.RemoteBinaryPath+" is installed"))
	}
	return checks
}

func remoteLinuxCheck(ctx context.Context, runner RemoteRunner, host remoteHost, prefix string) Check {
	out, err := runner.Run(ctx, host.Host, "uname -s")
	if err != nil {
		return fail(prefix+" linux", err.Error())
	}
	if strings.TrimSpace(out) != "Linux" {
		return fail(prefix+" linux", fmt.Sprintf("expected Linux, got %q", strings.TrimSpace(out)))
	}
	return pass(prefix+" linux", "Linux")
}

func remoteCommandCheck(ctx context.Context, runner RemoteRunner, host remoteHost, name, command, okMessage string) Check {
	if _, err := runner.Run(ctx, host.Host, command); err != nil {
		return fail(name, err.Error())
	}
	return pass(name, okMessage)
}

func hasBuildServices(cfg *config.Config) bool {
	for _, svc := range cfg.Services {
		if strings.TrimSpace(svc.Image.Build) != "" {
			return true
		}
	}
	return false
}

func sortedEnvironmentNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Environments))
	for name := range cfg.Environments {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedServiceNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Services))
	for name := range cfg.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type remoteHost struct {
	Environment string
	Host        scheduler.Host
}

func remoteHosts(cfg *config.Config, configPath string) ([]remoteHost, []Check) {
	store := state.NewStore(localStateDir(configPath))
	var hosts []remoteHost
	var diagnostics []Check
	for _, envName := range sortedEnvironmentNames(cfg) {
		env := cfg.Environments[envName]
		resolved, ok, err := provisionedHosts(store, envName, env)
		if err != nil {
			diagnostics = append(diagnostics, fail("remote hosts:"+envName, err.Error()))
			continue
		}
		if ok {
			hosts = append(hosts, resolved...)
			continue
		}
		hosts = append(hosts, explicitHostsForEnvironment(envName, env)...)
	}
	return hosts, diagnostics
}

func provisionedHosts(store state.Store, envName string, env config.Environment) ([]remoteHost, bool, error) {
	facts, err := store.ReadHostFacts(envName)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(facts) == 0 {
		return nil, false, nil
	}
	hosts, err := applyHostFacts(envName, scheduler.HostsForEnvironment(env), facts)
	if err != nil {
		return nil, true, err
	}
	resolved := make([]remoteHost, 0, len(hosts))
	for _, host := range hosts {
		resolved = append(resolved, remoteHost{Environment: envName, Host: host})
	}
	return resolved, true, nil
}

func explicitHostsForEnvironment(envName string, env config.Environment) []remoteHost {
	var hosts []remoteHost
	poolNames := make([]string, 0, len(env.Hosts.Pools))
	for poolName := range env.Hosts.Pools {
		poolNames = append(poolNames, poolName)
	}
	sort.Strings(poolNames)
	for _, poolName := range poolNames {
		pool := env.Hosts.Pools[poolName]
		if len(pool.Hosts) == 0 {
			continue
		}
		user := pool.User
		if user == "" {
			user = "root"
		}
		names := append([]string(nil), pool.Hosts...)
		sort.Strings(names)
		for _, name := range names {
			hosts = append(hosts, remoteHost{
				Environment: envName,
				Host:        scheduler.Host{Name: name, Pool: poolName, User: user},
			})
		}
	}
	return hosts
}

type hostFactKey struct {
	name string
	pool string
}

func applyHostFacts(envName string, hosts []scheduler.Host, facts []state.HostFact) ([]scheduler.Host, error) {
	factsByKey := map[hostFactKey]state.HostFact{}
	for _, fact := range facts {
		key := hostFactKey{name: strings.TrimSpace(fact.Name), pool: strings.TrimSpace(fact.Pool)}
		if key.name == "" || key.pool == "" {
			return nil, fmt.Errorf("host facts for %s contain a host without name or pool", envName)
		}
		if _, exists := factsByKey[key]; exists {
			return nil, fmt.Errorf("host facts for %s contain duplicate host %s in pool %s", envName, key.name, key.pool)
		}
		factsByKey[key] = fact
	}

	resolved := append([]scheduler.Host(nil), hosts...)
	matched := map[hostFactKey]struct{}{}
	var missing []string
	for i, host := range resolved {
		key := hostFactKey{name: host.Name, pool: host.Pool}
		fact, ok := factsByKey[key]
		if !ok {
			missing = append(missing, fmt.Sprintf("%s pool=%s", host.Name, host.Pool))
			continue
		}
		resolved[i].Contact = hostFactContact(fact)
		matched[key] = struct{}{}
	}

	var extra []string
	for key := range factsByKey {
		if _, ok := matched[key]; ok {
			continue
		}
		extra = append(extra, fmt.Sprintf("%s pool=%s", key.name, key.pool))
	}
	if len(missing) > 0 || len(extra) > 0 {
		sort.Strings(missing)
		sort.Strings(extra)
		parts := []string{}
		if len(missing) > 0 {
			parts = append(parts, "missing "+strings.Join(missing, ", "))
		}
		if len(extra) > 0 {
			parts = append(parts, "extra "+strings.Join(extra, ", "))
		}
		return nil, fmt.Errorf("host facts for %s do not match configured hosts: %s; run ship provision apply %s --yes", envName, strings.Join(parts, "; "), envName)
	}
	return resolved, nil
}

func hostFactContact(fact state.HostFact) string {
	if contact := strings.TrimSpace(fact.PublicAddress); contact != "" {
		return contact
	}
	return strings.TrimSpace(fact.IPv4)
}

func localStateDir(configPath string) string {
	if strings.TrimSpace(configPath) == "" {
		configPath = config.DefaultConfigFile
	}
	absPath, err := filepath.Abs(configPath)
	if err != nil {
		return config.LocalStateDir
	}
	return filepath.Join(filepath.Dir(absPath), config.LocalStateDir)
}

func explicitHosts(cfg *config.Config) []remoteHost {
	var hosts []remoteHost
	for _, envName := range sortedEnvironmentNames(cfg) {
		hosts = append(hosts, explicitHostsForEnvironment(envName, cfg.Environments[envName])...)
	}
	return hosts
}

func resolvePath(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(base, path))
}

func pass(name, message string) Check {
	return Check{Name: name, Status: StatusPass, Message: message}
}

func warn(name, message string) Check {
	return Check{Name: name, Status: StatusWarn, Message: message}
}

func fail(name, message string) Check {
	return Check{Name: name, Status: StatusFail, Message: message}
}

type sshRemote struct{}

func (sshRemote) Run(ctx context.Context, host scheduler.Host, command string) (string, error) {
	return (transport.SSH{User: host.User, Host: host.ContactTarget()}).Run(ctx, command)
}
