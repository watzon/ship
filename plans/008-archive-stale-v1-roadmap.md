# Plan 008: Archive the stale V1 checklist and leave a truthful project status

> **Executor instructions**: Follow this plan step by step. Run every verification command and confirm the expected result before moving to the next step. If anything in the "STOP conditions" section occurs, stop and report; do not improvise. When done, update this plan's row in `plans/README.md` unless a reviewer told you they maintain the index.
>
> **Drift check (run first)**: `git diff --stat 93da974..HEAD -- PLAN.md README.md docs docs_release_test.go`
> Stop if `PLAN.md` has already been reconciled, if another file links to specific checklist anchors, or if active roadmap ownership moved elsewhere.

## Status

- **Priority**: P2
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: docs
- **Planned at**: commit `93da974`, 2026-07-16

## Why this matters

The root `PLAN.md` is a 656-line implementation checklist whose Phase 6–8 status claims contradict shipped deploy, rollback, ingress, and accessory behavior. Humans and coding agents can treat unchecked historical items as current work and reimplement completed features. Preserve the document as history, clearly label its stale checkbox state, and replace the root file with a concise current-status pointer to maintained documentation.

## Current state

- `PLAN.md` is not linked from the root README documentation table.
- `README.md` and `docs/` are the maintained user-facing sources.
- `docs_release_test.go` checks version references in a fixed file list; moving this plan should not change release pins, but the full test must confirm that.
- Do not “fix” hundreds of checkboxes. That creates an unverifiable pseudo-roadmap and invites the same drift.

Stale rollout claims (`PLAN.md:258-299`):

```markdown
## Phase 6: Service Deployment And Rolling Updates

Status: deploy currently builds/pushes, computes placements, calls agent pull/run, writes release metadata, and supports dry-run.

### Tasks

- [ ] Convert plans into executable typed actions.
- [ ] Create release metadata before mutation begins.
- [x] Start new replicas according to deterministic placement.
- [ ] Run health gates before routing traffic.
- [ ] Add healthy replicas to ingress.
```

Stale ingress claims (`PLAN.md:310-337`):

```markdown
## Phase 7: Caddy Ingress

Status: Caddyfile generation exists; remote rollout is not complete.

### Tasks

- [x] Generate Caddy config from healthy service replicas and service ingress domains.
- [ ] Manage Caddy as a container on ingress hosts.
- [x] Write generated Caddy config to Ship state.
```

Stale accessory claims (`PLAN.md:346-369`):

```markdown
## Phase 8: Accessories

Status: accessory config, planning, and restore guardrails exist.

### Tasks

- [x] Support single-primary accessories.
- [x] Support volumes.
- [x] Support backup command declarations.
```

Current implementations are visible in:

- typed actions and execution: `internal/deployment/deployment.go:23-31,172-319,443-496`;
- Caddy write/reload and rollback: `internal/agent/rpc.go:841-902`, `internal/deployment/deployment.go:523-758`;
- accessory commands: `internal/cli/root.go:6219-7120`;
- current operator docs: `docs/deploy-and-operate.md`.

## Commands you will need

| Purpose | Command | Expected on success |
|---|---|---|
| Find references | `git grep -n 'PLAN.md\|Phase 6: Service Deployment' -- ':!plans/**'` | only reviewed intentional references |
| Docs tests | `go test ./... -run TestPinnedReleaseDocsUseCurrentOrNextReleaseTag -count=1` | exit 0 or documented tag-context skip |
| Full tests | `go test ./...` | exit 0 |
| Markdown whitespace | `git diff --check` | exit 0 |

## Scope

**In scope**:

- `PLAN.md`
- `docs/history/v1-implementation-plan.md` (create)
- `docs/README.md` only if it already has a history/index section suitable for one link
- `plans/README.md` for status only

**Out of scope**:

- Writing a new product roadmap or promising dates/features.
- Updating user-facing feature documentation unrelated to stale roadmap claims.
- Editing source code or tests.
- Marking historical checkboxes complete without evidence.
- Direction plans 011–013; they remain optional proposals, not current commitments.

## Git workflow

- Branch: `advisor/008-archive-stale-v1-roadmap`
- Commit message: `docs: archive stale V1 implementation plan`
- Preserve Git history with `git mv` when moving the historical content.
- Do not push or open a PR unless instructed.

## Steps

### Step 1: Inventory links and historical intent

Search tracked files for `PLAN.md`, phase heading anchors, and links into the root file. Record every match before moving it. Read the opening status and V1 completion definition to ensure the document is an implementation snapshot rather than an active public roadmap.

If any external-facing documentation treats it as active, stop and ask whether the maintainer wants reconciliation instead of archival.

**Verify**: `git grep -n 'PLAN.md\|Phase 6: Service Deployment' -- ':!plans/**'` → every match is understood and listed in the commit/PR notes.

### Step 2: Move the full historical checklist

Create `docs/history/` if absent and move the complete current `PLAN.md` to `docs/history/v1-implementation-plan.md` without deleting sections or rewriting checkbox history.

Insert a short banner immediately below the title stating:

- this is the original V1 implementation snapshot;
- checkbox/status values are preserved historical state and are not current capability claims;
- current behavior is documented in the root README and `docs/` guides;
- the archival date and source commit are `2026-07-16` and `93da974`.

Do not update individual historical checkboxes.

**Verify**: compare the moved file against `git show 93da974:PLAN.md` after excluding only the inserted banner → all historical content remains.

### Step 3: Replace the root file with current status and canonical links

Create a concise root `PLAN.md` that contains:

1. Title: current project status.
2. A factual statement that the V1 implementation checklist is complete enough to have shipped releases and is archived.
3. A link to `docs/history/v1-implementation-plan.md`.
4. Canonical links to `README.md`, `docs/quickstart.md`, `docs/configuration/README.md`, `docs/deploy-and-operate.md`, `docs/recovery.md`, and `docs/development.md`.
5. A statement that this file is not an active roadmap and that uncommitted future ideas must not be inferred from historical unchecked boxes.

Do not list Plans 011–013 as commitments.

**Verify**: every relative link in root `PLAN.md` resolves to a tracked file.

### Step 4: Update inbound links only when necessary

For every reference found in Step 1:

- historical references should point to the archived file;
- references seeking current behavior should point to the relevant maintained guide;
- do not add a new README link unless users need the history.

If `docs/README.md` already has a history or contributor index, add one “Historical V1 implementation plan” link. Otherwise leave it unchanged.

**Verify**: `git grep -n 'PLAN.md' -- ':!plans/**'` → no stale link treats the archive as active roadmap.

### Step 5: Run docs and full checks

Run whitespace and existing documentation-aware tests, then the normal suite.

**Verify**: `git diff --check` → exit 0; `go test ./...` → exit 0.

## Test plan

This is documentation-only. Verification must establish:

- full historical content is preserved;
- archive banner prevents current-state interpretation;
- root status file contains only valid links and no stale checklist;
- no inbound link breaks;
- existing docs release test and full Go suite pass.

Do not create source-text tests for `PLAN.md`; repository search and link checks are sufficient for a documentation move.

## Done criteria

- [ ] Original 656-line checklist is preserved in `docs/history/v1-implementation-plan.md` except for an added archival banner.
- [ ] Root `PLAN.md` is concise, current, and explicitly not an active roadmap.
- [ ] Stale rollout, ingress, and accessory claims no longer appear as current root status.
- [ ] Every changed relative Markdown link resolves.
- [ ] No future feature is presented as committed work.
- [ ] `git diff --check` exits 0.
- [ ] `go test ./...` exits 0.
- [ ] No files outside the in-scope list changed.
- [ ] `plans/README.md` status row is updated.

## STOP conditions

Stop and report if:

- Any tracked or external-facing doc links to specific root `PLAN.md` anchors.
- The maintainer has started using `PLAN.md` as an active post-V1 roadmap since commit `93da974`.
- Git history comparison shows content was lost during the move.
- Current documentation lacks a canonical destination for a claim referenced from the plan.
- A docs test requires embedding a release version in the archive.

## Maintenance notes

- Historical plans should remain immutable except for broken links or an explicit erratum.
- Active product direction belongs in reviewed issues/design docs, not revived historical checkboxes.
- Reviewers should verify the move preserves history and that the root replacement makes no unsupported maturity claims.