# Plan 003: Let environment overrides express explicit false, and stop partial overrides wiping sibling fields

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **PRECONDITION**: this plan rewrites the per-environment deep-merge code in
> `internal/config/config.go`, which was UNCOMMITTED work-in-progress at
> planning time. Run `git status` first. If `internal/config/config.go` or
> `internal/config/config_test.go` appear as modified-but-uncommitted, STOP
> and report — the maintainer must land their in-flight work before this plan
> runs, otherwise you will be editing (and possibly mangling) code they are
> still writing.
>
> **Drift check (after the precondition passes)**: compare the "Current
> state" excerpts below against the live code. Line numbers will have
> shifted once the in-flight work is committed; the *code shapes* must match.
> On a shape mismatch, STOP.

## Status

- **Priority**: P1
- **Effort**: M
- **Risk**: MED
- **Depends on**: the in-flight config merge work being committed (see precondition); plan 001 recommended first (adds `-race`/vet gates)
- **Category**: bug
- **Planned at**: commit `29fc466` (dirty working tree), 2026-07-01

## Why this matters

Ship just gained per-environment deep-merging of services and accessories:
an environment can override individual fields of a base service instead of
redefining it. But the merge functions use Go zero-values to detect "field was
set", which makes two classes of override silently impossible:

1. **You cannot turn a flag off per environment.** `if override.Primary`,
   `if override.Pull`, `if override.Required` etc. mean an explicit
   `primary: false` / `pull: false` / `required: false` in an environment is
   indistinguishable from "not set" and is silently dropped — the environment
   deploys with the base's `true`. For flags like `backup.required` (guards
   destructive accessory operations) that's a correctness hazard, not a
   cosmetic one.
2. **Partially overriding `release:` or `backup.schedule:` wipes the sibling
   fields.** Setting only `release.command` in an environment replaces the
   whole `ReleaseCommand` struct, zeroing the base's `replica` and
   `timeout_seconds`.

The codebase already contains the correct pattern — `IngressHealth.Enabled`
is a `*bool` and its merge checks `!= nil` — it just wasn't applied to the
other boolean flags. This plan applies that existing pattern and converts the
two wholesale struct replacements to field-by-field merges.

## Current state

All in `internal/config/config.go` (line numbers from the planning-time
working tree; they will shift):

- The exemplar (correct) pattern — `IngressHealth.Enabled *bool`
  (config.go:2022 area) merged at config.go:3675-3679:

  ```go
  func mergeIngressHealth(base, override IngressHealth) IngressHealth {
  	out := base
  	if override.Enabled != nil {
  		out.Enabled = override.Enabled
  	}
  ```

  and read via nil-check, e.g. config.go:2707:
  `svc.Ingress.Health.Enabled != nil && *svc.Ingress.Health.Enabled && …`

- The broken zero-value guards to fix:
  - `mergeService` (config.go:3468): `if override.Scale > 0 { out.Scale = override.Scale }` (3477)
    — **Scale stays out of scope, see below** — and
    `if override.Release.Command != "" || override.Release.Replica > 0 || override.Release.TimeoutSeconds > 0 { out.Release = override.Release }` (3514-3516).
  - `mergeAccessory` (config.go:3523): `if override.Primary { out.Primary = override.Primary }` (3534-3536).
  - `mergeImageSpec` (config.go:3570): `if override.Pull { … }` / `if override.NoCache { … }` (~3600-3603).
  - Buildpack merge: `if override.Publish` (~3644) and `if override.TrustBuilder` (~3650); struct `BuildpackConfig` at config.go:1951 (`Publish bool`, `TrustBuilder bool`).
  - `mergeBackupSpec` (config.go:3765): `if override.Required` / `if override.RestoreCheck` (3779-3784) and
    `if strings.TrimSpace(override.Schedule.Cron) != "" || override.Schedule.TimeoutSeconds > 0 { out.Schedule = override.Schedule }` (3788-3790).

- Field declarations to convert (all currently `bool` with only `yaml` tags):
  - `ImageSpec.Pull`, `ImageSpec.NoCache` (config.go:1929 area)
  - `BuildpackConfig.Publish`, `BuildpackConfig.TrustBuilder` (config.go:1951)
  - `Accessory.Primary` (config.go ~2170, `yaml:"primary"`)
  - `BackupSpec.Required`, `BackupSpec.RestoreCheck`

- Known read sites (the compiler will find the rest once types change):
  - `ImageSpec.Pull`: config.go:2598, 2651; `internal/cli/root.go:2827` (maps to docker build options)
  - `ImageSpec.NoCache`: config.go:2601, 2654; `internal/cli/root.go:2828`
  - `Accessory.Primary`: 4 non-test sites (grep `.Primary` across `internal/`)
  - `internal/docker/docker.go:298,301` use the docker package's own options
    struct (plain bools) — that struct does NOT change; only the mapping site
    in root.go adapts.

- Existing tests for the merge behavior (added with the in-flight work),
  in `internal/config/config_test.go`:
  `TestResolveEnvironmentDeepMergesPartialServiceOverrides` and
  `TestResolveEnvironmentMergesServiceAndAccessoryEnvOverrides` — model new
  tests on these.

- **Hash side-effect you must know about**: `internal/cli/root.go:1578-1585`:

  ```go
  func configHash(cfg *config.Config) string {
  	data, err := json.Marshal(cfg)
  	...
  ```

  The config hash is a JSON marshal of the struct. Converting `bool` fields to
  `*bool` changes their JSON encoding (`false` → `null` for unset), so every
  config's hash changes once, and `ship status` will report config drift
  against releases deployed before this change. This is a one-time,
  self-healing warning (next deploy records the new hash). It is accepted —
  but it must be called out in your final report and the commit message.

## Commands you will need

| Purpose  | Command                        | Expected on success |
|----------|--------------------------------|---------------------|
| Build    | `go build ./cmd/ship`          | exit 0              |
| Tests    | `go test ./...`                | all pass            |
| One pkg  | `go test ./internal/config`    | all pass            |
| Vet      | `go vet ./...`                 | exit 0              |

## Scope

**In scope** (the only files you should modify):
- `internal/config/config.go`
- `internal/config/config_test.go`
- Read-site fixes forced by the type change, expected in:
  `internal/cli/root.go`, and possibly `internal/deployment/`,
  `internal/accessory/`, `internal/planner/` (compiler-guided; mechanical
  accessor swaps only).

**Out of scope** (do NOT touch, even though they look related):
- `Service.Scale` — an explicit `scale: 0` override is currently
  indistinguishable from unset, but zero-scale means "remove the service"
  elsewhere in the product, so whether `scale: 0` should be a legal
  *override* is a product decision. Leave `Scale int` and its `> 0` guard
  as-is; it is recorded in the maintenance notes for the maintainer.
- `copyServices` / `copyAccessories` shallow-copy aliasing (base and resolved
  envs share nested maps/slices/`*Ingress`) — latent, no current mutation
  site; deferred, see maintenance notes.
- `internal/docker/docker.go` options struct — keeps plain bools.
- Any YAML schema/docs files (`docs/`, `skills/`) — a docs sync happens
  separately.

## Git workflow

- Branch: `advisor/003-env-override-merge-semantics`
- Commit style: `fix(config): allow env overrides to set boolean flags to false`
  and `fix(config): merge release and backup.schedule field-by-field`.
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Field-by-field merge for `ReleaseCommand` and `BackupSchedule`

No type changes yet. Replace the wholesale replacements:

In `mergeService`, replace
`if override.Release.Command != "" || … { out.Release = override.Release }` with a
helper following the existing merge-helper naming (`mergeReleaseCommand`):

```go
func mergeReleaseCommand(base, override ReleaseCommand) ReleaseCommand {
	out := base
	if strings.TrimSpace(override.Command) != "" {
		out.Command = override.Command
	}
	if override.Replica > 0 {
		out.Replica = override.Replica
	}
	if override.TimeoutSeconds > 0 {
		out.TimeoutSeconds = override.TimeoutSeconds
	}
	return out
}
```

and in `mergeBackupSpec` do the same for `Schedule` (`mergeBackupSchedule`:
fields `Cron string`, `TimeoutSeconds int` — struct at config.go:2199 area).

Add two tests in `config_test.go` (model on
`TestResolveEnvironmentDeepMergesPartialServiceOverrides`):
- base sets `release: {command, replica: 2, timeout_seconds: 30}`; env
  overrides only `release.command` → resolved keeps `replica == 2` and
  `timeout_seconds == 30`.
- base sets `backup.schedule: {cron, timeout_seconds}`; env overrides only
  `schedule.cron` → `timeout_seconds` preserved.

**Verify**: `go test ./internal/config` → all pass including the 2 new tests.

### Step 2: Convert the six boolean flags to `*bool` with accessor methods

For each of `ImageSpec.Pull`, `ImageSpec.NoCache`, `BuildpackConfig.Publish`,
`BuildpackConfig.TrustBuilder`, `Accessory.Primary`, `BackupSpec.Required`,
`BackupSpec.RestoreCheck`:

1. Change the field type to `*bool` (yaml tag unchanged — yaml.v3 decodes
   `true`/`false` into `*bool` natively; absent key stays nil).
2. Add an accessor on the owning struct next to existing methods (see
   `BuildpackConfig.Enabled()` at config.go:1961 for placement style):

   ```go
   func (i ImageSpec) PullEnabled() bool { return i.Pull != nil && *i.Pull }
   ```

   Names: `PullEnabled`, `NoCacheEnabled`, `PublishEnabled`,
   `TrustBuilderEnabled`, `IsPrimary`, `BackupRequired`, `RestoreCheckEnabled`
   (method on the struct that owns the field).
3. Update the merge guards to the nil-check pattern:
   `if override.Pull != nil { out.Pull = override.Pull }` etc.
4. Build; fix every read site the compiler reports by swapping direct field
   reads for the accessor. Do not change any logic beyond the swap.
5. If any code *assigns* `true`/`false` literals to these fields (e.g. test
   fixtures, `ship init` scaffolding), introduce a tiny package-level helper
   in the test file or use `ptrBool := func(b bool) *bool { return &b }` —
   check first whether config.go already has such a helper near the
   `IngressHealth.Enabled` usage and reuse it.

**Verify**: `go build ./cmd/ship` → exit 0; `go test ./...` → all pass.

### Step 3: Add explicit-false override tests

In `config_test.go`, add `TestResolveEnvironmentOverridesBooleanFlagsToFalse`:
base config sets `image.pull: true`, accessory `primary: true`, and
`backup.required: true`; the environment override sets each to `false`;
assert the resolved environment reports false via the accessors, and that a
*different* environment without the override still resolves true (proves the
base wasn't mutated).

**Verify**: `go test ./internal/config -run TestResolveEnvironment` → all pass.

### Step 4: Full-suite regression pass

**Verify**: `go test ./...` → all pass. `go vet ./...` → exit 0.

## Test plan

- Step 1: 2 sibling-preservation tests (release, backup.schedule).
- Step 3: 1 explicit-false override test covering three representative flags,
  plus the cross-environment isolation assertion.
- Pattern: `TestResolveEnvironmentDeepMergesPartialServiceOverrides` in
  `internal/config/config_test.go` (YAML-literal config → `ResolveEnvironment`
  → assert resolved fields).
- Verification: `go test ./internal/config` and `go test ./...` all pass.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `go test ./...` exits 0; the 3 new tests exist and pass
- [ ] `grep -n "if override.Primary\b\|if override.Pull\b\|if override.NoCache\b\|if override.Required\b\|if override.RestoreCheck\b" internal/config/config.go` → no matches
- [ ] `grep -n "out.Release = override.Release\|out.Schedule = override.Schedule" internal/config/config.go` → no matches
- [ ] `go vet ./...` exits 0
- [ ] Final report notes the one-time config-hash drift side effect
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- The precondition fails: `internal/config/config.go` has uncommitted
  modifications when you start.
- The merge functions on disk don't match the "Current state" shapes (the
  in-flight work may have evolved past this plan — report, don't adapt).
- Converting a field to `*bool` breaks YAML decoding in an existing test in a
  way that isn't a missing accessor swap (would indicate custom UnmarshalYAML
  logic this plan didn't account for).
- The read-site fix-ups force changes in more than ~6 files — that means the
  blast radius was underestimated; report the file list instead of pushing on.
- Anything requires touching `internal/docker/docker.go`'s option structs.

## Maintenance notes

- **Deferred decision — `scale: 0` overrides**: an environment cannot
  currently override a service's scale to zero (`> 0` guard). If zero-scale
  per environment should be supported, `Scale` needs the same treatment via
  `*int`. Product call for the maintainer.
- **Deferred — deep-copy in `copyServices`/`copyAccessories`** (config.go:3328,
  3457): resolved environments share nested `Labels`/`Ports`/`*Ingress` with
  the base config. Nothing mutates them in place today, but any future
  in-place edit of a resolved service would corrupt other environments. If
  that class of bug ever appears, deep-copying in those two functions is the
  fix.
- **Reviewer focus**: the one-time config-hash drift (see Current state);
  and that every accessor swap is behavior-identical (`x.Pull` →
  `x.PullEnabled()` where nil ⇒ false matches the old zero-value default).
- Docs: `docs/configuration/README.md` doesn't yet document env-level service
  deep-merge at all; when it does, it must document explicit-false semantics.
