package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/watzon/ship/internal/cli/ui"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/planner"
	"github.com/watzon/ship/internal/provider"
	"github.com/watzon/ship/internal/provider/providers"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/state"
)

var newEnvironmentProvider = providers.ForEnvironment

func provisionCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{Use: "provision", Short: "Plan or apply server provisioning"}
	var planJSON bool
	plan := &cobra.Command{
		Use:   "plan ENV",
		Short: "Print the provisioning plan",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			plan, err := planner.ProvisionPlan(cfg, args[0])
			if err != nil {
				return err
			}
			if planJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(plan)
			}
			fmt.Fprint(cmd.OutOrStdout(), plan.String())
			return nil
		},
	}
	plan.Flags().BoolVar(&planJSON, "json", false, "print the provisioning plan as JSON")
	cmd.AddCommand(plan)
	var yes bool
	apply := &cobra.Command{
		Use:   "apply ENV",
		Short: "Create servers and bootstrap Ship",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			resolved, env, err := cfg.ResolveEnvironment(args[0])
			if err != nil {
				return err
			}
			cfg = resolved
			if !yes && !opts.dryRun {
				return fmt.Errorf("provision apply requires --yes (or --dry-run) before creating servers")
			}
			prov, err := newEnvironmentProvider(env, opts.dryRun)
			if err != nil {
				return err
			}
			if opts.dryRun {
				plans, err := prov.PlanHosts(cfg.Project, args[0], env)
				if err != nil {
					return err
				}
				for _, host := range plans {
					fmt.Fprintf(cmd.OutOrStdout(), "would provision %s pool=%s\n", host.Name, host.Pool)
				}
				return nil
			}
			stateDir, err := localStateDirForConfig(opts.configPath)
			if err != nil {
				return err
			}
			store := state.NewStore(stateDir)
			recordEvent(store, state.Event{Environment: args[0], Kind: "provision", Status: "started"})
			result, err := prov.Reconcile(ctx, cfg.Project, args[0], env)
			if err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "provision", Status: "failed", Message: err.Error()})
				return err
			}
			for _, host := range result.Existing {
				printProviderHost(cmd.OutOrStdout(), "exists", host)
			}
			for _, host := range result.Created {
				printProviderHost(cmd.OutOrStdout(), "created", host)
			}
			for _, host := range result.Extra {
				printProviderHostDetails(cmd.OutOrStdout(), "extra", host)
				fmt.Fprintln(cmd.OutOrStdout(), " (not deleted)")
			}
			facts := hostFactsFromReconcile(prov.Name(), result)
			if err := store.SaveHostFacts(args[0], facts); err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "provision", Status: "failed", Message: err.Error()})
				return err
			}
			hosts, err := applyHostFacts(args[0], scheduler.HostsForEnvironment(env), facts)
			if err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "provision", Status: "failed", Message: err.Error()})
				return err
			}
			for _, host := range hosts {
				shipBinary, err := resolveShipBinaryForHost(ctx, host, opts)
				if err != nil {
					recordEvent(store, state.Event{Environment: args[0], Kind: "provision", Status: "failed", Host: host.Name, Message: err.Error()})
					return fmt.Errorf("resolve ship binary for %s: %w", host.Name, err)
				}
				if err := bootstrapHost(ctx, host, shipBinary, opts.dryRun); err != nil {
					recordEvent(store, state.Event{Environment: args[0], Kind: "provision", Status: "failed", Host: host.Name, Message: err.Error()})
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "bootstrapped %s\n", host.Name)
			}
			recordEvent(store, state.Event{Environment: args[0], Kind: "provision", Status: "succeeded", Message: fmt.Sprintf("created=%d existing=%d extra=%d", len(result.Created), len(result.Existing), len(result.Extra))})
			return nil
		},
	}
	apply.Flags().BoolVar(&yes, "yes", false, "confirm provisioning changes")
	addAgentBinaryOverrideFlags(apply, opts)
	cmd.AddCommand(apply)
	var decommissionYes bool
	decommission := &cobra.Command{
		Use:   "decommission ENV",
		Short: "Delete Ship-managed servers for an environment",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			resolved, env, err := cfg.ResolveEnvironment(args[0])
			if err != nil {
				return err
			}
			cfg = resolved
			if !decommissionYes && !opts.dryRun {
				return fmt.Errorf("provision decommission requires --yes (or --dry-run) before deleting servers")
			}
			prov, err := newEnvironmentProvider(env, opts.dryRun)
			if err != nil {
				return err
			}
			stateDir, err := localStateDirForConfig(opts.configPath)
			if err != nil {
				return err
			}
			store := state.NewStore(stateDir)
			hosts, err := prov.List(ctx, cfg.Project, args[0])
			if err != nil {
				return err
			}
			if opts.dryRun {
				if len(hosts) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "would decommission no servers")
					return nil
				}
				for _, host := range hosts {
					printProviderHost(cmd.OutOrStdout(), "would decommission", host)
				}
				return nil
			}
			recordEvent(store, state.Event{Environment: args[0], Kind: "decommission", Status: "started"})
			for _, host := range hosts {
				if err := prov.Delete(ctx, host); err != nil {
					recordEvent(store, state.Event{Environment: args[0], Kind: "decommission", Status: "failed", Host: host.Name, Message: err.Error()})
					return err
				}
				printProviderHost(cmd.OutOrStdout(), "decommissioned", host)
			}
			if err := store.DeleteHostFacts(args[0]); err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "decommission", Status: "failed", Message: err.Error()})
				return err
			}
			recordEvent(store, state.Event{Environment: args[0], Kind: "decommission", Status: "succeeded", Message: fmt.Sprintf("deleted=%d", len(hosts))})
			return nil
		},
	}
	decommission.Flags().BoolVar(&decommissionYes, "yes", false, "confirm deletion of Ship-managed servers")
	cmd.AddCommand(decommission)
	return cmd
}

func printProviderHost(w io.Writer, action string, host provider.Host) {
	printProviderHostDetails(w, action, host)
	fmt.Fprintln(w)
}

func printProviderHostDetails(w io.Writer, action string, host provider.Host) {
	pool := host.Pool
	if pool == "" {
		pool = host.Labels[provider.LabelPool]
	}
	fmt.Fprintf(w, "%s %s", action, host.Name)
	if strings.Contains(action, "decommission") {
		printProviderID(w, host.ID)
		if pool != "" {
			fmt.Fprintf(w, " pool=%s", pool)
		}
	} else {
		if pool != "" {
			fmt.Fprintf(w, " pool=%s", pool)
		}
		printProviderID(w, host.ID)
	}
	if host.PublicAddress != "" {
		if ip := net.ParseIP(host.PublicAddress); ip != nil && ip.To4() != nil {
			fmt.Fprintf(w, " ipv4=%s", host.PublicAddress)
		} else {
			fmt.Fprintf(w, " public_address=%s", host.PublicAddress)
		}
	}
}

func printProviderID(w io.Writer, id string) {
	if id == "" {
		return
	}
	if _, err := strconv.ParseInt(id, 10, 64); err == nil {
		fmt.Fprintf(w, " server_id=%s", id)
		return
	}
	fmt.Fprintf(w, " provider_id=%s", id)
}

func hostFactsFromReconcile(providerName string, result provider.ReconcileResult) []state.HostFact {
	hostsByLogical := map[string]provider.Host{}
	for _, host := range result.Existing {
		hostsByLogical[provider.LogicalName(host)] = host
	}
	for _, host := range result.Created {
		hostsByLogical[provider.LogicalName(host)] = host
	}

	facts := make([]state.HostFact, 0, len(result.Desired))
	for _, plan := range result.Desired {
		fact := state.HostFact{
			Name:     plan.Name,
			Pool:     plan.Pool,
			User:     plan.User,
			Provider: providerName,
		}
		if host, ok := hostsByLogical[plan.Name]; ok {
			applyProviderHostToFact(&fact, host)
		}
		facts = append(facts, fact)
	}
	return facts
}

func applyProviderHostToFact(fact *state.HostFact, host provider.Host) {
	fact.ProviderID = host.ID
	if id, err := strconv.ParseInt(host.ID, 10, 64); err == nil {
		fact.ServerID = id
	}
	if host.Name != fact.Name {
		fact.ProviderName = host.Name
	} else {
		fact.ProviderName = ""
	}
	fact.SSHPort = host.SSHPort
	fact.IdentityFile = host.IdentityFile
	fact.KnownHostsFile = host.KnownHostsFile
	fact.JumpHost = host.JumpHost
	fact.SSHOptions = copyStringMap(host.SSHOptions)
	fact.PublicAddress = host.PublicAddress
	fact.IPv4 = ""
	if ip := net.ParseIP(host.PublicAddress); ip != nil && ip.To4() != nil {
		fact.IPv4 = host.PublicAddress
	}
}
