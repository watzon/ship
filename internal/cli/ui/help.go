package ui

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	GroupSetup       = "setup"
	GroupPlan        = "plan"
	GroupInfra       = "infra"
	GroupDeploy      = "deploy"
	GroupOperate     = "operate"
	GroupAccessories = "accessories"
	GroupSecrets     = "secrets"
	GroupRecovery    = "recovery"
)

func ConfigureRoot(cmd *cobra.Command) {
	cmd.AddGroup(
		&cobra.Group{ID: GroupSetup, Title: "Project setup:"},
		&cobra.Group{ID: GroupPlan, Title: "Config and planning:"},
		&cobra.Group{ID: GroupInfra, Title: "Infrastructure:"},
		&cobra.Group{ID: GroupDeploy, Title: "Deploy:"},
		&cobra.Group{ID: GroupOperate, Title: "Day-2 operations:"},
		&cobra.Group{ID: GroupAccessories, Title: "Accessories:"},
		&cobra.Group{ID: GroupSecrets, Title: "Secrets:"},
		&cobra.Group{ID: GroupRecovery, Title: "Recovery:"},
	)
	applyHelp(cmd)
}

func applyHelp(cmd *cobra.Command) {
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetHelpFunc(helpFunc)
	cmd.SetUsageFunc(usageFunc)
	for _, child := range cmd.Commands() {
		applyHelp(child)
	}
}

func helpFunc(cmd *cobra.Command, _ []string) {
	renderHelp(cmd.OutOrStdout(), cmd, true)
}

func usageFunc(cmd *cobra.Command) error {
	renderHelp(cmd.OutOrStdout(), cmd, false)
	return nil
}

func renderHelp(w io.Writer, cmd *cobra.Command, long bool) {
	style := NewStyle(w)
	fmt.Fprintln(w, style.Bold(style.Teal(cmd.CommandPath()))+" "+style.White(cmd.Short))
	if long && cmd.Long != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, style.Gray(wrap(cmd.Long)))
	}
	if cmd.HasAvailableLocalFlags() {
		fmt.Fprintln(w)
		fmt.Fprintln(w, style.TealDark("Flags:"))
		fmt.Fprint(w, style.Gray(formatFlags(cmd.LocalFlags())))
	}
	if cmd.HasAvailableInheritedFlags() {
		fmt.Fprintln(w)
		fmt.Fprintln(w, style.TealDark("Global Flags:"))
		fmt.Fprint(w, style.Gray(formatFlags(cmd.InheritedFlags())))
	}
	if cmd.HasAvailableSubCommands() {
		fmt.Fprintln(w)
		renderSubcommands(w, style, cmd)
	}
	if cmd.HasParent() {
		fmt.Fprintln(w)
		fmt.Fprintln(w, style.Gray("Run 'ship --help' for the full command list."))
	}
}

func renderSubcommands(w io.Writer, style Style, cmd *cobra.Command) {
	if len(cmd.Groups()) > 0 {
		for _, group := range cmd.Groups() {
			commands := commandsForGroup(cmd, group.ID)
			if len(commands) == 0 {
				continue
			}
			fmt.Fprintln(w, style.TealDark(group.Title))
			for _, child := range commands {
				fmt.Fprintln(w, formatCommandLine(style, child))
			}
			fmt.Fprintln(w)
		}
		ungrouped := commandsForGroup(cmd, "")
		if len(ungrouped) > 0 {
			fmt.Fprintln(w, style.TealDark("Other commands:"))
			for _, child := range ungrouped {
				fmt.Fprintln(w, formatCommandLine(style, child))
			}
		}
		return
	}
	fmt.Fprintln(w, style.TealDark("Commands:"))
	for _, child := range cmd.Commands() {
		if child.Hidden || !child.IsAvailableCommand() {
			continue
		}
		fmt.Fprintln(w, formatCommandLine(style, child))
	}
}

func commandsForGroup(cmd *cobra.Command, groupID string) []*cobra.Command {
	var commands []*cobra.Command
	for _, child := range cmd.Commands() {
		if child.Hidden || !child.IsAvailableCommand() {
			continue
		}
		if child.GroupID != groupID {
			continue
		}
		commands = append(commands, child)
	}
	return commands
}

func formatCommandLine(style Style, cmd *cobra.Command) string {
	name := cmd.Name()
	if cmd.HasSubCommands() {
		name = cmd.CommandPath()
		if cmd.Parent() != nil {
			name = strings.TrimPrefix(name, cmd.Root().Name()+" ")
		}
	}
	use := strings.TrimSpace(cmd.Use)
	if strings.HasPrefix(use, cmd.Name()) {
		use = strings.TrimPrefix(use, cmd.Name())
		use = strings.TrimSpace(use)
	}
	line := "  " + style.Teal(padRight(name, 18)) + " "
	if use != "" {
		line += style.Gray(use) + "  "
	}
	line += style.White(cmd.Short)
	return line
}

func formatFlags(flags *pflag.FlagSet) string {
	if flags == nil {
		return ""
	}
	var b strings.Builder
	flags.VisitAll(func(flag *pflag.Flag) {
		if flag.Hidden {
			return
		}
		var line string
		if flag.Shorthand != "" {
			line = fmt.Sprintf("  -%s, --%s", flag.Shorthand, flag.Name)
		} else {
			line = fmt.Sprintf("      --%s", flag.Name)
		}
		if flag.DefValue != "" && flag.DefValue != "false" {
			line += fmt.Sprintf(" (default %q)", flag.DefValue)
		}
		line += "\n        " + flag.Usage + "\n"
		b.WriteString(line)
	})
	return b.String()
}

func wrap(text string) string {
	return strings.TrimSpace(text)
}
