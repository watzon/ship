# Plan 013: Design an opt-in, application-consistent service-volume migration contract

> **Executor instructions**: This is a design spike, not an implementation plan. Follow it step by step, gather evidence from the live repository, and write the specified design artifact. Do not change production code. If anything in the "STOP conditions" section occurs, stop and report; do not improvise. When done, update this plan's row in `plans/README.md` unless a reviewer told you they maintain the index.
>
> **Drift check (run first)**: `git diff --stat 93da974..HEAD -- internal/config internal/cli/migrate.go internal/agent internal/deployment docs`
> Plans 001, 007, and 009 may affect RPC inventory, migration tests, and CLI file paths. Stop if a service-volume migration contract already landed or Docker volume syntax changed.

## Status

- **Priority**: Direction
- **Effort**: L
- **Risk**: HIGH
- **Depends on**: `plans/001-agent-rpc-registry.md`, `plans/007-migrate-failure-contracts.md`, `plans/009-split-cli-command-domains.md`
- **Category**: direction
- **Planned at**: commit `93da974`, 2026-07-16

## Why this matters

`ship migrate` moves service containers to a replacement host but explicitly leaves service volume data behind. A generic byte copy is not enough: volume specs include Docker named volumes and arbitrary bind mounts; live writers can produce inconsistent snapshots; ownership and restore ordering matter; and multiple replicas may share or independently populate volume names on a host. The project needs an opt-in contract that makes application consistency and unsupported cases explicit before any implementation.

## Current state

Service volume config is an untyped Docker-compatible string list (`internal/config/config.go:1919-1921`):

```go
type Service struct {
	// ...
	Volumes []string `yaml:"volumes"`
	// ...
}
```

Validation only requires nonempty `source:target` syntax (`internal/config/config.go:3032-3047`):

```go
func validateVolumeSpecs(label string, specs []string) []string {
	for i, spec := range specs {
		// ...
		if !strings.Contains(spec, ":") {
			errs = append(errs, fmt.Sprintf("%s must use source:target syntax", specLabel))
		}
	}
	return errs
}
```

Deployment passes the strings directly to Docker (`internal/deployment/deployment.go:983-987`):

```go
for _, volume := range svc.Volumes {
	if strings.TrimSpace(volume) != "" {
		args = append(args, "-v", volume)
	}
}
```

Migration only warns (`internal/cli/migrate.go:317-322`):

```go
for _, name := range plan.services {
	if len(cfg.Services[name].Volumes) > 0 {
		fmt.Fprintf(w, "warning: service %s uses volumes on %s; volume data is not migrated\n", name, plan.source.Name)
	}
}
```

Accessories already have explicit `backup.command`, `restore_command`, optional export, artifact validation, and restore checks. That is the closest safety pattern, but service topology differs and should not be forced into accessory semantics without analysis.

## Commands you will need

| Purpose | Command | Expected on success |
|---|---|---|
| Map volume paths | `git grep -n 'Volumes\|ensure_volume\|volume data is not migrated\|backup_command\|restore_command' -- internal docs` | relevant config/runtime paths listed |
| Config tests | `go test ./internal/config -run 'Test.*Volume|Test.*Service' -count=1` | exit 0 |
| Migration tests | `go test ./internal/cli -run 'Test.*Migrate' -count=1` | exit 0 |
| Agent tests | `go test ./internal/agent -run 'Test.*Volume|Test.*File' -count=1` | exit 0 |
| Design whitespace | `git diff --check` | exit 0 |

## Scope

**In scope**:

- Read-only investigation of service volume parsing, Docker commands, agent file/RPC transport, migration order, accessory backup patterns, and docs.
- Create `docs/design/service-volume-migration.md`.
- `plans/README.md` for status only.
- Disposable local Docker experiments only when Docker is available; absence must not block the non-Docker design work.

**Out of scope**:

- Production Go changes, schema additions, RPC additions, or data copying.
- Database-specific replication, distributed storage, or zero-downtime guarantees.
- Migrating arbitrary host paths by default.
- Provider volume snapshot implementation.
- Accessory migration changes.
- Encrypting or uploading backups to a new external service.

## Git workflow

- Branch: `advisor/013-service-volume-migration-design`
- Commit message: `docs(design): specify service volume migration`
- Commit only the design artifact and plan index status.
- Do not push or open a PR unless instructed.

## Steps

### Step 1: Build a volume-spec taxonomy from current Docker semantics

Document and test parsing cases for:

- named volume: `uploads:/app/uploads`;
- absolute bind mount: `/srv/config:/app/config:ro`;
- relative source if current validation allows it;
- options such as `ro`, `z`, `Z`, propagation, and comma-separated flags;
- anonymous or target-only syntax, which current validation rejects;
- repeated source mounted to multiple targets;
- the same named source used by multiple services/replicas;
- environment overrides replacing the base volume list.

For each case state whether it is portable between hosts, safely discoverable, writable, and eligible for an automatic contract. Do not write a home-grown colon splitter that misparses Windows paths as a future implementation recommendation; Ship targets Linux hosts but parsing should use Docker-compatible semantics.

**Verify**: artifact includes a table classifying every current valid spec as named, bind, ambiguous, or unsupported.

### Step 2: Compare four migration strategies

Evaluate:

1. **Application-defined backup/restore commands** — best consistency and format control; requires user configuration and artifact transport.
2. **Generic tar export/import of named Docker volumes** — portable for files but not application-consistent under writes; ownership/xattrs/sparse files need validation.
3. **Host-level rsync/tar of bind mounts** — handles arbitrary paths but crosses a major root-filesystem security boundary and has weak portability.
4. **Provider volume snapshots/reattachment** — potentially efficient but provider-specific, zone-dependent, and not applicable to ordinary local Docker volumes.

Score application consistency, downtime, portability, security, ownership/metadata fidelity, artifact size/streaming, retry safety, provider neutrality, and implementation effort.

Recommended default: implement only an explicit application-defined policy first; consider generic named-volume tar later as an opt-in `crash_consistent` mode after metadata experiments. Reject automatic bind-mount copying.

**Verify**: each score cites current code or a controlled experiment; no strategy is presented as universally safe.

### Step 3: Specify the proposed config model

Design a typed, opt-in service migration policy. The artifact must give exact YAML and Go-shape candidates. It should cover:

- `mode`: `application` (initial supported mode) and potentially future `crash_consistent`;
- named `volumes` or mount sources included, never implicit “all paths”;
- backup command and restore command execution context;
- optional pre-backup quiesce and post-backup resume commands;
- restore verification command;
- artifact path/URI contract;
- timeout and ownership behavior;
- whether commands run inside the service container or a purpose-built helper image;
- secret environment handling without logging values.

Compare service-level commands with per-volume declarations. Choose one based on cases such as a database spanning multiple volumes, where one transactionally consistent backup should cover all data.

Do not overload existing deploy hooks if their lifecycle/working directory cannot express migration artifacts safely.

**Verify**: schema can represent one database with multiple volumes and one service with a mix of migratable named volumes and unsupported bind mounts.

### Step 4: Define consistency and downtime semantics

Specify the required operator contract:

- migration must refuse configured service volumes without a declared policy unless an explicit `--ignore-volume` escape hatch is designed and named;
- maintenance mode or a write-quiesce command must run before the backup consistency point when the application requires it;
- new writes after backup are not transferred unless the design includes a second synchronization pass;
- old service remains available or stopped at each phase explicitly;
- restore verification must complete before host-fact repoint/service rollout commit;
- old volume data is never deleted automatically;
- `--keep-server` retains a recoverable source;
- retry behavior is idempotent for artifact naming and target restore.

Evaluate whether the existing migrate order must change: current accessory movement occurs before host-fact repoint and service rollout. Place service backup, transfer, restore, and verification relative to these steps, with a complete state transition diagram.

**Verify**: every phase states whether writes are possible and what data-loss window exists.

### Step 5: Design artifact transport and resource limits

Trace existing SSH streaming (`transport.SSH.CopyTo`/`CopyFromLocal`), agent file RPCs, and accessory artifact handling. Decide:

- local-controller relay versus host-to-host stream;
- whether artifacts are files, directories, stdout streams, or exported URIs;
- maximum-size behavior and disk-space preflight;
- checksum/integrity validation;
- cancellation and partial-file cleanup;
- permissions, owner, xattrs, sparse files, symlinks, and device file rejection;
- path validation and traversal prevention;
- redaction of command output and secret material.

Recommended portable baseline: host-to-host streaming or remote artifact copy without buffering the entire archive in controller memory, plus explicit checksum and destination free-space checks. Validate whether current transport supports this without exposing arbitrary new network APIs.

**Verify**: artifact includes a threat/resource table and rejects unbounded in-memory buffering.

### Step 6: Run optional local Docker metadata experiments

If Docker is available, create a temporary named volume containing:

- nested files;
- executable bits;
- numeric ownership;
- symlink;
- sparse file;
- xattr if supported.

Export and restore using the candidate helper-image/tar approach, then compare metadata and cleanup all temporary resources. Also test a container writing during export to demonstrate why generic tar is not application-consistent.

If Docker is unavailable, mark these results `UNVERIFIED` and make them a blocking gate for any future `crash_consistent` implementation. Do not weaken the recommended application-defined mode.

**Verify**: compact experiment results are recorded with exact commands and environmental limitations; no temporary Docker resources remain.

### Step 7: Specify partial-failure and recovery contracts

Using Plan 007's migration boundaries, add service-volume-specific failures:

- quiesce fails;
- backup command fails;
- artifact transfer is interrupted;
- checksum mismatch;
- target volume creation/ownership fails;
- restore fails;
- restore verification fails;
- host-fact repoint fails after successful restore;
- service rollout fails after restore;
- old-server delete fails.

For each, state old/new server existence, write availability, artifact retention, host fact, current release, target volume state, and exact safe retry/cleanup action. The old server must not be deleted until restore verification and service rollout succeed.

**Verify**: no failure requires guessing which copy is authoritative.

### Step 8: Produce an implementation-plan recommendation

End `docs/design/service-volume-migration.md` with one verdict:

- `IMPLEMENT_APPLICATION_DEFINED_VOLUME_MIGRATION`;
- `SPIKE_GENERIC_NAMED_VOLUME_TRANSFER_NEXT`;
- `KEEP_WARNING_ONLY`.

For an implementation verdict, list exact future files/symbols, config migration/validation, RPC methods, host-migration stage changes, tests, and docs. Separate required phase 1 from deferred generic tar/provider snapshot work.

**Verify**: a fresh executor can write a bounded implementation plan without inventing consistency or security semantics.

### Step 9: Validate the artifact

Run focused existing tests and validate links/references.

**Verify**: `go test ./internal/config ./internal/agent ./internal/cli -run 'Test.*Volume|Test.*Migrate' -count=1` → exit 0; `git diff --check` → exit 0; only the design artifact and plan status changed.

## Test plan

This spike records evidence rather than adding source tests. Required artifact sections:

- current volume-spec taxonomy;
- strategy decision matrix;
- exact opt-in config proposal;
- migration state diagram and data-loss window;
- artifact transport threat/resource table;
- optional Docker metadata experiment results;
- complete partial-failure matrix;
- one machine-readable verdict.

A future implementation must add config validation, protocol registry, agent command/path validation, transfer integrity, cancellation/cleanup, dry-run, maintenance/quiesce/resume, restore verification, retry, and Plan 007 residual-state tests.

## Done criteria

- [ ] `docs/design/service-volume-migration.md` exists and is self-contained.
- [ ] Named volumes, bind mounts, options, and ambiguous specs are classified.
- [ ] Four strategies are compared; automatic bind-mount copying is explicitly accepted or rejected.
- [ ] Proposed policy is opt-in and application consistency is defined.
- [ ] Data-loss/downtime window and migration stage order are explicit.
- [ ] Artifact transport avoids unbounded controller memory and addresses integrity/path security.
- [ ] Every partial failure has residual state and safe retry/cleanup action.
- [ ] Artifact ends with one machine-readable verdict and bounded future scope.
- [ ] No production source or test file changed.
- [ ] Focused existing tests pass.
- [ ] `git diff --check` exits 0.
- [ ] `plans/README.md` status row is updated.

## STOP conditions

Stop and report if:

- The proposed contract implies zero downtime or zero data loss without an application/replication mechanism that proves it.
- Arbitrary bind mounts would be copied with root privileges by default.
- Artifact transfer buffers unbounded volume data in memory.
- Volume identity is ambiguous across services/replicas and the schema cannot disambiguate it.
- Restore can commit host facts before integrity and application verification succeed.
- Generic tar is recommended without successful metadata/security experiments.

## Maintenance notes

- Service-volume mobility is a data protocol, not a Docker convenience feature; review it with the same rigor as database recovery.
- Keep provider snapshots and generic named-volume export as separate future modes with separate guarantees.
- Any future implementation must extend Plan 007's migration failure matrix before release.