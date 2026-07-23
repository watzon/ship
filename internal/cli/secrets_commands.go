package cli

import (
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
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/secrets"
	"github.com/watzon/ship/internal/state"
)

func secretsCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{Use: "secrets", Short: "Manage and verify Ship secrets"}
	var initRecipient string
	initCmd := &cobra.Command{
		Use:   "init ENV",
		Short: "Create an encrypted secret store for an environment",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			secretOpts, err := secretSourceOptions(opts, args[0])
			if err != nil {
				return err
			}
			if err := secrets.InitStore(secretOpts, initRecipient); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created %s\n", secrets.StorePath(secretOpts))
			return nil
		},
	}
	initCmd.Flags().StringVar(&initRecipient, "recipient", "", "age recipient for encrypting this environment's secrets")
	_ = initCmd.MarkFlagRequired("recipient")
	cmd.AddCommand(initCmd)

	var setValue string
	setCmd := &cobra.Command{
		Use:   "set ENV NAME",
		Short: "Set a secret in the encrypted store",
		Args:  ui.ExactArgs(ui.Env, ui.Secret),
		RunE: func(cmd *cobra.Command, args []string) error {
			secretOpts, err := secretSourceOptions(opts, args[0])
			if err != nil {
				return err
			}
			value := setValue
			if value == "" {
				var ok bool
				value, ok = os.LookupEnv(args[1])
				if !ok {
					return fmt.Errorf("missing --value and environment variable %s", args[1])
				}
			}
			if err := secrets.SetStoredSecret(secretOpts, "", args[1], value); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "set %s in %s\n", args[1], secrets.StorePath(secretOpts))
			return nil
		},
	}
	setCmd.Flags().StringVar(&setValue, "value", "", "secret value; defaults to environment variable NAME")
	cmd.AddCommand(setCmd)

	unsetCmd := &cobra.Command{
		Use:   "unset ENV NAME",
		Short: "Remove a secret from the encrypted store",
		Args:  ui.ExactArgs(ui.Env, ui.Secret),
		RunE: func(cmd *cobra.Command, args []string) error {
			secretOpts, err := secretSourceOptions(opts, args[0])
			if err != nil {
				return err
			}
			if err := secrets.UnsetStoredSecret(secretOpts, "", args[1]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "unset %s in %s\n", args[1], secrets.StorePath(secretOpts))
			return nil
		},
	}
	cmd.AddCommand(unsetCmd)

	cmd.AddCommand(&cobra.Command{
		Use:   "list ENV",
		Short: "List encrypted store secret names",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			secretOpts, err := secretSourceOptions(opts, args[0])
			if err != nil {
				return err
			}
			values, err := secrets.ReadStore(secretOpts)
			if err != nil {
				return err
			}
			names := make([]string, 0, len(values))
			for name := range values {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				fmt.Fprintln(cmd.OutOrStdout(), name)
			}
			return nil
		},
	})

	var exportRedacted bool
	exportCmd := &cobra.Command{
		Use:   "export ENV",
		Short: "Export encrypted store secrets",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			secretOpts, err := secretSourceOptions(opts, args[0])
			if err != nil {
				return err
			}
			values, err := secrets.ReadStore(secretOpts)
			if err != nil {
				return err
			}
			names := make([]string, 0, len(values))
			for name := range values {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				value := values[name]
				if exportRedacted {
					value = "<redacted:" + secrets.Digest(value) + ">"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s=%s\n", name, value)
			}
			return nil
		},
	}
	exportCmd.Flags().BoolVar(&exportRedacted, "redacted", false, "redact values and show digests")
	cmd.AddCommand(exportCmd)

	var verifyWithProcessEnv bool
	verifyCmd := &cobra.Command{
		Use:   "verify [ENV]",
		Short: "Check required secrets exist",
		Args:  cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			var checks []secrets.Check
			if len(args) > 0 {
				resolved, _, err := cfg.ResolveEnvironment(args[0])
				if err != nil {
					return err
				}
				secretOpts, err := secretSourceOptions(opts, args[0])
				if err != nil {
					return err
				}
				secretOpts.SkipProcessEnv = !verifyWithProcessEnv
				checks, err = secrets.VerifyForEnv(resolved, secretOpts)
			} else {
				checks, err = secrets.Verify(cfg)
			}
			for _, check := range checks {
				if check.Present {
					fmt.Fprintf(cmd.OutOrStdout(), "ok   %s digest=%s\n", check.Name, check.Digest)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "fail %s missing\n", check.Name)
				}
			}
			return err
		},
	}
	verifyCmd.Flags().BoolVar(&verifyWithProcessEnv, "with-process-env", false, "include ambient process environment overrides when ENV is provided")
	cmd.AddCommand(verifyCmd)
	var diffWithProcessEnv bool
	diffCmd := &cobra.Command{
		Use:   "diff ENV",
		Short: "Compare encrypted-store secret digests with the current release",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			resolved, _, err := cfg.ResolveEnvironment(args[0])
			if err != nil {
				return err
			}
			secretOpts, err := secretSourceOptions(opts, args[0])
			if err != nil {
				return err
			}
			secretOpts.SkipProcessEnv = !diffWithProcessEnv
			rendered, err := secrets.RenderScopedForEnv(resolved, secretOpts)
			if err != nil {
				return err
			}
			stateDir, err := localStateDirForConfig(opts.configPath)
			if err != nil {
				return err
			}
			release, err := state.NewStore(stateDir).CurrentRelease(args[0])
			if err != nil {
				return fmt.Errorf("current release for %s: %w", args[0], err)
			}
			diff := secrets.Diff(rendered.Digests, release.SecretDigests)
			if diff.Empty() {
				fmt.Fprintf(cmd.OutOrStdout(), "secrets match current release %s\n", release.ID)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "secret drift against release %s\n", release.ID)
			for _, name := range diff.Missing {
				fmt.Fprintf(cmd.OutOrStdout(), "missing %s\n", name)
			}
			for _, name := range diff.Changed {
				fmt.Fprintf(cmd.OutOrStdout(), "changed %s%s\n", name, secretDiffSourceSuffix(name, rendered.ProcessEnvStoreOverrides))
			}
			for _, name := range diff.Extra {
				fmt.Fprintf(cmd.OutOrStdout(), "extra %s%s\n", name, secretDiffSourceSuffix(name, rendered.ProcessEnvStoreOverrides))
			}
			return fmt.Errorf("secret drift detected")
		},
	}
	diffCmd.Flags().BoolVar(&diffWithProcessEnv, "with-process-env", false, "include ambient process environment overrides")
	cmd.AddCommand(diffCmd)
	var renderDryRun bool
	var renderWithProcessEnv bool
	render := &cobra.Command{
		Use:   "render ENV",
		Short: "Render redacted remote secret env files",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !renderDryRun && !opts.dryRun {
				return fmt.Errorf("secrets render only supports --dry-run in V1")
			}
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			resolved, env, err := cfg.ResolveEnvironment(args[0])
			if err != nil {
				return err
			}
			secretOpts, err := secretSourceOptions(opts, args[0])
			if err != nil {
				return err
			}
			secretOpts.SkipProcessEnv = !renderWithProcessEnv
			rendered, err := secrets.RenderScopedForEnv(resolved, secretOpts)
			if err != nil {
				return err
			}
			if len(rendered.Scopes) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no required secrets")
				return nil
			}
			for _, scope := range secretRenderScopes(resolved, env, args[0]) {
				file, ok := rendered.Scopes[strings.TrimSuffix(filepath.Base(scope), ".env")]
				if !ok {
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "# %s\n%s\n", scope, file.Redacted)
			}
			return nil
		},
	}
	render.Flags().BoolVar(&renderDryRun, "dry-run", false, "print redacted env-file output without exposing values")
	render.Flags().BoolVar(&renderWithProcessEnv, "with-process-env", false, "include ambient process environment overrides")
	cmd.AddCommand(render)
	return cmd
}

func printProcessEnvStoreOverrideWarning(w io.Writer, names []string) {
	if len(names) == 0 {
		return
	}
	ui.PrintWarn(w, "warning: process environment overrides encrypted store secrets: "+strings.Join(names, ", ")+"; unset them to deploy the stored values")
}

func secretDiffSourceSuffix(scopedName string, processEnvStoreOverrides []string) string {
	name := scopedName
	if index := strings.LastIndexByte(name, ':'); index >= 0 {
		name = name[index+1:]
	}
	for _, overridden := range processEnvStoreOverrides {
		if name == overridden {
			return " (local source: process env, not store)"
		}
	}
	return ""
}

func secretRenderScopes(cfg *config.Config, env config.Environment, envName string) []string {
	scopesByPath := map[string]struct{}{}
	if placements, err := scheduler.PlaceServices(cfg, env); err == nil {
		for _, placement := range placements {
			scopesByPath[secrets.RemoteEnvFilePath(envName, "service-"+placement.Service)] = struct{}{}
		}
	}
	for _, name := range accessory.SortedNames(cfg, "") {
		scopesByPath[secrets.RemoteEnvFilePath(envName, "accessory-"+name)] = struct{}{}
	}
	scopes := make([]string, 0, len(scopesByPath))
	for path := range scopesByPath {
		scopes = append(scopes, path)
	}
	sort.Strings(scopes)
	return scopes
}
