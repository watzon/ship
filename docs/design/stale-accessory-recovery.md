# Stale accessory recovery design

## Decision and safety invariant

Ship should add a distinct `accessory recover` command for disaster recovery. `accessory failover` remains the planned movement command and must require a source that Ship can stop and then observe as stopped before it starts the target. Manual accessory state reassignment remains unsupported.

The hard invariant is:

> Ship must not start a target primary while the saved source can still be a primary. Starting the target requires either authoritative fencing evidence for the exact saved machine or the explicit `--confirm-source-fenced` attestation together with `--yes`. Missing config, missing filtered inventory, failed SSH, and a missing container observation are never fencing evidence.

The first implementation should require the attestation for every real recovery. The current provider interface cannot supply portable authoritative proof, and existing accessory state normally does not retain a provider ID. Provider verification can be added later as a supplemental capability with a strict contract.

## Repository evidence

This design was checked against `main` at `dbb9770`, after Plans 006, 007, and 009:

- `internal/accessory/accessory.go` keeps saved placement when an eligible host has the same logical name. `matchingHost` compares only `Host.Name`. It ignores the saved pool, contact address, provider name, and provider ID.
- `accessory.HostFact` copies a `scheduler.Host` into accessory state, but `scheduler.Host` has no provider identity fields. Normal accessory saves therefore preserve logical name, pool, user, and contact, but not `Provider`, `ProviderID`, `ProviderName`, or `ServerID`, even though `state.HostFact` can represent them.
- `resolvedHostsForEnvironment` applies environment host facts to scheduler contacts, but it does not carry their provider IDs into accessory placement. A provider or migration repoint can therefore keep the same logical name while changing the machine.
- `runAccessoryRestore` and `runAccessoryFailover` both call `PlacementForHosts`, so an out-of-pool or missing logical host is terminal. A replacement with the same logical name is not terminal. It is accepted as the persisted placement.
- `runAccessoryFailover` currently starts and restores the target before it stops the source. The future implementation must reverse that safety boundary: preflight the target, stop and verify the source, then start the target.
- `startAccessoryWithRestore` starts the target container before its `test -s` artifact check. Recovery must move the remote artifact check before container start.
- The restore command's successful exit and a subsequent managed-container observation are the portable verification available today. `backup.restore_check` does not define a database-specific post-restore command.
- `state.SaveAccessoryState` is an atomic file replacement. `state.Event` can encode recovery phases without a schema change. Plan 006 made event persistence race-safe and made CLI event failures visible warnings through `recordEvent`.
- `provider.Provider.List` is scoped to project and environment in its contract and implementations commonly filter by labels or tags. `provider.Host` has no lifecycle status. An item missing from `List` cannot prove that an exact provider resource is destroyed or powered off.
- `docs/recovery.md` covers ordinary restore only. The restore artifact is a remote path under the configured artifact directory. Recovery can require the operator to stage the artifact on the target, so it does not need a backup transport redesign.

A disposable test using a temporary state directory confirmed three identity behaviors:

1. A saved contact of `203.0.113.10` was accepted as persisted on a same-name replacement at `198.51.100.20`.
2. Two eligible hosts with the saved logical name were not rejected. The first match won.
3. Moving the saved logical host out of the accessory pool produced the current stale-placement error.

The design must therefore classify identity drift before using current placement logic. Name equality alone is not proof that the saved machine and current machine are the same.

## Six-state taxonomy

"Current inventory" below means config plus saved environment host facts and any provider listing. It is discovery data, not a fencing mechanism.

| ID | State | What Ship can establish | What Ship cannot establish | Recovery treatment |
|---|---|---|---|---|
| S1 | Saved host still exists but moved out of the accessory pool | Config proves that the logical host is no longer eligible for this accessory. Saved state proves the last recorded logical name and any recorded contact or provider fields. A reachable agent can report managed containers at one instant. | Pool movement does not stop the machine or its container. It does not prove that the old primary is fenced. | Classify as stale. Refuse ordinary restore and failover. Recovery still needs authoritative fencing or attestation. |
| S2 | Saved logical host was repointed to a replacement server | A difference in known provider ID, provider name, or contact is identity-drift evidence. Environment facts may identify the replacement. Current code only sees the same logical name and follows it. | Existing accessory saves often lack provider ID, and an address or DNS name can be reused. Ship cannot infer that the old machine was stopped during the repoint. | Classify as stale identity drift, even when the logical name is still eligible. If source and target cannot be distinguished by at least a saved contact or provider ID, refuse same-logical-name recovery and require a distinct target identity. |
| S3 | Saved host is absent from config or inventory but may still run | Ship can establish only that current discovery did not return the saved logical host or provider resource. | It cannot distinguish deletion, changed labels, changed environment, API filtering, credential scope, transient API failure, or a running unlisted machine. | Source remains unknown. Inventory absence is insufficient. Require attestation unless a future exact-ID provider check supplies authoritative terminal state. |
| S4 | Saved host is known destroyed or powered off | A future provider capability can establish this only when it queries the exact saved provider ID through an unfiltered endpoint and returns a contractually terminal `deleted`, `not_found`, or `powered_off` state. An externally generated destruction or fencing record can support operator attestation. | Current `Provider.List` cannot prove it. A provider name match or filtered absence cannot prove it. Powered-off state can later be reversed outside Ship. | Exact, authoritative proof may satisfy the fence gate in a later rollout. Record the evidence and observation time. Current rollout still requires attestation. |
| S5 | Saved host is reachable but its managed accessory container is absent or stopped | The agent can prove that the expected managed container was absent or stopped at observation time. A running observation proves the source is not fenced. | Absence or stopped state does not prevent a manual start, another unlabelled copy, a service manager, or a different machine from serving the same data. | Treat absence or stopped state as supporting evidence only. Still require fencing proof or attestation. A running source is a hard refusal that attestation cannot override. |
| S6 | Saved state has corrupt or ambiguous identity | Ship can detect missing required fields, malformed state, duplicate same-name eligible hosts, conflicting provider IDs, a target indistinguishable from the source, or several machines claiming the logical host. | Ship cannot safely choose which machine is or was authoritative. | Refuse without remote mutation. Repair inventory or provide a distinct, well-identified target through a separately reviewed procedure. Do not rewrite accessory state manually. |

Classification must be conservative. Any failed or contradictory observation produces `saved_host_stale_source_unknown`, not `source_fenced_or_destroyed`.

## Recovery surface comparison

Scores use 1 for poor and 5 for strong. Higher split-brain score means lower accidental split-brain risk.

| Surface | Clarity | Split-brain safety | Auditability | Dry-run quality | Compatibility | Provider neutral | Restore helper reuse | Result |
|---|---:|---:|---:|---:|---:|---:|---:|---|
| Add disaster mode to `accessory failover` | 2 | 2 | 3 | 3 | 3 | 4 | 5 | Reject. One command would cover both a source Ship controls and a source whose state is unknown. Flags could easily select the wrong safety protocol. |
| Add `accessory recover` | 5 | 5 | 5 | 5 | 5 | 5 | 4 | Choose. The command name marks an incident path, can require a dedicated fence gate, and can preserve ordinary failover behavior for planned moves. |
| Manually edit state, then restore | 1 | 1 | 1 | 1 | 2 | 5 | 2 | Reject. Editing makes the target authoritative before restore verification, erases source identity, and provides no reliable audit or retry state. |

The operator distinction is direct:

- Use `ship accessory failover` when Ship can reach the current source, stop it, verify it is stopped, and then restore the target.
- Use `ship accessory recover` when the saved source placement or identity is stale and fencing happened outside the normal failover flow.
- Never edit `.ship/state` to make restore or deploy proceed.

## Command contract

The exact proposed real command is:

```text
ship accessory recover ENV NAME --to HOST --artifact PATH --confirm-source-fenced --yes
```

The exact preview is:

```text
ship --dry-run accessory recover ENV NAME --to HOST --artifact PATH
```

### Flags

| Argument or flag | Contract | Risk controlled |
|---|---|---|
| `ENV NAME` | Names one configured accessory in one resolved environment. | Prevents implicit multi-accessory recovery. |
| `--to HOST` | Required. Resolves one eligible target and its current physical identity. No first-host default. | Prevents silent placement and makes the target auditable. |
| `--artifact PATH` | Required. Must be an explicit remote target path accepted by `ValidateRestoreArtifact`. No fallback to `LastBackup.Artifact`. | A recorded local path may exist only on the lost source. Explicit input prevents restoring the wrong or unavailable artifact. |
| `--confirm-source-fenced` | Required for the first implementation. It means: "I attest that the saved source shown by Ship, and every machine that could still serve its data, cannot run this accessory." | Makes the split-brain decision explicit without a vague `--force`. It does not convert weak observations into proof. |
| `--yes` | Required with the attestation for a real run. Dry-run does not require it. | Requires a second, generic confirmation for destructive target restore and state commit. |
| global `--dry-run` | Performs config, local state, provider, agent topology, and target artifact reads only. It creates no lock file, event, journal, accessory state, remote file, container, network, or volume. | Gives incident responders a complete plan without changing the incident. |

A later provider-verified mode may omit `--confirm-source-fenced` only when all of these conditions hold:

1. Saved accessory state contains an exact provider name and immutable provider ID for the source.
2. The provider implements an explicit exact-ID inspection capability whose contract is not scoped by Ship labels, project, environment, or current config.
3. The capability reports `deleted`, authoritative `not_found`, or `powered_off` for that exact ID.
4. Ship rechecks the observation immediately before target start and records the evidence.
5. Any API error, unsupported status, identity mismatch, filtered result, or contradictory agent observation falls back to the attestation requirement or refusal.

`Provider.List` must never satisfy this mode. The initial implementation should print `provider fencing proof unavailable; explicit attestation required` for every provider.

### Preflight order

The real command performs all read-only checks first, then acquires the environment operation lock and repeats safety-sensitive checks while holding it. Dry-run reports whether a lock is already present but does not create one.

1. Resolve config and require one single-primary accessory that passes `ValidateRestore`: `primary=true`, `backup.required=true`, and nonempty backup and restore commands.
2. Read saved accessory state. Require a stale eligibility or identity classification. A healthy, same-identity placement is a failover case, not recovery.
3. Validate saved identity. Reject malformed, contradictory, or ambiguous state.
4. Resolve `--to` to one eligible target. Require a physical identity distinct from the saved source. Same logical name is allowed only when stable provider IDs or recorded contacts distinguish source and target.
5. Validate the artifact path locally with `ValidateRestoreArtifact`.
6. Read provider inventory as discovery evidence. Do not infer fencing from absence.
7. Inspect every reachable current environment host for the managed accessory labels and deterministic container name. Refuse any managed copy outside the saved source. Refuse a managed container on the target, whether running or stopped. Refuse an unreachable current host other than the attested saved source because topology is incomplete.
8. Inspect the saved source when it is reachable. A running managed container is a hard contradiction and refusal. An absent or stopped container remains supporting evidence only.
9. On the target, run the current `test -s` artifact command before any container start. Refuse a missing or empty artifact.
10. Print maintenance guidance before mutation: `ship maintenance enable ENV --message "Accessory recovery in progress"`. Recovery does not silently change maintenance mode.
11. Establish fencing through the future authoritative provider gate or, in the initial implementation, both `--confirm-source-fenced` and `--yes`.
12. Acquire `AcquireOperationLock(ENV, "accessory_recover")`, then repeat saved state, identity, topology, target occupancy, artifact, and fencing checks to close the preflight race.
13. Save a recovery journal before the first remote mutation.

Every remote inspection must report its observation time and identity in dry-run output. The preview prints all blockers in one pass and exits nonzero when any unresolved risk remains.

### Required dry-run output

The preview includes:

- saved source logical name, pool, last contact, provider name, provider ID, and which fields are missing;
- stale classification and current identity differences;
- target logical and physical identity;
- every host inspected, every managed container found, and every unreachable host;
- target artifact path and remote `test -s` result;
- fencing evidence rating and whether attestation plus `--yes` is still required;
- the maintenance command;
- ordered remote mutations, the state commit point, and target cleanup policy;
- the exact real command with shell-safe values;
- explicit warnings that inventory absence, SSH failure, and container absence are not fencing.

### Exact refusal guidance

The implementation should use these actionable messages, with values substituted:

| Condition | Required error text |
|---|---|
| Accessory is not single-primary or restorable | `accessory "NAME" cannot be recovered: recovery requires primary=true, backup.required=true, backup.command, and backup.restore_command` |
| Saved state missing | `accessory "NAME" has no saved placement in ENV; use accessory deploy for a new accessory, not accessory recover` |
| Placement is healthy and identity matches | `accessory "NAME" is still placed on HOST with matching identity; use accessory failover for a planned move` |
| Saved identity corrupt or ambiguous | `accessory "NAME" saved source identity is corrupt or ambiguous: DETAIL; recovery cannot choose a source, so repair inventory and rerun --dry-run` |
| Same logical target cannot be distinguished | `target HOST cannot be distinguished from saved source HOST; choose a target with a distinct provider ID or contact address` |
| Target is not eligible | `target host "HOST" is not eligible in accessory pool "POOL"; choose one configured pool member` |
| Target has a managed container | `target HOST already has managed accessory container CONTAINER with status STATUS; inspect and remove the incomplete target before retrying` |
| Another managed copy exists | `accessory "NAME" has another managed copy on HOST with status STATUS; stop and fence that copy before recovery` |
| Saved source is observed running | `saved source SOURCE is running managed container CONTAINER; recovery refuses even with --confirm-source-fenced; use accessory failover or fence it externally` |
| A non-source host cannot be inspected | `cannot prove a single-copy topology because HOST could not be inspected: ERROR; restore access or fence that host before retrying` |
| Provider list omits the source | `provider inventory did not return SOURCE, but this is not fencing proof because inventory may be filtered; pass --confirm-source-fenced only after external fencing` |
| Attestation missing | `source fencing is unproved; fence the saved source SOURCE, then rerun with --confirm-source-fenced and --yes` |
| `--yes` missing | `accessory recover requires --yes together with --confirm-source-fenced to confirm target restore and placement commit` |
| Artifact path invalid | `restore artifact "PATH" is invalid: DETAIL; stage a .backup file inside "ARTIFACT_DIR" on target HOST` |
| Artifact missing or empty on target | `restore artifact "PATH" is missing or empty on target HOST; stage it on that host and rerun --dry-run` |
| Operation lock busy | `environment "ENV" is already busy with another Ship operation; wait for it to finish, then rerun recovery preflight` |
| Existing journal has different inputs | `accessory "NAME" has unfinished recovery ATTEMPT for target OLD_TARGET and artifact OLD_PATH; use those exact inputs to resume or follow the printed cleanup instructions` |
| Existing journal is corrupt | `accessory "NAME" recovery journal is unreadable: DETAIL; do not edit accessory placement state, and inspect the target and source before proceeding` |

Attestation never overrides a running-source observation, ambiguous identity, another possible primary, target occupancy, artifact failure, or incomplete topology.

## Safety state machine

### Durable recovery journal

Before remote mutation, write a separate atomic `AccessoryRecovery` journal. It does not replace or edit `AccessoryState`. It contains:

- attempt ID, environment, accessory, artifact, and timestamps;
- the complete saved source `HostFact` snapshot and resolved target `HostFact` snapshot;
- stale classification;
- fencing mode (`operator_attestation` or future `provider_verified`), evidence summary, and evidence time;
- phase and last error;
- whether target container start and restore verification completed.

Proposed journal phases are `preflighted`, `target_started`, `target_restore_verified`, `committed`, `failed_before_target`, and `failed_after_target_start`. All accessory deploy, restore, failover, backup, migrate, and recover entry points must refuse to mutate an accessory with an unfinished journal, except an exact recovery resume. Events are audit output, not the source of retry truth, because event writes may fail.

Use event kind `accessory_recover` with statuses `blocked`, `started`, `target_started`, `target_verified`, `committed`, and `failed`. Existing `Accessory`, `Host`, and `Message` fields carry the attempt details, so `state.Event` needs no schema change. The journal, not an event, decides whether a retry may restore or commit.

### States and transitions

| From | To | Required observations or attestation | Remote mutation | Saved state and event update | Retry and incident output |
|---|---|---|---|---|---|
| `healthy_on_saved_host` | `saved_host_stale_source_unknown` | Eligibility changed, identity changed, saved host disappeared, or identity became ambiguous. | None. | Accessory placement remains saved source. A real blocked attempt may emit `accessory_recover/blocked`. Dry-run emits no event. | Print source and current identities plus the stale reason. |
| `saved_host_stale_source_unknown` | `source_fenced_or_destroyed` | Initial rollout requires the named `--confirm-source-fenced` attestation and `--yes`. A future rollout may use authoritative exact-ID proof. No contradictory running-source or extra-copy observation exists. | None. | Write journal phase `preflighted`; emit `accessory_recover/started`. Placement remains source. | Exact same inputs can retry. Changed inputs refuse. |
| `source_fenced_or_destroyed` | `target_restore_started` | Lock held, repeated topology check clean, target empty, artifact present and nonempty. | Prepare secrets, registry auth, image, network, and volumes. Start exactly one deterministic managed target container. Never contact, reconnect, delete, or rewrite the unknown source. | Atomically set journal `target_started`; emit `accessory_recover/target_started`. Placement remains source. | Print attempt ID, source, target, artifact, and cleanup policy. |
| `target_restore_started` | `target_restore_verified` | Restore command exits successfully. A fresh target observation finds exactly one correctly named and labelled managed container running on the exact target identity. | Run restore command, then read-only container inspection. | Atomically set journal `target_restore_verified`; event status may be `target_verified`. Placement remains source. | Exact resume may proceed to commit without rerunning the restore. |
| `target_restore_verified` | `state_committed_to_target` | Journal and live target identity agree, target is still the only observed managed copy, source fence evidence is still valid for this attempt. | None. | **Commit point:** atomically save `AccessoryState.Host=target`, `LastRestore`, and `UpdatedAt`; then mark journal `committed`; emit `accessory_recover/committed`. | After this save, target is authoritative. Retry must never restore again. |
| Any state before target start | `recovery_failed_before_commit` | Preflight, lock, journal, or remote preparation failed before a target container could have started. | Best-effort cleanup only for preparation artifacts known to this attempt. No source action. | Placement remains source. Journal records `failed_before_target`; event status `failed`. | Safe to retry exact inputs after correcting the error. Re-run all observations and fencing checks. |
| `target_restore_started` | `recovery_failed_after_target_start` | Start outcome is uncertain, restore failed, target verification failed, or verified-phase journal write failed. | Best-effort `stop_container_keep` on the exact target. Do not remove volumes or the source. | Placement remains source. Journal records `failed_after_target_start` when possible; event status `failed`. | Direct restore retry is refused because restore commands need not be idempotent. Print target inspection and manual cleanup instructions. |
| `recovery_failed_before_commit` | `source_fenced_or_destroyed` | Exact attempt inputs, clean repeated preflight, and fencing gate still satisfied. | None until preflight completes. | Journal returns to `preflighted`. | Retry follows the normal path. |
| `recovery_failed_after_target_start` with verified journal | `state_committed_to_target` | Exact target still exists, is running, has expected labels, is the only managed copy, and journal says restore verification completed. | None. | Retry performs the atomic placement commit only. | Print that restore was not rerun. |
| `recovery_failed_after_target_start` without verified journal | `source_fenced_or_destroyed` | Operator has inspected the incomplete target, removed its container and partial target data out of band, and a repeated preflight proves the target clean. | None by Ship during cleanup. | A new attempt ID is required. Old journal remains in incident history or is archived by an explicit future abort workflow. | Print commands to inspect the target, but do not generate destructive volume deletion commands automatically. |
| `state_committed_to_target` | `state_committed_to_target` | Saved state and journal target match. | None. | Best-effort journal cleanup and any missing `committed` event. Event failure is only a warning. | Idempotent success. Print target as authoritative. |

There is no transition directly from `saved_host_stale_source_unknown` to `target_restore_started`. There is also no transition that treats an absent inventory entry as fencing.

### Commit point

The only authority commit is the atomic `SaveAccessoryState` that changes `Host` to the target after restore verification. Before that save, saved placement remains the source. After it succeeds, every retry treats the target as authoritative even if the journal cleanup or `committed` event fails.

The state save and event append cannot be one transaction. Safety therefore uses the accessory state plus recovery journal, and audit uses events. A failed event write must print the Plan 006 warning and must not change a successful recovery into a failed command.

## Partial failures and safe retry

| Failure boundary | Could a target primary exist? | Residual remote state | Accessory state and journal | Command result and safe next action |
|---|---:|---|---|---|
| Config, saved state, or identity classification | No | None | Placement unchanged; no journal. Optional blocked event on real run. | Refuse. Fix config or inventory, then rerun dry-run. |
| Provider discovery or topology observation | No | None | Placement unchanged; no journal. | Refuse on API error, contradictory running source, extra copy, or incomplete non-source host inspection. Do not convert absence into fencing. |
| Attestation or `--yes` missing | No | None | Placement unchanged; no journal. | Refuse with the exact source identity and required flags. |
| Operation lock unavailable | No | None | Placement unchanged; no journal. | Wait for the active operation. Do not retry concurrently. |
| Artifact path validation | No | None | Placement unchanged; no journal. | Stage a valid `.backup` path inside the configured directory. |
| Target artifact `test -s` | No | None | Placement unchanged; no journal. | Stage or replace the artifact on the target, then rerun dry-run. |
| Recovery journal save | No | None | Placement unchanged; no usable attempt. | Refuse before remote mutation. Repair local state storage. |
| Secret render or secret file write | No | Secret file may have been written on target. | Placement source; journal `failed_before_target`. | Correct the error and retry exact inputs. A secret file is not a primary. |
| Registry auth, image pull, network ensure, or volume ensure | No | Image, network, auth, empty or preexisting volumes may remain. | Placement source; journal `failed_before_target`. | Retry after correction. Preflight must still require no target container. Existing volume contents must be reported; automatic deletion is forbidden. |
| `run_container` returns failure | Unknown | Container may be absent, stopped, or running if the RPC outcome was lost. | Placement source; journal becomes `failed_after_target_start` unless a fresh observation proves no container started. | Inspect target immediately. If running, stop and keep it. Refuse direct restore retry until target data is cleaned. |
| `target_started` event write | Yes, but source is already fenced | Target is running before restore. | Journal `target_started`; placement source. | Print warning and continue. Event persistence is not a safety gate. |
| Restore command | Yes | Best-effort stop target and keep its container and volumes for inspection. Restore may be partial. | Placement source; journal `failed_after_target_start`. | Refuse automatic rerun. Operator inspects and cleans target data, then starts a new attempt. Never touch source. |
| Post-restore running-container verification | Yes | Target may be healthy or unhealthy. Best-effort stop and keep it unless the failure is only a transient observation error, in which case report uncertainty. | Placement source; journal `failed_after_target_start`. | Restore is not rerun automatically. Re-observe and either resume only from a durable verified phase or clean the target. |
| Journal write of `target_restore_verified` | Yes | Target contains restored data. Best-effort stop and keep because Ship cannot persist safe resume proof. | Placement source; prior journal phase remains. | Repair local state storage, inspect target, and follow incomplete-target cleanup. Do not commit from memory. |
| Atomic accessory state save | Yes, and source is fenced | Leave verified target running for availability. Do not rerun restore. | Placement still source; journal durably says `target_restore_verified`. | Exit failure with urgent handoff. Exact retry re-observes target and performs only the state commit. Other accessory mutations are blocked by the journal. |
| Journal write of `committed` after state save | Yes, authoritative | Target stays running. | Placement target; journal may still say verified. | Treat recovery as committed. Warn, then exact retry reconciles the journal without restoring. |
| `committed` event write | Yes, authoritative | Target stays running. | Placement target; journal committed. | Print event warning and exit success. Do not misreport recovery as failed. |
| Journal cleanup | Yes, authoritative | Target stays running. | Placement target; committed journal remains. | Warn and exit success. Later exact retry performs cleanup only. |
| Final output or terminal disconnect | Depends on last durable phase | As described by journal. | State and journal decide authority, not terminal output. | Run dry-run or exact recovery again. It prints whether the target was committed and never repeats restore after commit. |

The implementation should not remove a target volume automatically. The current agent cannot establish whether `ensure_volume` created it or whether it contained preexisting data. A read-only volume inspection RPC is useful for warnings, but destructive volume cleanup needs a separate explicit workflow.

## Identity and fencing evidence

### Identity strength

Source identity uses the strongest saved fields in this order:

1. `Provider` plus immutable `ProviderID` (or legacy numeric `ServerID`).
2. `ProviderName` plus saved `PublicAddress` or `IPv4`.
3. Logical `Name`, `Pool`, and saved contact.
4. Logical name and pool only, which is legacy weak identity.

When both saved and current provider IDs exist, unequal IDs prove identity drift. When IDs are absent, a changed contact supports identity drift but does not identify provider lifecycle. Same logical name never overrides a known ID or contact mismatch. Duplicate same-name eligible hosts are ambiguous and must be rejected.

New accessory state writes must copy provider identity from environment host facts. Existing records remain readable. A legacy weak record requires operator attestation, and same-logical-name recovery is refused when source and target cannot be distinguished.

### Evidence strength table

| Signal | Strength | Reason and use |
|---|---|---|
| Exact saved provider ID queried through an unfiltered endpoint returns terminal `deleted` | Proves fenced | Sufficient only under a documented provider contract with immutable IDs. Record provider, ID, result, and time. Current interface does not offer this. |
| Exact saved provider ID queried through an unfiltered endpoint returns authoritative `not_found` | Proves fenced | Sufficient only if `not_found` cannot result from project, region, label, or credential filtering. Otherwise it is insufficient. |
| Exact saved provider ID reports `powered_off` | Proves fenced at observation time | May satisfy a future gate only when the provider contract is authoritative and Ship rechecks immediately before target start. Output must warn that external actors can power it on again. |
| Provider reports a different ID for the same logical name | Supporting only | Proves identity drift, not destruction or shutdown of the old ID. |
| Saved provider ID is absent from `Provider.List(project, environment)` | Insufficient | Current lists are commonly filtered by Ship ownership, project, environment, tags, labels, region, or inventory source. |
| Config omits the host or moved it to another pool | Insufficient | Config controls desired placement, not machine power or process state. |
| Environment host facts omit or repoint the host | Insufficient | Host facts are mutable discovery state. Repointing can erase the old provider address from current inventory. |
| SSH connection fails or times out | Insufficient | Network, DNS, firewall, credential, jump-host, or agent failure is indistinguishable from shutdown. |
| Agent reports expected managed container absent | Supporting only | It narrows observed topology at one instant. It does not prevent restart, another label set, or another machine. |
| Agent reports expected managed container stopped | Supporting only | A stopped container can be started later. It is not host fencing. |
| Agent reports expected managed container running | Proves not fenced | Hard refusal. Attestation cannot override a direct contradictory observation. |
| No managed copy is observed on every reachable current host | Supporting only | Useful split-brain scan, but it says nothing about absent or unknown former hosts. |
| Successful historical migrate or decommission event | Supporting only | Events can be stale, event writes can fail, and external state can change after the event. |
| Operator passes `--confirm-source-fenced` and `--yes` | Accepted manual gate, not machine proof | This is the explicit named attestation allowed by the safety policy. Record that the operator accepted responsibility for all machines that could serve the saved data. |
| Corrupt state, conflicting IDs, duplicate logical claims, or target indistinguishable from source | Insufficient | Refuse. Attestation is not a tool for choosing among ambiguous identities. |

### Provider contract recommendation

Do not add provider verification to the first recovery implementation. If added later, define an optional interface in `internal/provider/provider.go`, separate from `Provider.List`, such as:

```go
type HostStateInspector interface {
    InspectHostState(ctx context.Context, providerID string) (HostStateObservation, error)
}
```

`HostStateObservation` must state whether the query is exact and unfiltered, the provider ID returned, lifecycle state, observation time, and whether that state is authoritative for fencing. Unsupported providers and inventory-backed providers always require attestation. Provider deletion or power-off actions remain outside this command and outside this design.

## Future implementation scope

### Files and symbols

| File | Exact future work |
|---|---|
| `internal/state/state.go` | Add `AccessoryRecovery`, `AccessoryRecoveryPhase`, and atomic `SaveAccessoryRecovery`, `ReadAccessoryRecovery`, and `DeleteAccessoryRecovery` methods. Reuse existing `HostFact` provider fields. |
| `internal/state/state_test.go` | Cover journal round trip, atomic phase updates, corrupt journal, commit recovery, and cleanup failure. |
| `internal/scheduler/scheduler.go` | Carry optional provider identity fields on resolved hosts so accessory placement can snapshot the physical machine. |
| `internal/cli/shared.go` | Copy provider identity from environment host facts into resolved scheduler hosts. Add a helper that refuses accessory mutations when an unfinished recovery journal exists. |
| `internal/accessory/accessory.go` | Add saved-placement identity comparison and stale classification. Change name-only matching so known provider ID or contact drift is stale, and reject duplicate same-name matches. Make `HostFact` persist resolved provider identity. |
| `internal/accessory/accessory_test.go` | Cover all six taxonomy states, same-name replacement drift, legacy weak identity, duplicate logical claims, and identity-preserving placement. |
| `internal/cli/accessory_commands.go` | Register `accessory recover`; add `runAccessoryRecover`, preflight, journal resume, explicit commit, and failure cleanup. Split `startAccessoryWithRestore` so target artifact validation occurs before container start and restore verification occurs before state commit. Harden live failover to stop and observe the source before target start. |
| `internal/cli/accessory_commands_test.go` | Add the full fake matrix below, including exact RPC order, no-mutation refusals, journals, retries, event warnings, and live-failover source-stop ordering. |
| `internal/agent/rpc.go` and `internal/agent/rpc_test.go` | Optionally add read-only volume inspection so preflight can report preexisting target volumes. Do not add destructive volume cleanup to recovery. |
| `internal/provider/provider.go` and provider-local tests | Later rollout only: add the optional exact-ID inspection contract after at least one provider documents authoritative semantics. Do not change all providers as part of the initial command. |
| `docs/recovery.md` | Add the incident runbook, staging requirement for remote artifacts, dry-run example, fencing attestation, partial-failure handoff, and maintenance guidance. |
| `docs/deploy-and-operate.md` | Separate planned failover from stale-source recovery and document the recovery journal lockout. |

No manual state command, automatic provider deletion, automatic provider power-off, automatic target-volume deletion, leader election, replication promotion, or backup transport belongs in this implementation.

### Rollout order

1. Land identity propagation, conservative placement classification, and journal state with unit tests. This also closes the same-name repoint path for ordinary accessory commands.
2. Refactor artifact preflight and restore verification. Harden live failover so source stop and observation precede target start.
3. Add the distinct recovery command with mandatory attestation for every provider, operation locking, dry-run, journal resume, and event warnings.
4. Add operator documentation and run the full hermetic suite.
5. In a separate reviewed change, add provider-specific exact-ID proof for providers whose API contracts are authoritative. Keep attestation as the portable fallback.

## Future fake-based test matrix

| Area | Fake setup | Expected result |
|---|---|---|
| Source unknown | Saved source missing from config and filtered provider list, no attestation | Refuse before journal or remote mutation. Error says inventory absence is not fencing. |
| Explicit attestation | Same source, `--confirm-source-fenced` and `--yes` | Proceed only after clean topology and artifact checks. Journal records `operator_attestation`. |
| Missing second confirmation | Attestation present without `--yes` | Refuse before remote mutation. |
| Vague confirmation | `--yes` present without attestation | Refuse before remote mutation. |
| Reliable provider proof | Exact saved ID returns authoritative deleted state | Later provider rollout may proceed without attestation and records evidence. |
| Filtered provider absence | Saved ID absent from project and environment list | Refuse or require attestation. Never mark proved. |
| Provider API error | Exact inspector errors or times out | Refuse or require attestation; do not use cached absence as proof. |
| Powered-off proof | Exact inspector reports powered off, then same on immediate recheck | Later provider rollout may proceed and records both observations. |
| Provider state changes | First check says powered off, recheck says running | Hard refusal before target start. |
| Same-name replacement | Saved and current names match, provider ID or contact differs | Classify S2 stale identity drift, not healthy persisted placement. |
| Indistinguishable replacement | Same logical name, no provider IDs or contacts | Refuse same-name target as S6 ambiguous. |
| Moved pool | Saved host exists only outside accessory pool | Classify S1 and require fencing gate. |
| Corrupt saved state | Missing host name, conflicting ID fields, or invalid journal | Refuse with no remote mutation. |
| Duplicate logical identity | Two eligible current hosts share saved logical name | Refuse S6 instead of choosing first. |
| Healthy placement | Saved identity matches eligible running source | Refuse recovery and direct operator to failover. |
| Source running | Agent observes saved container running even with attestation | Hard refusal. No target mutation. |
| Source stopped or absent | Agent observes stopped or absent container with attestation | Treat as supporting evidence and proceed only with both flags. |
| Another primary | Managed accessory container observed on any other host | Refuse before target mutation. |
| Unreachable non-source host | One current host inspection fails | Refuse because topology is incomplete. |
| Target occupied running | Managed container exists on target | Refuse and print inspection guidance. |
| Target occupied stopped | Stopped managed container exists on target | Refuse. Do not overwrite or silently reuse it. |
| Ineligible target | `--to` is outside accessory pool | Refuse before agent calls. |
| Target equals source identity | Target resolves to exact saved provider ID or contact | Refuse as not a recovery move. |
| Invalid artifact path | Wrong suffix or path outside artifact directory | Refuse before agent calls. |
| Missing artifact | Target `test -s` returns false | Refuse before container start. |
| Artifact check RPC error | Target health-check RPC fails | Refuse before container start and preserve source state. |
| Dry-run | All checks pass but flags omitted | Print all evidence, risks, mutation order, and exact real command; create no event, lock, journal, file, volume, network, or container. |
| Lock contention | Fake holds environment operation lock | Refuse before journal and remote mutation. |
| Journal write failure | State store rejects initial journal save | Refuse before remote mutation. |
| Secret preparation failure | Fake secret write fails | Journal `failed_before_target`; no container; exact retry allowed. |
| Registry or pull failure | Fake fails registry auth or pull | Journal `failed_before_target`; no container; exact retry allowed. |
| Network or volume preparation failure | Fake fails ensure call | No container. Report residual preparation and allow exact retry after clean preflight. |
| Container start failure, absent | Run RPC errors and observation proves no container | Preserve source state; journal failed before target existence; exact retry allowed. |
| Container start failure, uncertain | Run RPC errors and observation also fails | Journal `failed_after_target_start`; block direct retry and print urgent inspection guidance. |
| Container start failure, running | Run RPC errors but observation finds target running | Stop and keep target; block automatic restore retry. |
| Restore command failure | Target started, restore RPC errors | Stop and keep target and volumes; source state unchanged; journal `failed_after_target_start`. |
| Restore verification failure | Restore exits zero but target is missing, stopped, mislabelled, or duplicated | Do not commit. Stop and keep known target. Block automatic restore rerun. |
| Verified journal update failure | Restore and observation succeed, journal phase save fails | Do not commit from memory. Stop and keep target if possible, and print manual handoff. |
| State save failure | Journal is verified, `SaveAccessoryState` fails | Leave verified target running, state on source, block other accessory operations, and let exact retry perform commit only. |
| Retry before target start | Journal `failed_before_target`, target clean | Recheck everything, then retry normal path. |
| Retry after partial restore | Journal `failed_after_target_start` without verified phase | Refuse restore rerun until operator cleans target container and data. |
| Retry after verified restore | Journal verified, exact target is sole running copy | Commit state without starting or restoring again. |
| Retry after commit | State already points to journal target | Perform journal or event reconciliation only and return success. |
| Mismatched retry | Target or artifact differs from unfinished journal | Refuse and print original attempt details. |
| Started event failure | Event store fails after target start | Print warning, continue from journal, and do not label operation failed solely for the event. |
| Failed event failure | Recovery fails and event store also fails | Return the recovery failure and print a separate event warning without hiding the primary error. |
| Committed event failure | State commit succeeds, event store fails | Print warning and exit success. Retry never restores again. |
| Journal cleanup failure | State points to target, cleanup fails | Warn and exit success; later exact retry cleans only the journal. |
| Planned failover ordering | Resolvable source and empty target | RPC order is artifact preflight, source stop, source stopped observation, target start, restore, verification, commit. No overlap of running primaries. |
| Planned failover source stop failure | Stop RPC fails or source still runs | Refuse target start and keep state on source. |

## Residual risks

- Operator attestation can be wrong. The command makes the decision explicit and auditable, but Ship cannot verify external fencing portably today.
- Provider power state can change after observation. Provider proof must be exact, immediate, and recorded, and external actors must avoid restarting the old machine.
- Legacy accessory state may have only logical name and pool. It cannot support provider proof and may not support same-name replacement recovery.
- Successful restore command exit plus a running managed container is not database-level semantic verification. Database-specific promotion or consistency checks need separate configuration and review.
- Preexisting target volumes can contain data, and current RPCs cannot distinguish newly created from existing volumes. Recovery must report this and never delete them automatically.
- The local state directory is the authority and journal location. Losing it during the incident requires a separate reconstruction procedure, not manual placement reassignment.
- Unlabelled or externally managed database copies are outside agent discovery. The fencing attestation covers every machine that could still serve the saved data, not only Ship-labelled containers.

VERDICT=IMPLEMENT_DISTINCT_RECOVER_COMMAND
