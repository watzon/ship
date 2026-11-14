package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/watzon/ship/internal/planner"
)

// deployProgress reports each deploy step as it finishes. Steps are looked up in
// the plan that was printed at the start of the deploy, so the lines the runner
// emits use the same kind, target, and order as the plan.
type deployProgress struct {
	w        io.Writer
	plan     planner.Plan
	executed []planner.Action
	last     time.Time
}

func newDeployProgress(w io.Writer, plan planner.Plan) *deployProgress {
	return &deployProgress{w: w, plan: plan, last: time.Now()}
}

// done records the planned step kind/target as finished and prints one line.
// It is safe to call on a nil receiver.
func (p *deployProgress) done(kind, target string) {
	if p == nil {
		return
	}
	action := planner.Action{Kind: kind, Target: target}
	for _, planned := range p.plan.Actions {
		if planned.Kind == kind && planned.Target == target {
			action = planned
			break
		}
	}
	now := time.Now()
	elapsed := now.Sub(p.last).Round(100 * time.Millisecond)
	p.last = now
	p.executed = append(p.executed, action)
	if action.Details != "" && (kind == "start") {
		fmt.Fprintf(p.w, "%s %s %s: ok (%s)\n", kind, target, action.Details, elapsed)
		return
	}
	fmt.Fprintf(p.w, "%s %s: ok (%s)\n", kind, target, elapsed)
}

// doneKind marks every planned step of a kind as finished, in plan order.
func (p *deployProgress) doneKind(kind string) {
	if p == nil {
		return
	}
	for _, planned := range p.plan.Actions {
		if planned.Kind == kind {
			p.done(planned.Kind, planned.Target)
		}
	}
}
