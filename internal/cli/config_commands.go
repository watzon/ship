package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/cli/ui"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/doctor"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/shipbinary"
	"github.com/watzon/ship/internal/state"
	"gopkg.in/yaml.v3"
)

func configCmd(opts *options) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "config ENV",
		Short: "Show the resolved Ship config for an environment",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			value, err := resolvedConfigValue(cfg, args[0])
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), value)
			}
			doc, err := yaml.Marshal(value)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(doc)
			return err
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print resolved config as JSON")
	return cmd
}

func resolvedConfigValue(cfg *config.Config, envName string) (map[string]any, error) {
	resolved, _, err := cfg.ResolveEnvironment(envName)
	if err != nil {
		return nil, err
	}
	value, ok := compactConfigValue(reflect.ValueOf(resolved), false)
	if !ok {
		return map[string]any{}, nil
	}
	out, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("resolved config rendered as %T", value)
	}
	return out, nil
}

func compactConfigValue(value reflect.Value, keepZero bool) (any, bool) {
	if !value.IsValid() {
		return nil, false
	}
	for value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil, false
		}
		value = value.Elem()
		keepZero = true
	}
	switch value.Kind() {
	case reflect.Struct:
		out := map[string]any{}
		t := value.Type()
		for i := 0; i < value.NumField(); i++ {
			field := t.Field(i)
			if field.PkgPath != "" {
				continue
			}
			name := yamlFieldName(field)
			if name == "" {
				continue
			}
			if child, ok := compactConfigValue(value.Field(i), false); ok {
				out[name] = child
			}
		}
		if len(out) == 0 && !keepZero {
			return nil, false
		}
		return out, true
	case reflect.Map:
		if value.Len() == 0 && !keepZero {
			return nil, false
		}
		out := map[string]any{}
		keys := value.MapKeys()
		sort.Slice(keys, func(i, j int) bool {
			return fmt.Sprint(keys[i].Interface()) < fmt.Sprint(keys[j].Interface())
		})
		for _, key := range keys {
			if child, ok := compactConfigValue(value.MapIndex(key), false); ok {
				out[fmt.Sprint(key.Interface())] = child
			}
		}
		if len(out) == 0 && !keepZero {
			return nil, false
		}
		return out, true
	case reflect.Slice, reflect.Array:
		if value.Len() == 0 && !keepZero {
			return nil, false
		}
		out := make([]any, 0, value.Len())
		for i := 0; i < value.Len(); i++ {
			if child, ok := compactConfigValue(value.Index(i), false); ok {
				out = append(out, child)
			}
		}
		if len(out) == 0 && !keepZero {
			return nil, false
		}
		return out, true
	case reflect.String:
		if value.String() == "" && !keepZero {
			return nil, false
		}
		return value.String(), true
	case reflect.Bool:
		if !value.Bool() && !keepZero {
			return nil, false
		}
		return value.Bool(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if value.Int() == 0 && !keepZero {
			return nil, false
		}
		return value.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if value.Uint() == 0 && !keepZero {
			return nil, false
		}
		return value.Uint(), true
	case reflect.Float32, reflect.Float64:
		if value.Float() == 0 && !keepZero {
			return nil, false
		}
		return value.Float(), true
	default:
		if value.IsZero() && !keepZero {
			return nil, false
		}
		return value.Interface(), true
	}
}

func yamlFieldName(field reflect.StructField) string {
	tag := field.Tag.Get("yaml")
	if tag == "-" {
		return ""
	}
	if tag != "" {
		name, _, _ := strings.Cut(tag, ",")
		return name
	}
	return strings.ToLower(field.Name)
}

func initCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create ship.yml and local Ship state",
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := os.Stat(opts.configPath); err == nil {
				return fmt.Errorf("%s already exists", opts.configPath)
			}
			if err := os.WriteFile(opts.configPath, []byte(config.Sample()), 0o644); err != nil {
				return err
			}
			if err := os.MkdirAll(config.LocalStateDir, 0o755); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Join(config.LocalStateDir, "secrets"), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(config.LocalStateDir, "secrets.example"), []byte("DATABASE_URL=\n"), 0o644); err != nil {
				return err
			}
			if err := ensureShipGitignore(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created %s and %s/\n", opts.configPath, config.LocalStateDir)
			return nil
		},
	}
}

func ensureShipGitignore() error {
	const block = `
.ship/*
!.ship/secrets/
!.ship/secrets/*.age
!.ship/secrets/*.recipients
.ship/secrets/*.env
.ship/secrets/*.identity
.ship/secrets/*key*
`
	data, err := os.ReadFile(".gitignore")
	if errors.Is(err, os.ErrNotExist) {
		return os.WriteFile(".gitignore", []byte(strings.TrimPrefix(block, "\n")), 0o644)
	}
	if err != nil {
		return err
	}
	if strings.Contains(string(data), "!.ship/secrets/*.age") {
		return nil
	}
	f, err := os.OpenFile(".gitignore", os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if len(data) > 0 && data[len(data)-1] != '\n' {
		if _, err := f.WriteString("\n"); err != nil {
			return err
		}
	}
	_, err = f.WriteString(block)
	return err
}

func doctorCmd(opts *options) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "doctor [ENV]",
		Short: "Validate local tools, config, and credentials",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			var report doctor.Report
			envName := ""
			if len(args) > 0 {
				envName = args[0]
			}
			if err != nil {
				report = doctor.ConfigLoadError(err)
			} else {
				report = doctor.Run(cmd.Context(), cfg, doctor.Options{ConfigPath: opts.configPath, Environment: envName})
			}
			if jsonOutput {
				if err := report.WriteJSON(cmd.OutOrStdout()); err != nil {
					return err
				}
			} else {
				report.WriteText(cmd.OutOrStdout())
			}
			if report.Failed() {
				return fmt.Errorf("doctor found issues")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print doctor results as JSON")
	return cmd
}

type hostsView struct {
	Environment string      `json:"environment"`
	Source      string      `json:"source"`
	Hosts       []hostEntry `json:"hosts"`
}

type hostEntry struct {
	Name           string            `json:"name"`
	Pool           string            `json:"pool"`
	User           string            `json:"user"`
	Contact        string            `json:"contact"`
	SSHPort        int               `json:"ssh_port,omitempty"`
	IdentityFile   string            `json:"identity_file,omitempty"`
	KnownHostsFile string            `json:"known_hosts_file,omitempty"`
	JumpHost       string            `json:"jump_host,omitempty"`
	SSHOptions     map[string]string `json:"ssh_options,omitempty"`
}

func hostsCmd(opts *options) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "hosts ENV",
		Short: "Show resolved host inventory for an environment",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			_, env, err := cfg.ResolveEnvironment(args[0])
			if err != nil {
				return err
			}
			stateDir, err := localStateDirForConfig(opts.configPath)
			if err != nil {
				return err
			}
			view, err := buildHostsView(state.NewStore(stateDir), args[0], env)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), view)
			}
			renderHostsText(cmd.OutOrStdout(), view)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print hosts as JSON")
	return cmd
}

func buildHostsView(store state.Store, envName string, env config.Environment) (hostsView, error) {
	source := "config"
	if _, err := store.ReadHostFacts(envName); err == nil {
		source = "state"
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return hostsView{}, err
	}
	hosts, err := resolvedHostsForEnvironment(store, envName, env)
	if err != nil {
		return hostsView{}, err
	}
	view := hostsView{Environment: envName, Source: source}
	for _, host := range hosts {
		view.Hosts = append(view.Hosts, hostEntry{
			Name:           host.Name,
			Pool:           host.Pool,
			User:           host.User,
			Contact:        host.ContactTarget(),
			SSHPort:        host.SSHPort,
			IdentityFile:   host.IdentityFile,
			KnownHostsFile: host.KnownHostsFile,
			JumpHost:       host.JumpHost,
			SSHOptions:     copyStringMap(host.SSHOptions),
		})
	}
	return view, nil
}

func renderHostsText(w io.Writer, view hostsView) {
	ui.PrintHeader(w, view.Environment, ui.HeaderField{Label: "source", Value: view.Source, Accent: true})
	if len(view.Hosts) == 0 {
		ui.PrintNotice(w, "no hosts")
		return
	}
	table := ui.NewTable(w)
	table.SetHeaders("NAME", "POOL", "USER", "CONTACT", "SSH", "NOTES")
	for _, host := range view.Hosts {
		ssh := "-"
		if host.SSHPort > 0 {
			ssh = fmt.Sprintf(":%d", host.SSHPort)
		}
		var notes []string
		if host.IdentityFile != "" {
			notes = append(notes, "identity="+host.IdentityFile)
		}
		if host.KnownHostsFile != "" {
			notes = append(notes, "known_hosts="+host.KnownHostsFile)
		}
		if host.JumpHost != "" {
			notes = append(notes, "jump="+host.JumpHost)
		}
		if len(host.SSHOptions) > 0 {
			notes = append(notes, "options="+formatStringMap(host.SSHOptions))
		}
		table.AddRow(host.Name, host.Pool, host.User, host.Contact, ssh, ui.Dash(strings.Join(notes, " ")))
	}
	ui.RenderTable(w, table)
}

func formatStringMap(values map[string]string) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+values[key])
	}
	return strings.Join(parts, ",")
}

type versionView struct {
	ShipVersion      string             `json:"ship_version"`
	MinAgentProtocol int                `json:"min_agent_protocol"`
	MaxAgentProtocol int                `json:"max_agent_protocol"`
	Environment      string             `json:"environment,omitempty"`
	Hosts            []versionHostEntry `json:"hosts,omitempty"`
}

type versionHostEntry struct {
	Name             string   `json:"name"`
	Pool             string   `json:"pool"`
	Contact          string   `json:"contact"`
	Hostname         string   `json:"hostname,omitempty"`
	DockerOK         bool     `json:"docker_ok,omitempty"`
	StateDir         string   `json:"state_dir,omitempty"`
	AgentVersion     string   `json:"agent_version,omitempty"`
	AgentProtocol    int      `json:"agent_protocol,omitempty"`
	SupportedMethods []string `json:"supported_methods,omitempty"`
	Error            string   `json:"error,omitempty"`
}

func versionCmd(opts *options) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "version [ENV]",
		Short: "Show local Ship and remote agent versions",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			view := localVersionView()
			if len(args) == 1 {
				_, env, store, err := environmentContext(opts, args[0])
				if err != nil {
					return err
				}
				view, err = buildVersionView(cmd.Context(), store, args[0], env)
				if err != nil {
					return err
				}
			}
			if jsonOutput {
				if err := writeJSON(cmd.OutOrStdout(), view); err != nil {
					return err
				}
			} else {
				renderVersionText(cmd.OutOrStdout(), view)
			}
			if failed := countVersionFailures(view); failed > 0 {
				return fmt.Errorf("version check failed on %d/%d hosts", failed, len(view.Hosts))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print version information as JSON")
	return cmd
}

func localVersionView() versionView {
	return versionView{
		ShipVersion:      agent.Version(),
		MinAgentProtocol: agent.AgentMinProtocol,
		MaxAgentProtocol: agent.AgentProtocol,
	}
}

func buildVersionView(ctx context.Context, store state.Store, envName string, env config.Environment) (versionView, error) {
	hosts, err := resolvedHostsForEnvironment(store, envName, env)
	if err != nil {
		return versionView{}, err
	}
	view := localVersionView()
	view.Environment = envName
	for _, host := range hosts {
		entry := versionHostEntry{
			Name:    host.Name,
			Pool:    host.Pool,
			Contact: host.ContactTarget(),
		}
		var status agent.Status
		if err := newDeployAgent(host).Call(ctx, "status", map[string]any{}, &status); err != nil {
			entry.Error = err.Error()
			view.Hosts = append(view.Hosts, entry)
			continue
		}
		entry.Hostname = status.Hostname
		entry.DockerOK = status.DockerOK
		entry.StateDir = status.StateDir
		entry.AgentVersion = status.AgentVersion
		entry.AgentProtocol = status.ProtocolVersion
		entry.SupportedMethods = append([]string(nil), status.SupportedMethods...)
		view.Hosts = append(view.Hosts, entry)
	}
	return view, nil
}

func renderVersionText(w io.Writer, view versionView) {
	style := ui.NewStyle(w)
	fmt.Fprint(w, style.Teal("ship "))
	fmt.Fprint(w, style.White(view.ShipVersion))
	fmt.Fprint(w, style.Gray("  protocol "))
	fmt.Fprintln(w, style.White(fmt.Sprintf("%d-%d", view.MinAgentProtocol, view.MaxAgentProtocol)))
	if view.Environment == "" {
		return
	}
	ui.PrintHeader(w, view.Environment)
	if len(view.Hosts) == 0 {
		ui.PrintNotice(w, "no hosts")
		return
	}
	table := ui.NewTable(w)
	table.SetHeaders("HOST", "POOL", "CONTACT", "AGENT", "DOCKER", "STATE", "DETAIL")
	for _, host := range view.Hosts {
		if host.Error != "" {
			table.AddRow(host.Name, host.Pool, host.Contact, "-", "-", "-", host.Error)
			continue
		}
		agentVersion := host.AgentVersion
		if agentVersion == "" {
			agentVersion = "unknown"
		}
		if host.AgentProtocol > 0 {
			agentVersion = fmt.Sprintf("%s (%d)", agentVersion, host.AgentProtocol)
		}
		detail := ui.Dash(host.Hostname)
		if len(host.SupportedMethods) > 0 {
			detail = strings.Join(host.SupportedMethods, ",")
		}
		table.AddRow(host.Name, host.Pool, host.Contact, agentVersion, fmt.Sprintf("%t", host.DockerOK), ui.Dash(host.StateDir), detail)
	}
	ui.RenderTable(w, table)
}

func countVersionFailures(view versionView) int {
	var failed int
	for _, host := range view.Hosts {
		if host.Error != "" {
			failed++
		}
	}
	return failed
}

func agentCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{Use: "agent", Short: "Manage or run the Ship node agent"}
	cmd.AddCommand(&cobra.Command{
		Use:    "rpc",
		Short:  "Serve one or more JSON-RPC requests over stdin/stdout",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return agent.ServeStdio(cmd.Context())
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "run",
		Short: "Report agent install status (RPC is served on demand, not by a daemon)",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "ship agent is installed; RPC is served on demand through `ship agent rpc` over SSH, so no daemon runs")
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "install ENV",
		Short: "Print or run host bootstrap commands for every host in an environment",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, env, store, err := environmentContext(opts, args[0])
			if err != nil {
				return err
			}
			hosts, err := resolvedHostsForEnvironment(store, args[0], env)
			if err != nil {
				return err
			}
			for _, host := range hosts {
				unit := systemdUnit()
				command := fmt.Sprintf("mkdir -p %s && cat >/etc/systemd/system/ship-agent.service <<'EOF'\n%s\nEOF\nsystemctl daemon-reload && systemctl enable --now ship-agent", config.RemoteStateDir, unit)
				out, err := sshForHost(host, opts.dryRun).Run(cmd.Context(), command)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", host.Name, strings.TrimSpace(out))
			}
			return nil
		},
	})
	var upgradeJSON bool
	upgrade := &cobra.Command{
		Use:   "upgrade ENV",
		Short: "Upload the current Ship binary to every host agent",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			view, err := upgradeAgents(cmd.Context(), opts, args[0])
			if upgradeJSON {
				if writeErr := writeJSON(cmd.OutOrStdout(), view); writeErr != nil {
					return writeErr
				}
			} else {
				renderAgentUpgradeText(cmd.OutOrStdout(), view)
			}
			if err != nil {
				return err
			}
			if failed := countAgentUpgradeFailures(view); failed > 0 {
				return fmt.Errorf("agent upgrade failed on %d/%d hosts", failed, len(view.Hosts))
			}
			return nil
		},
	}
	upgrade.Flags().BoolVar(&upgradeJSON, "json", false, "print upgrade results as JSON")
	addAgentBinaryOverrideFlags(upgrade, opts)
	cmd.AddCommand(upgrade)
	cmd.AddCommand(&cobra.Command{
		Use:   "status ENV",
		Short: "Ask every host agent for status",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, env, store, err := environmentContext(opts, args[0])
			if err != nil {
				return err
			}
			hosts, err := resolvedHostsForEnvironment(store, args[0], env)
			if err != nil {
				return err
			}
			renderAgentStatusText(cmd.OutOrStdout(), args[0], hosts, func(host scheduler.Host) (agent.Status, error) {
				var status agent.Status
				client := agent.Client{SSH: sshForHost(host, opts.dryRun)}
				if err := client.Call(cmd.Context(), "status", map[string]any{}, &status); err != nil {
					return agent.Status{}, err
				}
				return status, nil
			})
			return nil
		},
	})
	return cmd
}

func releaseCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "release", Short: "Inspect Ship's own release assets"}
	var jsonOutput bool
	check := &cobra.Command{
		Use:   "check VERSION",
		Short: "Verify a Ship release published binaries for every supported platform",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			statuses, allOK, err := shipbinary.CheckRelease(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if jsonOutput {
				if err := writeJSON(cmd.OutOrStdout(), statuses); err != nil {
					return err
				}
			} else {
				table := ui.NewTable(cmd.OutOrStdout())
				table.SetHeaders("ASSET", "STATUS", "DETAIL")
				for _, status := range statuses {
					result := "ok"
					if !status.OK {
						result = "missing"
					}
					table.AddRow(status.Name, result, ui.Dash(status.Detail))
				}
				ui.RenderTable(cmd.OutOrStdout(), table)
			}
			if !allOK {
				return fmt.Errorf("release v%s is missing assets; do not pin it for agent installs", strings.TrimPrefix(strings.TrimSpace(args[0]), "v"))
			}
			return nil
		},
	}
	check.Flags().BoolVar(&jsonOutput, "json", false, "print asset statuses as JSON")
	cmd.AddCommand(check)
	return cmd
}

func renderAgentStatusText(w io.Writer, envName string, hosts []scheduler.Host, fetch func(scheduler.Host) (agent.Status, error)) {
	ui.PrintHeader(w, envName)
	if len(hosts) == 0 {
		ui.PrintNotice(w, "no hosts")
		return
	}
	table := ui.NewTable(w)
	table.SetHeaders("HOST", "DOCKER", "STATE DIR", "AGENT", "DETAIL")
	for _, host := range hosts {
		status, err := fetch(host)
		if err != nil {
			table.AddRow(host.Name, "-", "-", "-", err.Error())
			continue
		}
		table.AddRow(host.Name, fmt.Sprintf("%t", status.DockerOK), ui.Dash(status.StateDir), ui.Dash(status.AgentVersion), ui.Dash(status.Hostname))
	}
	ui.RenderTable(w, table)
}

func renderAgentUpgradeText(w io.Writer, view agentUpgradeView) {
	fields := []ui.HeaderField{
		{Label: "version", Value: view.ShipVersion, Accent: true},
		{Label: "sha256", Value: view.SHA256},
	}
	if view.DryRun {
		fields = append(fields, ui.HeaderField{Label: "mode", Value: "dry-run"})
	}
	ui.PrintHeader(w, view.Environment, fields...)
	if len(view.Hosts) == 0 {
		ui.PrintNotice(w, "no hosts")
		return
	}
	table := ui.NewTable(w)
	table.SetHeaders("HOST", "POOL", "CONTACT", "RESULT", "PATH", "SHA256")
	for _, host := range view.Hosts {
		result := "unchanged"
		if host.Error != "" {
			result = "failed"
		} else if view.DryRun {
			result = "planned"
		} else if host.Installed {
			result = "installed"
		}
		detail := ui.Dash(host.Error)
		if host.Error == "" && view.DryRun {
			detail = "would install"
		}
		if host.Error != "" {
			table.AddRow(host.Name, host.Pool, host.Contact, result, ui.Dash(host.Path), detail)
			continue
		}
		table.AddRow(host.Name, host.Pool, host.Contact, result, ui.Dash(host.Path), ui.Dash(host.SHA256))
	}
	ui.RenderTable(w, table)
}

func countAgentUpgradeFailures(view agentUpgradeView) int {
	var failed int
	for _, host := range view.Hosts {
		if host.Error != "" {
			failed++
		}
	}
	return failed
}

// systemdUnit is an install marker, not a daemon: Ship's RPC is served by
// execing `ship agent rpc` over each SSH session, so there is no long-running
// process to supervise. `ship agent run` prints a notice and exits 0, which
// under Type=oneshot + RemainAfterExit leaves the unit "active (exited)" —
// a Restart=always unit here would loop forever.
func systemdUnit() string {
	return `[Unit]
Description=Ship node agent (RPC served on demand via SSH; no daemon)
Documentation=https://github.com/watzon/ship
After=docker.service
Requires=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=` + config.RemoteBinaryPath + ` agent run

[Install]
WantedBy=multi-user.target`
}
