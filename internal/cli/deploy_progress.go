package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/deployment"
	"github.com/watzon/ship/internal/planner"
)

// deployClock supplies timestamps for step durations. Tests replace it.
var deployClock = time.Now

// deployProgress reports each deploy step as it finishes. Steps are looked up in
// the plan that was printed at the start of the deploy, so the lines the runner
// emits use the same kind, target, and order as the plan.
//
// Every duration is passed in by the caller, measured around the operation
// itself, so a line never includes time spent on unrelated work.
type deployProgress struct {
	w        io.Writer
	plan     planner.Plan
	executed []planner.Action
	// problems lists completions that were not in the plan or that repeated a
	// step. They are still printed; Err reports them so tests can fail on them.
	problems []string
}

func newDeployProgress(w io.Writer, plan planner.Plan) *deployProgress {
	return &deployProgress{w: w, plan: plan}
}

// Err reports completions that were unplanned or repeated.
func (p *deployProgress) Err() error {
	if p == nil || len(p.problems) == 0 {
		return nil
	}
	return fmt.Errorf("deploy progress: %s", strings.Join(p.problems, "; "))
}

func formatElapsed(d time.Duration) string {
	return d.Round(100 * time.Millisecond).String()
}

// done records the planned step kind/target as finished and prints one line.
// It is safe to call on a nil receiver.
func (p *deployProgress) done(kind, target string, elapsed time.Duration) {
	p.doneNote(kind, target, elapsed, "")
}

// doneNote is done with an extra note after the duration.
func (p *deployProgress) doneNote(kind, target string, elapsed time.Duration, note string) {
	if p == nil {
		return
	}
	action, planned := p.lookup(kind, target)
	if !planned {
		p.problems = append(p.problems, fmt.Sprintf("step %s %s completed but is not in the plan", kind, target))
	}
	for _, seen := range p.executed {
		if seen.Kind == kind && seen.Target == target {
			p.problems = append(p.problems, fmt.Sprintf("step %s %s completed twice", kind, target))
			break
		}
	}
	p.executed = append(p.executed, action)
	label := target
	if kind == "start" && action.Details != "" {
		label += " " + action.Details
	}
	suffix := formatElapsed(elapsed)
	if note != "" {
		suffix += ", " + note
	}
	fmt.Fprintf(p.w, "%s %s: ok (%s)\n", kind, label, suffix)
}

func (p *deployProgress) lookup(kind, target string) (planner.Action, bool) {
	for _, planned := range p.plan.Actions {
		if planned.Kind == kind && planned.Target == target {
			return planned, true
		}
	}
	return planner.Action{Kind: kind, Target: target}, false
}

// doneKind marks every planned step of a kind as finished, in plan order.
func (p *deployProgress) doneKind(kind string, elapsed time.Duration) {
	if p == nil {
		return
	}
	for _, planned := range p.plan.Actions {
		if planned.Kind == kind {
			p.done(planned.Kind, planned.Target, elapsed)
		}
	}
}

// detail prints an indented sub-step line (a health check or a Caddy reload)
// that belongs to a plan step but is not a plan step itself.
func (p *deployProgress) detail(format string, args ...any) {
	if p == nil {
		return
	}
	fmt.Fprintf(p.w, "  "+format+"\n", args...)
}

// failed prints the failure of a step, after the lines of every step that
// already completed.
func (p *deployProgress) failed(kind, target string, elapsed time.Duration, err error) {
	if p == nil {
		return
	}
	fmt.Fprintf(p.w, "%s %s: FAILED (%s): %v\n", kind, target, formatElapsed(elapsed), err)
}

// failedWithOutput is failed plus what the failed command printed.
func (p *deployProgress) failedWithOutput(kind, target string, elapsed time.Duration, err error, result agent.CommandResult) {
	if p == nil {
		return
	}
	p.failed(kind, target, elapsed, err)
	if result.ExitCode != 0 {
		fmt.Fprintf(p.w, "  exit code: %d\n", result.ExitCode)
	}
	if out := strings.TrimSpace(result.Output); out != "" {
		fmt.Fprintf(p.w, "  stdout:\n%s\n", indentLines(out, "    "))
	}
	if errOut := strings.TrimSpace(result.Stderr); errOut != "" {
		fmt.Fprintf(p.w, "  stderr:\n%s\n", indentLines(errOut, "    "))
	}
}

func indentLines(text, prefix string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

// releaseReporter wires release command results into the progress output.
func (p *deployProgress) releaseReporter() *releaseReporter {
	return &releaseReporter{
		onDone: func(service string, elapsed time.Duration) { p.done("release", service, elapsed) },
		onFail: func(service string, elapsed time.Duration, err error, result agent.CommandResult) {
			p.failedWithOutput("release", service, elapsed, err, result)
		},
	}
}

// rolloutEvent prints a finished or failed rollout operation as it happens:
// each start as a plan step, each health check and Caddy reload as a detail.
func (p *deployProgress) rolloutEvent(event deployment.StepEvent) {
	if p == nil {
		return
	}
	target := fmt.Sprintf("%s.%d", event.Service, event.Replica)
	switch event.Kind {
	case deployment.ActionStart:
		if event.Err != nil {
			p.failed("start", target, event.Elapsed, event.Err)
			return
		}
		p.done("start", target, event.Elapsed)
	case deployment.ActionHealth:
		if event.Err != nil {
			p.failed("health", target, event.Elapsed, event.Err)
			return
		}
		p.detail("health %s: ok (%d %s, %s)", target, event.Checks, plural(event.Checks, "check", "checks"), formatElapsed(event.Elapsed))
	case deployment.ActionIngress:
		if event.Err != nil {
			p.failed("ingress", "caddy reload", event.Elapsed, event.Err)
			return
		}
		p.detail("caddy reload %s: ok (%s)", event.Host, formatElapsed(event.Elapsed))
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
