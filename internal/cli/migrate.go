package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/watzon/ship/internal/accessory"
	"github.com/watzon/ship/internal/cli/ui"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/deployment"
	"github.com/watzon/ship/internal/provider"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/secrets"
	"github.com/watzon/ship/internal/state"
)

var copyRemoteArtifact = func(ctx context.Context, source scheduler.Host, readCommand string, dst scheduler.Host, writeCommand string, dryRun bool) error {
	return sshForHost(source, dryRun).CopyTo(ctx, readCommand, sshForHost(dst, dryRun), writeCommand)
}

var uploadLocalArtifact = func(ctx context.Context, dst scheduler.Host, localPath, writeCommand string, dryRun bool) error {
	return sshForHost(dst, dryRun).CopyFromLocal(ctx, localPath, writeCommand)
}

func migrateCmd(opts *options) *cobra.Command {
	var yes bool
	var keepServer bool
	var artifactOverrides []string
	cmd := &cobra.Command{
		Use:   "migrate ENV HOST",
		Short: "Move a host's workloads onto a freshly provisioned replacement server",
		Long: `Migrate replaces the server behind a logical host with a freshly provisioned
one and moves everything across: the replacement is created from the current
pool settings (so a changed size, image, or location takes effect), the Ship
agent is installed, accessories are backed up on the old server and restored on
the new one, service replicas are started from the current release with health
checks, ingress upstreams are updated, and the old server is stopped and
deleted.

The logical host keeps its name and placement; only the machine behind it
changes. Requires a provider that can create servers.`,
		Args: ui.ExactArgs(ui.Env, ui.ArgNamed("HOST", "logical host name from the environment inventory (e.g. web-1)")),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigrate(cmd.Context(), cmd.OutOrStdout(), opts, args[0], args[1], yes, keepServer, artifactOverrides)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the migration")
	cmd.Flags().BoolVar(&keepServer, "keep-server", false, "stop workloads on the old server but do not delete it")
	cmd.Flags().StringArrayVar(&artifactOverrides, "artifact", nil, "restore an accessory from a local backup artifact instead of taking a fresh one (NAME=PATH, repeatable)")
	addAgentBinaryOverrideFlags(cmd, opts)
	return cmd
}

type migratePlan struct {
	source      scheduler.Host
	plan        provider.HostPlan
	services    []string
	replicas    int
	accessories []string
	ingress     bool
}

func runMigrate(ctx context.Context, w io.Writer, opts *options, envName, hostName string, yes, keepServer bool, artifactOverrides []string) error {
	cfg, env, store, err := environmentContext(opts, envName)
	if err != nil {
		return err
	}
	stateDir, err := localStateDirForConfig(opts.configPath)
	if err != nil {
		return err
	}
	hosts, err := resolvedHostsForEnvironment(store, envName, env)
	if err != nil {
		return err
	}
	if !yes && !opts.dryRun {
		return fmt.Errorf("migrate requires --yes (or --dry-run) before replacing a server")
	}
	prov, err := newEnvironmentProvider(env, opts.dryRun)
	if err != nil {
		return err
	}
	creator, ok := prov.(provider.HostCreator)
	if !ok {
		return fmt.Errorf("provider %q cannot create servers, so ship migrate cannot provision a replacement; see the migration notes in docs/deploy-and-operate.md for inventory-backed hosts", prov.Name())
	}
	plan, err := buildMigratePlan(cfg, env, store, prov, envName, hostName, hosts)
	if err != nil {
		return err
	}
	overrides, err := parseAccessoryArtifacts(artifactOverrides, plan.accessories)
	if err != nil {
		return err
	}
	if err := validateMigrateAccessories(cfg, plan.accessories); err != nil {
		return err
	}
	printMigrateWarnings(w, cfg, plan)
	if opts.dryRun {
		printMigrateDryRun(w, cfg, store, envName, plan, keepServer)
		recordEvent(store, state.Event{Environment: envName, Kind: "migrate", Status: "planned", Host: hostName, Message: "dry-run"})
		return nil
	}

	operationLock, err := store.AcquireOperationLock(envName, "migrate")
	if err != nil {
		return err
	}
	defer operationLock.Unlock()
	recordEvent(store, state.Event{Environment: envName, Kind: "migrate", Status: "started", Host: hostName})
	cleanupHint := ""
	fail := func(err error) error {
		if cleanupHint != "" {
			fmt.Fprintln(w, cleanupHint)
		}
		recordEvent(store, state.Event{Environment: envName, Kind: "migrate", Status: "failed", Host: hostName, Message: err.Error()})
		runNotifications(ctx, store, cfg, envName, "migrate", "failed", "", err.Error(), nil)
		return err
	}

	// Verify the source agent is reachable before creating anything.
	if err := preflightAgentProtocols(ctx, w, opts, envName, []scheduler.Host{plan.source}, false); err != nil {
		return fail(fmt.Errorf("source host %s is not reachable: %w", hostName, err))
	}

	created, err := creator.CreateHost(ctx, cfg.Project, envName, env, plan.plan)
	if err != nil {
		return fail(fmt.Errorf("create replacement server for %s: %w", hostName, err))
	}
	fmt.Fprintf(w, "provisioned replacement server %s (%s) for host %s\n", created.Name, created.PublicAddress, hostName)
	cleanupHint = fmt.Sprintf("note: replacement server %s (%s) was created and is still in your provider account; delete it there before retrying", created.Name, created.PublicAddress)
	if strings.TrimSpace(created.PublicAddress) == "" {
		return fail(fmt.Errorf("replacement server %s has no public address", created.Name))
	}
	if provider.LogicalName(created) != hostName {
		fmt.Fprintf(w, "warning: provider %s did not persist the %s=%s label; `ship provision apply` will not recognize %s as host %s until the label is added\n",
			prov.Name(), provider.LabelHost, hostName, created.Name, hostName)
	}

	replacement := replacementSchedulerHost(plan.source, created)
	shipBinary, err := resolveShipBinaryForHost(ctx, replacement, opts)
	if err != nil {
		return fail(fmt.Errorf("resolve ship binary for %s: %w", hostName, err))
	}
	if err := bootstrapHost(ctx, replacement, shipBinary, opts.dryRun); err != nil {
		return fail(fmt.Errorf("bootstrap replacement server %s: %w", created.Name, err))
	}
	fmt.Fprintf(w, "bootstrapped %s\n", created.Name)
	if err := preflightAgentProtocols(ctx, w, opts, envName, []scheduler.Host{replacement}, false); err != nil {
		return fail(fmt.Errorf("replacement server %s failed agent preflight: %w", created.Name, err))
	}

	for _, name := range plan.accessories {
		if err := migrateAccessory(ctx, w, opts, cfg, env, envName, store, name, plan.source, replacement, overrides[name]); err != nil {
			return fail(err)
		}
	}

	oldFact, err := repointHostFact(store, envName, plan.source, prov.Name(), created)
	if err != nil {
		return fail(err)
	}
	cleanupHint = fmt.Sprintf("note: host %s now points at %s; the old server (%s) is still running — after fixing the cause, run `ship deploy %s` to converge, then delete the old server with your provider", hostName, created.PublicAddress, plan.source.ContactTarget(), envName)

	current, currentErr := store.CurrentRelease(envName)
	hasRelease := currentErr == nil
	hostsAfter, err := resolvedHostsForEnvironment(store, envName, env)
	if err != nil {
		return fail(err)
	}
	if hasRelease && len(plan.services) > 0 {
		if err := migrateServiceRollout(ctx, opts, cfg, env, store, envName, stateDir, hostsAfter, replacement, current); err != nil {
			return fail(err)
		}
		fmt.Fprintf(w, "started release %s replicas on %s\n", current.ID, hostName)
	}
	if hasRelease && len(plan.accessories) > 0 {
		if err := restartCurrentServicesAfterAccessoryChange(ctx, w, cfg, envName, store, hostsAfter); err != nil {
			return fail(err)
		}
	}

	stopOldWorkloads(ctx, w, cfg, envName, plan.source)

	if keepServer {
		fmt.Fprintf(w, "keeping old server %s (%s); `ship provision apply` will report it as extra until you delete it with your provider\n", oldServerLabel(oldFact, hostName), plan.source.ContactTarget())
	} else if err := deleteOldServer(ctx, w, prov, cfg.Project, envName, oldFact, hostName, created.ID); err != nil {
		return fail(err)
	}

	releaseID := ""
	var images map[string]string
	if hasRelease {
		releaseID = current.ID
		images = current.Images
	}
	recordEvent(store, state.Event{Environment: envName, Kind: "migrate", Status: "succeeded", Host: hostName, Message: fmt.Sprintf("server %s -> %s", plan.source.ContactTarget(), created.PublicAddress)})
	runNotifications(ctx, store, cfg, envName, "migrate", "succeeded", releaseID, fmt.Sprintf("host %s moved to %s", hostName, created.PublicAddress), images)
	fmt.Fprintf(w, "migrated host %s: %s -> %s\n", hostName, plan.source.ContactTarget(), created.PublicAddress)
	if plan.ingress {
		fmt.Fprintf(w, "note: host %s serves ingress traffic; update any DNS records pointing at %s to %s\n", hostName, plan.source.ContactTarget(), created.PublicAddress)
	}
	return nil
}

func buildMigratePlan(cfg *config.Config, env config.Environment, store state.Store, prov provider.Provider, envName, hostName string, hosts []scheduler.Host) (migratePlan, error) {
	plan := migratePlan{}
	found := false
	for _, host := range hosts {
		if host.Name == hostName {
			plan.source = host
			found = true
			break
		}
	}
	if !found {
		names := make([]string, 0, len(hosts))
		for _, host := range hosts {
			names = append(names, host.Name)
		}
		return plan, fmt.Errorf("host %q is not in the %s inventory (hosts: %s)", hostName, envName, strings.Join(names, ", "))
	}

	placements, err := scheduler.PlaceServicesOnHosts(cfg, hosts)
	if err != nil {
		return plan, err
	}
	services := map[string]bool{}
	for _, placement := range placements {
		if placement.Host.Name != hostName {
			continue
		}
		plan.replicas++
		services[placement.Service] = true
		if svc := cfg.Services[placement.Service]; svc.Ingress != nil {
			plan.ingress = true
		}
	}
	for name := range services {
		plan.services = append(plan.services, name)
	}
	sort.Strings(plan.services)

	for _, name := range accessory.SortedNames(cfg, "") {
		placement, err := accessory.PlacementForHosts(cfg, hosts, envName, name, store)
		if err != nil {
			return plan, err
		}
		if placement.Persisted && placement.Host.Name == hostName {
			plan.accessories = append(plan.accessories, name)
		}
	}

	plans, err := prov.PlanHosts(cfg.Project, envName, env)
	if err != nil {
		return plan, err
	}
	for _, hostPlan := range plans {
		if hostPlan.Name == hostName {
			plan.plan = hostPlan
			plan.plan.Name = migrateReplacementName(hostName)
			plan.plan.Labels = copyStringMap(hostPlan.Labels)
			if plan.plan.Labels == nil {
				plan.plan.Labels = map[string]string{}
			}
			plan.plan.Labels[provider.LabelHost] = hostName
			return plan, nil
		}
	}
	return plan, fmt.Errorf("host %q has no provisioning plan for provider %q; migrate only supports provider-managed pools", hostName, prov.Name())
}

func migrateReplacementName(hostName string) string {
	return fmt.Sprintf("%s-m%s", hostName, deployNow().UTC().Format("20060102150405"))
}

func parseAccessoryArtifacts(overrides, moving []string) (map[string]string, error) {
	parsed := map[string]string{}
	movingSet := map[string]bool{}
	for _, name := range moving {
		movingSet[name] = true
	}
	for _, override := range overrides {
		name, path, ok := strings.Cut(override, "=")
		name = strings.TrimSpace(name)
		path = strings.TrimSpace(path)
		if !ok || name == "" || path == "" {
			return nil, fmt.Errorf("invalid --artifact %q, expected NAME=PATH", override)
		}
		if !movingSet[name] {
			return nil, fmt.Errorf("--artifact %s: accessory %q is not placed on the host being migrated", override, name)
		}
		parsed[name] = path
	}
	return parsed, nil
}

func validateMigrateAccessories(cfg *config.Config, moving []string) error {
	var blocked []string
	for _, name := range moving {
		if err := accessory.ValidateRestore(cfg.Accessories[name]); err != nil {
			blocked = append(blocked, fmt.Sprintf("%s (%v)", name, err))
		}
	}
	if len(blocked) > 0 {
		return fmt.Errorf("cannot migrate accessory data for: %s; configure backup/restore commands, or move the accessory another way before migrating", strings.Join(blocked, ", "))
	}
	return nil
}

func printMigrateWarnings(w io.Writer, cfg *config.Config, plan migratePlan) {
	for _, name := range plan.services {
		if len(cfg.Services[name].Volumes) > 0 {
			fmt.Fprintf(w, "warning: service %s uses volumes on %s; volume data is not migrated\n", name, plan.source.Name)
		}
	}
}

func printMigrateDryRun(w io.Writer, cfg *config.Config, store state.Store, envName string, plan migratePlan, keepServer bool) {
	fmt.Fprintf(w, "would provision replacement server %s pool=%s size=%s location=%s image=%s\n",
		plan.plan.Name, plan.plan.Pool, plan.plan.Size, plan.plan.Location, plan.plan.Image)
	fmt.Fprintf(w, "would bootstrap the Ship agent on the replacement\n")
	for _, name := range plan.accessories {
		fmt.Fprintf(w, "would move accessory %s (backup on %s, restore on the replacement)\n", name, plan.source.Name)
	}
	if len(plan.services) > 0 {
		release := "the current release"
		if current, err := store.CurrentRelease(envName); err == nil {
			release = "release " + current.ID
		}
		fmt.Fprintf(w, "would start %d replica(s) of %s from %s and update ingress upstreams\n", plan.replicas, strings.Join(plan.services, ", "), release)
	}
	fmt.Fprintf(w, "would stop Ship-managed workloads on the old server\n")
	if keepServer {
		fmt.Fprintf(w, "would keep the old server (delete it manually with your provider)\n")
	} else {
		fmt.Fprintf(w, "would delete the old server for host %s\n", plan.source.Name)
	}
	if plan.ingress {
		fmt.Fprintf(w, "note: host %s serves ingress traffic; DNS records must be updated to the replacement address afterwards\n", plan.source.Name)
	}
}

func replacementSchedulerHost(source scheduler.Host, created provider.Host) scheduler.Host {
	replacement := source
	replacement.Contact = created.PublicAddress
	if created.SSHPort > 0 {
		replacement.SSHPort = created.SSHPort
	}
	if strings.TrimSpace(created.IdentityFile) != "" {
		replacement.IdentityFile = created.IdentityFile
	}
	if strings.TrimSpace(created.KnownHostsFile) != "" {
		replacement.KnownHostsFile = created.KnownHostsFile
	}
	if strings.TrimSpace(created.JumpHost) != "" {
		replacement.JumpHost = created.JumpHost
	}
	if len(created.SSHOptions) > 0 {
		replacement.SSHOptions = mergeStringMap(replacement.SSHOptions, created.SSHOptions)
	}
	return replacement
}

func migrateAccessory(ctx context.Context, w io.Writer, opts *options, cfg *config.Config, env config.Environment, envName string, store state.Store, name string, source, replacement scheduler.Host, localArtifact string) error {
	acc := cfg.Accessories[name]
	containerName := accessory.ContainerName(cfg.Project, envName, name)
	recordEvent(store, state.Event{Environment: envName, Kind: "migrate_accessory", Status: "started", Accessory: name, Host: replacement.Name})

	var artifact string
	if localArtifact != "" {
		artifact = accessory.BackupArtifactPath(acc, envName, name, deployNow())
		writeCommand := "mkdir -p " + shellQuote(filepath.Dir(artifact)) + " && cat > " + shellQuote(artifact)
		if err := uploadLocalArtifact(ctx, replacement, localArtifact, writeCommand, opts.dryRun); err != nil {
			return fmt.Errorf("upload artifact for accessory %s: %w", name, err)
		}
	} else {
		if err := runAccessoryBackup(ctx, w, opts, cfg, env, envName, store, []string{name}); err != nil {
			return err
		}
		saved, err := store.ReadAccessoryState(envName, name)
		if err != nil {
			return err
		}
		if saved.LastBackup == nil || strings.TrimSpace(saved.LastBackup.Artifact) == "" {
			return fmt.Errorf("accessory %s has no recorded backup artifact after backup", name)
		}
		artifact = saved.LastBackup.Artifact
		readCommand := "cat " + shellQuote(artifact)
		writeCommand := "mkdir -p " + shellQuote(filepath.Dir(artifact)) + " && cat > " + shellQuote(artifact)
		if err := copyRemoteArtifact(ctx, source, readCommand, replacement, writeCommand, opts.dryRun); err != nil {
			return fmt.Errorf("transfer artifact for accessory %s: %w", name, err)
		}
	}
	fmt.Fprintf(w, "transferred backup artifact for accessory %s\n", name)

	result, err := startAccessoryWithRestore(ctx, opts, cfg, envName, name, replacement, artifact)
	if err != nil {
		return err
	}
	if err := newDeployAgent(source).Call(ctx, "stop_container", map[string]string{"name": containerName}, nil); err != nil {
		return fmt.Errorf("stop old accessory %s on %s: %w", name, source.Name, err)
	}
	saved, err := store.ReadAccessoryState(envName, name)
	if err != nil {
		return err
	}
	saved.Host = accessory.HostFact(replacement)
	saved.LastRestore = &state.AccessoryRestore{
		Artifact:  artifact,
		Host:      replacement.Name,
		Output:    result.Output,
		CreatedAt: deployNow().UTC(),
	}
	saved.UpdatedAt = deployNow().UTC()
	if err := store.SaveAccessoryState(saved); err != nil {
		return err
	}
	recordEvent(store, state.Event{Environment: envName, Kind: "migrate_accessory", Status: "succeeded", Accessory: name, Host: replacement.Name, Message: artifact})
	fmt.Fprintf(w, "moved accessory %s to the replacement server\n", name)
	return nil
}

func repointHostFact(store state.Store, envName string, source scheduler.Host, providerName string, created provider.Host) (state.HostFact, error) {
	facts, err := store.ReadHostFacts(envName)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// Inventory-provider environments that were never provisioned have no
		// hosts.json yet — treat that as an empty fact list and create the
		// replacement's fact below.
		return state.HostFact{}, fmt.Errorf("read host facts for %s: %w", envName, err)
	}
	var oldFact state.HostFact
	updated := false
	// A logical host can appear in several pools (say app and ingress) as
	// separate facts for the same machine — repoint every one of them, or the
	// unmatched pools keep resolving to the replaced server.
	for i := range facts {
		if facts[i].Name == source.Name {
			if !updated {
				oldFact = facts[i]
			}
			applyProviderHostToFact(&facts[i], created)
			updated = true
		}
	}
	if !updated {
		fact := state.HostFact{Name: source.Name, Pool: source.Pool, User: source.User, Provider: providerName}
		applyProviderHostToFact(&fact, created)
		facts = append(facts, fact)
	}
	if err := store.SaveHostFacts(envName, facts); err != nil {
		return state.HostFact{}, err
	}
	return oldFact, nil
}

func migrateServiceRollout(ctx context.Context, opts *options, cfg *config.Config, env config.Environment, store state.Store, envName, stateDir string, hosts []scheduler.Host, replacement scheduler.Host, current state.Release) error {
	secretOpts, err := secretSourceOptions(opts, envName)
	if err != nil {
		return err
	}
	secretFile, err := secrets.RenderScopedForEnv(cfg, secretOpts)
	if err != nil {
		return err
	}
	secretEnvFiles, secretWrites, err := serviceSecretEnvFiles(cfg, hosts, envName, secretFile)
	if err != nil {
		return err
	}
	if err := writeRemoteSecretFiles(ctx, secretWrites); err != nil {
		return err
	}
	images := make([]string, 0, len(current.Images))
	for _, image := range current.Images {
		images = append(images, image)
	}
	sort.Strings(images)
	if err := syncRemoteRegistryAuth(ctx, newDeployDocker(), []scheduler.Host{replacement}, images); err != nil {
		return fmt.Errorf("write registry auth on replacement: %w", err)
	}
	actions, err := deployment.Rollout(ctx, deployment.RolloutOptions{
		Config:         cfg,
		Environment:    env,
		Hosts:          hosts,
		EnvName:        envName,
		ReleaseID:      current.ID,
		Images:         current.Images,
		StateDir:       stateDir,
		SecretEnvFiles: secretEnvFiles,
		AgentFor:       deploymentAgentFactory(),
	})
	if err != nil {
		return err
	}
	recordIngressEvents(store, envName, current.ID, actions)
	return syncRemoteReleaseStateWithEvents(ctx, store, envName, hosts, "migrate_release_state_write", current)
}

// stopOldWorkloads stops Ship-managed containers left on the old server. The
// server is usually deleted right afterwards, so failures are reported as
// warnings instead of aborting the migration.
func stopOldWorkloads(ctx context.Context, w io.Writer, cfg *config.Config, envName string, oldServer scheduler.Host) {
	client := newDeployAgent(oldServer)
	observed, err := deployment.InspectObservedOnHosts(ctx, []scheduler.Host{oldServer}, deploymentAgentFactory())
	if err != nil {
		fmt.Fprintf(w, "warning: could not list containers on old server %s: %v\n", oldServer.ContactTarget(), err)
		return
	}
	names := make([]string, 0, len(observed)+1)
	for _, item := range observed {
		if name := strings.TrimSpace(item.Container.Names); name != "" {
			names = append(names, name)
		}
	}
	names = append(names, deployment.CaddyContainerName(cfg.Project, envName))
	for _, name := range names {
		if err := client.Call(ctx, "stop_container", map[string]string{"name": name}, nil); err != nil {
			fmt.Fprintf(w, "warning: stop container %s on old server: %v\n", name, err)
		}
	}
}

func deleteOldServer(ctx context.Context, w io.Writer, prov provider.Provider, project, envName string, oldFact state.HostFact, hostName, createdID string) error {
	providerHosts, err := prov.List(ctx, project, envName)
	if err != nil {
		return fmt.Errorf("list servers to delete old host %s: %w", hostName, err)
	}
	oldName := oldFact.ProviderName
	if oldName == "" {
		oldName = hostName
	}
	for _, host := range providerHosts {
		if host.ID == createdID {
			continue
		}
		matchesID := oldFact.ProviderID != "" && host.ID == oldFact.ProviderID
		matchesName := oldFact.ProviderID == "" && host.Name == oldName
		if matchesID || matchesName {
			if err := prov.Delete(ctx, host); err != nil {
				return fmt.Errorf("delete old server %s: %w", host.Name, err)
			}
			fmt.Fprintf(w, "deleted old server %s\n", host.Name)
			return nil
		}
	}
	fmt.Fprintf(w, "warning: old server for host %s was not found in the provider inventory; nothing deleted\n", hostName)
	return nil
}

func oldServerLabel(fact state.HostFact, hostName string) string {
	if fact.ProviderName != "" {
		return fact.ProviderName
	}
	return hostName
}
