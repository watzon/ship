package cli

import (
	"os"

	"github.com/spf13/cobra"
	"github.com/watzon/ship/internal/cli/ui"
	"github.com/watzon/ship/internal/config"
)

type options struct {
	configPath          string
	dryRun              bool
	envFiles            []string
	secretsIdentityFile string
	agentBinaryPath     string
	agentReleaseDir     string
}

func Execute() error {
	opts := &options{}
	root := &cobra.Command{
		Use:           "ship",
		Short:         "Deploy Docker apps to ordinary servers with horizontal scaling",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&opts.configPath, "config", "c", config.DefaultConfigFile, "path to ship.yml")
	root.PersistentFlags().BoolVar(&opts.dryRun, "dry-run", false, "print the intended operation without mutating remote state")
	root.PersistentFlags().StringArrayVar(&opts.envFiles, "env-file", nil, "load secrets from a dotenv file (repeatable)")
	root.PersistentFlags().StringVar(&opts.secretsIdentityFile, "secrets-identity-file", "", "age identity file for encrypted Ship secrets")
	ui.ConfigureRoot(root)

	init := initCmd(opts)
	init.GroupID = ui.GroupSetup
	doctor := doctorCmd(opts)
	doctor.GroupID = ui.GroupSetup
	cfg := configCmd(opts)
	cfg.GroupID = ui.GroupPlan
	hosts := hostsCmd(opts)
	hosts.GroupID = ui.GroupPlan
	plan := planCmd(opts)
	plan.GroupID = ui.GroupPlan
	scale := scaleCmd(opts)
	scale.GroupID = ui.GroupPlan
	provision := provisionCmd(opts)
	provision.GroupID = ui.GroupInfra
	migrate := migrateCmd(opts)
	migrate.GroupID = ui.GroupInfra
	agent := agentCmd(opts)
	agent.GroupID = ui.GroupInfra
	version := versionCmd(opts)
	version.GroupID = ui.GroupInfra
	release := releaseCmd()
	release.GroupID = ui.GroupInfra
	deploy := deployCmd(opts)
	deploy.GroupID = ui.GroupDeploy
	promote := promoteCmd(opts)
	promote.GroupID = ui.GroupDeploy
	status := statusCmd(opts)
	status.GroupID = ui.GroupOperate
	ps := psCmd(opts)
	ps.GroupID = ui.GroupOperate
	health := healthCmd(opts)
	health.GroupID = ui.GroupOperate
	maintenance := maintenanceCmd(opts)
	maintenance.GroupID = ui.GroupOperate
	logs := logsCmd(opts)
	logs.GroupID = ui.GroupOperate
	restart := restartCmd(opts)
	restart.GroupID = ui.GroupOperate
	execSvc := execServiceCmd(opts)
	execSvc.GroupID = ui.GroupOperate
	inspect := inspectCmd(opts)
	inspect.GroupID = ui.GroupOperate
	support := supportCmd(opts)
	support.GroupID = ui.GroupOperate
	events := eventsCmd(opts)
	events.GroupID = ui.GroupOperate
	releases := releasesCmd(opts)
	releases.GroupID = ui.GroupOperate
	lock := lockCmd(opts)
	lock.GroupID = ui.GroupOperate
	unlock := unlockCmd(opts)
	unlock.GroupID = ui.GroupOperate
	prune := pruneCmd(opts)
	prune.GroupID = ui.GroupOperate
	recover := recoverCmd(opts)
	recover.GroupID = ui.GroupRecovery
	rollback := rollbackCmd(opts)
	rollback.GroupID = ui.GroupRecovery
	accessory := accessoryCmd(opts)
	accessory.GroupID = ui.GroupAccessories
	secrets := secretsCmd(opts)
	secrets.GroupID = ui.GroupSecrets

	root.AddCommand(
		init, doctor,
		cfg, hosts, plan, scale,
		provision, migrate, agent, version, release,
		deploy, promote,
		status, ps, health, logs, execSvc, restart, inspect, support, events, releases, lock, unlock, maintenance, prune,
		accessory,
		secrets,
		recover, rollback,
	)

	if err := root.Execute(); err != nil {
		ui.PrintError(os.Stderr, err)
		return err
	}
	return nil
}
