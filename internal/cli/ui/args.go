package ui

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

type Arg struct {
	Name string
	Hint string
}

var (
	Env = Arg{
		Name: "ENV",
		Hint: "environment name from ship.yml (e.g. production, staging)",
	}
	Service = Arg{
		Name: "SERVICE",
		Hint: "service name from ship.yml",
	}
	Accessory = Arg{
		Name: "NAME",
		Hint: "accessory name from ship.yml",
	}
	Secret = Arg{
		Name: "NAME",
		Hint: "secret name",
	}
	SourceEnv = Arg{
		Name: "SOURCE_ENV",
		Hint: "source environment to copy from",
	}
	TargetEnv = Arg{
		Name: "TARGET_ENV",
		Hint: "target environment to promote into",
	}
	ScaleAssignments = Arg{
		Name: "SERVICE=N",
		Hint: "one or more scale assignments (e.g. web=3 worker=2)",
	}
)

func ArgNamed(name, hint string) Arg {
	return Arg{Name: name, Hint: hint}
}

func ExactArgs(specs ...Arg) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) == len(specs) {
			return nil
		}
		return argCountError(cmd, specs, len(args))
	}
}

func MinimumArgs(min int, specs ...Arg) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) >= min {
			return nil
		}
		missing := specs
		if len(args) < len(specs) {
			missing = specs[len(args):]
		}
		return argCountError(cmd, missing, len(args))
	}
}

func RangeArgs(min, max int, specs ...Arg) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) >= min && len(args) <= max {
			return nil
		}
		want := specs
		if len(want) > min {
			want = want[:min]
		}
		if len(args) < min {
			return argCountError(cmd, want, len(args))
		}
		return fmt.Errorf("too many arguments: expected at most %d, got %d", max, len(args))
	}
}

func argCountError(cmd *cobra.Command, specs []Arg, got int) error {
	var missing []Arg
	if got < len(specs) {
		missing = specs[got:]
	} else if got == 0 && len(specs) > 0 {
		missing = specs
	}
	return &UsageError{
		Message:  missingArgMessage(missing, got),
		Usage:    usageLine(cmd),
		Hints:    argHints(missing),
		Received: got,
		Expected: len(specs),
	}
}

func missingArgMessage(missing []Arg, got int) string {
	if len(missing) == 0 {
		return "invalid arguments"
	}
	if len(missing) == 1 && missing[0].Name == "ENV" && got == 0 {
		return "missing environment"
	}
	names := make([]string, len(missing))
	for i, spec := range missing {
		names[i] = spec.Name
	}
	if got == 0 {
		return "missing " + strings.Join(names, ", ")
	}
	return "missing " + strings.Join(names, ", ") + fmt.Sprintf(" (got %d arg(s))", got)
}

func usageLine(cmd *cobra.Command) string {
	if cmd == nil {
		return ""
	}
	if parent := cmd.Parent(); parent != nil && !parent.HasAvailableSubCommands() {
		return parent.CommandPath() + " " + strings.TrimSpace(cmd.Use)
	}
	return cmd.CommandPath() + " " + strings.TrimSpace(strings.TrimPrefix(cmd.Use, cmd.Name()))
}

func argHints(specs []Arg) []string {
	hints := make([]string, 0, len(specs))
	for _, spec := range specs {
		if spec.Hint == "" {
			continue
		}
		hints = append(hints, spec.Name+": "+spec.Hint)
	}
	return hints
}