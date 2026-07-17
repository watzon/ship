# Plan 011: Decide how `ship scale` becomes an applied, durable operation

> **Executor instructions**: This is a design spike, not an implementation plan. Follow it step by step, gather evidence from the live repository, and write the specified design artifact. Do not change production code. If anything in the "STOP conditions" section occurs, stop and report; do not improvise. When done, update this plan's row in `plans/README.md` unless a reviewer told you they maintain the index.
>
> **Drift check (run first)**: `git diff --stat 93da974..HEAD -- internal/cli internal/config docs README.md`
> Plan 009 may move `scaleCmd`; follow the symbol. Stop if applied scaling or config mutation support already landed.

## Status

- **Priority**: Direction
- **Effort**: M
- **Risk**: MED
- **Depends on**: `plans/009-split-cli-command-domains.md`
- **Category**: direction
- **Planned at**: commit `93da974`, 2026-07-16

## Why this matters

The README presents scale as a first-class operator workflow, but `ship scale ENV SERVICE=N` only computes and prints a placement plan. Operators must edit `ship.yml` and run deploy to change capacity. The design decision is whether scaling should mutate the GitOps source of truth, persist a runtime override, or remain a preview command with a clearer name. A weak choice creates config drift or destroys YAML comments/anchors.

## Current state

Documentation promises scale alongside deploy (`README.md:9`):

```markdown
- A single `ship` binary for provision, deploy, scale, migrate, logs, and recovery.
```

The quickstart documents a manual edit/deploy workflow (`docs/quickstart.md:117-123`):

```bash
# update ship.yml, then preview the durable config change
ship --dry-run deploy production
ship deploy production
```

The current scale command (`internal/cli/root.go:3807-3849` at planning commit) parses overrides and prints a `planner.DeploymentPlan`:

```go
func scaleCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scale ENV SERVICE=N [SERVICE=N...]",
		Short: "Preview deterministic manual scaling placement",
		Args:  ui.MinimumArgs(2, ui.Env, ui.ScaleAssignments),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			// ... parse overrides into cfg.Services[name].Scale ...
			plan, err := planner.DeploymentPlan(cfg, envName)
			fmt.Fprint(cmd.OutOrStdout(), plan.String())
			// record planned event only
			return nil
		},
	}
	return cmd
}
```

`internal/config` has custom `yaml.Node` unmarshalling but no round-trip writer. `yaml.Marshal` is used for render output, not source-preserving edits. This makes safe config mutation the primary technical unknown.

## Commands you will need

| Purpose | Command | Expected on success |
|---|---|---|
| Locate scale/config paths | `git grep -n 'func scaleCmd\|yaml.Node\|yaml.Marshal' -- internal docs README.md` | current callsites listed |
| Run current behavior | `go run ./cmd/ship scale production web=2 --config testdata/sample-app/ship.yml` | preview output or a documented fixture limitation |
| Config tests | `go test ./internal/config -count=1` | exit 0 |
| CLI scale tests | `go test ./internal/cli -run 'Test.*Scale' -count=1` | exit 0 |
| Design diff check | `git diff --check` | exit 0 |

## Scope

**In scope**:

- Read-only investigation of `internal/cli`, `internal/config`, `internal/state`, `docs`, `README.md`, and sample configs.
- Create `docs/design/applied-scaling.md`.
- `plans/README.md` for status only.
- Throwaway experiments under a temporary directory; do not commit them.

**Out of scope**:

- Production Go changes, new flags, state schema, or command behavior.
- Choosing autoscaling metrics, schedules, or provider-level instance scaling.
- Scaling accessories.
- Deploy fan-out/performance work.
- Editing users' config during the spike.

## Git workflow

- Branch: `advisor/011-applied-scaling-design`
- Commit message: `docs(design): decide applied scaling model`
- Commit only the design artifact and plan index status.
- Do not push or open a PR unless instructed.

## Steps

### Step 1: Specify the user contract before evaluating storage

In the design artifact, state the proposed command contract to evaluate:

- existing `ship scale ENV SERVICE=N...` remains preview-only for backward compatibility;
- an explicit mutation flag or subcommand performs the durable operation;
- applied scaling must update a durable source of truth before remote mutation;
- it must support multiple service assignments atomically;
- validation must happen before writing;
- deploy/rollout semantics and operation lock must be reused, not duplicated;
- failure after config write must leave actionable recovery and a subsequent deploy must converge;
- `--dry-run` must not write config or remote state.

Also list non-goals: autoscaling, provider VM count, accessory replicas, cross-repo Git commits, and implicit deploy from a mere preview.

**Verify**: contract distinguishes preview, durable write, and remote apply phases with failure semantics.

### Step 2: Evaluate three source-of-truth options

Build a decision matrix for:

1. **Patch `ship.yml` then reuse deploy** — durable and reviewable, but source-preserving YAML mutation is difficult and config may be outside the working directory.
2. **Persist runtime scale overrides in `.ship/` state** — fast and formatting-safe, but creates two sources of truth, surprises GitOps, and complicates drift/status/recovery.
3. **Keep preview-only and rename/clarify** — lowest risk, but leaves the advertised operator workflow incomplete.

Score each for durability, GitOps clarity, failure recovery, comment/anchor preservation, environment overlay semantics, CI/noninteractive use, `--config` paths, secrets risk, implementation complexity, and backward compatibility.

The recommended default is option 1 only if source-preserving edits and overlay targeting prove safe; otherwise recommend option 3 and reject silent runtime overrides.

**Verify**: every score cites a repository behavior or an experiment.

### Step 3: Prototype source-preserving YAML edits outside the repo

Copy representative config strings into a temporary directory and test `gopkg.in/yaml.v4` node-level editing for:

- comments before/after service definitions;
- anchors and aliases;
- quoted scalars;
- service-level base `scale` plus environment override `scale`;
- environment inheritance/merge behavior used by `config.ResolveEnvironment`;
- multiple assignments in one write;
- unknown provider keys and custom extensions;
- CRLF and final newline behavior;
- symlinked config path and file mode preservation.

Compare byte diffs. The acceptable mutation changes only target scalar nodes plus unavoidable formatter whitespace documented in the design. If comments, aliases, unknown fields, or unrelated formatting are lost, mark config mutation not ready and identify the smallest library/helper research needed.

Do not add a new dependency during the spike.

**Verify**: design artifact includes compact before/after examples and a pass/fail table for every case.

### Step 4: Resolve overlay targeting semantics

Trace `config.Load` and `ResolveEnvironment` to answer:

- where an assignment should be written when a service scale is inherited;
- whether `ship scale staging web=3` writes an environment service override or changes the global service;
- how `--env-file` affects resolved values but not YAML structure;
- what happens when service definitions use anchors/aliases;
- how multiple config files or non-default `--config` paths behave;
- whether a missing environment override block may be created without reformatting unrelated content.

Recommended rule: write the narrowest explicit environment override for the named environment, never rewrite the base service unless the user explicitly targets a global scope. Validate this against the actual schema.

**Verify**: design includes at least six input examples with the exact YAML node targeted or a reason to refuse.

### Step 5: Design atomic write and deploy handoff

Specify an implementation sequence, without coding it:

1. acquire the existing environment operation lock;
2. load raw YAML node tree and typed config;
3. validate all service names/counts and compute preview;
4. recheck file identity/hash immediately before write;
5. create an atomic backup in `.ship/` or adjacent safe location without copying secrets into logs;
6. atomically replace config while preserving mode;
7. reload typed config and verify resolved scales equal requested values;
8. invoke the same deploy workflow under the existing lock without reacquiring it;
9. on deploy failure, leave the config as desired-state truth and print the exact retry command; do not silently roll back desired state after partial remote mutation.

Address concurrent editor changes, symlinks, read-only files, Git dirty state, and event status. Do not have the CLI commit Git changes.

**Verify**: every failure boundary says which config and remote state remains and how the operator converges.

### Step 6: Produce an implementation-plan recommendation

End `docs/design/applied-scaling.md` with one verdict:

- `IMPLEMENT_CONFIG_PATCH_AND_DEPLOY`;
- `KEEP_PREVIEW_ONLY_PENDING_YAML_WRITER`;
- `REJECT_APPLIED_SCALE`.

If implementing, list exact future files/symbols, test matrix, CLI syntax, migration/backward-compatibility behavior, and a two-phase implementation order. If pending, state the experiment that must pass. If rejecting, update the documentation recommendation.

**Verify**: a fresh executor could write a separate implementation plan without reopening undecided product semantics.

### Step 7: Validate the artifact

Check links/code references and run existing focused tests to ensure the spike changed no source behavior.

**Verify**: `go test ./internal/config ./internal/cli -run 'Test.*Scale|TestLoad|TestResolveEnvironment' -count=1` → exit 0; `git diff --check` → exit 0; only `docs/design/applied-scaling.md` and plan status changed.

## Test plan

The spike uses experiments, not permanent source tests. The artifact must record:

- YAML preservation cases and diffs;
- environment overlay targeting examples;
- atomic write/failure table;
- current CLI output and tests;
- a decision matrix and one explicit verdict.

Any later implementation must add source-preserving mutation tests, dry-run tests, lock/concurrent-edit tests, deploy handoff tests, and config-write/deploy-failure recovery tests.

## Done criteria

- [ ] `docs/design/applied-scaling.md` exists and is self-contained.
- [ ] Three source-of-truth options are compared with evidence.
- [ ] Source-preserving YAML behavior is experimentally tested across every listed case.
- [ ] Environment overlay targeting is explicit and grounded in current schema resolution.
- [ ] Atomic write/deploy failure semantics are specified.
- [ ] Silent runtime overrides are accepted or rejected explicitly; no ambiguous hybrid remains.
- [ ] Artifact ends with one machine-readable verdict and exact next-step scope.
- [ ] No production source or test file changed.
- [ ] Focused existing tests pass.
- [ ] `git diff --check` exits 0.
- [ ] `plans/README.md` status row is updated.

## STOP conditions

Stop and report if:

- Current config schema cannot identify a unique source node for an environment/service scale.
- Node editing loses comments, anchors, aliases, unknown fields, or secrets-bearing structure and no safe refusal rule exists.
- Applied scaling requires a second persistent source of truth without maintainer approval.
- The existing deploy workflow cannot be invoked under an already-held operation lock without large refactoring.
- A config write could race another editor without detectable file identity/hash checks.

## Maintenance notes

- This artifact is a decision record, not a promise that applied scaling ships.
- If config schema or YAML library changes, rerun the preservation matrix before implementation.
- Runtime overrides should remain rejected unless the project explicitly adopts a two-source-of-truth model with drift UX.