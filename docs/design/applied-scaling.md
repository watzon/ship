# Applied scaling design

Status: design decision at commit `dbb9770`, after the Plan 009 CLI split.

Decision: keep `ship scale` preview-only until a source-preserving YAML writer passes the acceptance matrix in this document. The intended applied model is still to patch the named environment in `ship.yml` and then reuse deploy. Persistent scale overrides in `.ship/` are rejected because they would create a second desired-state source.

## User contract

The current command remains compatible:

```text
ship scale ENV SERVICE=N [SERVICE=N...]
```

It resolves the named environment, applies the assignments in memory, prints the deterministic placement plan, and does not write config or contact hosts. The current implementation also records a local `scale/planned` event. No remote apply is implied by this preview.

After the writer gate in this document passes, the explicit applied form should be:

```text
ship scale ENV SERVICE=N [SERVICE=N...] --apply
```

The phases are distinct:

| Phase | Durable config | Remote state | Contract |
| --- | --- | --- | --- |
| Plain preview | Unchanged | Unchanged | Print the placement that the assignments would produce. |
| `--dry-run --apply` | Unchanged | Unchanged | Validate the proposed durable patch and print both the config targets and deploy plan. |
| Durable write | Updated atomically | Unchanged | Make all assignments the local desired state in one file replacement. |
| Remote apply | Already updated | Converging or converged | Run the existing deploy workflow under the same environment operation lock. |

Applied scaling has these rules:

- The durable config write happens before any build, registry, or host mutation.
- All service names, counts, overlay targets, and the resulting resolved config are validated before a write. Counts retain the current `N >= 0` contract.
- Multiple assignments are one transaction. An invalid assignment or duplicate service name rejects the whole request before the backup or write.
- The first applied version writes only environment-scoped service overrides. It never changes `services.<name>.scale` at the root.
- The command reuses deploy planning, hooks, secret handling, rollout, release records, notifications, deploy lock checks, and the environment operation lock. It does not duplicate those behaviors.
- A failed deploy leaves the written config as desired state. The error prints a shell-quoted retry command of the form `ship deploy ENV --config PATH`, including each supplied `--env-file`, identity, agent, and deploy-lock override flag that affects the retry.
- A later `ship deploy ENV --config PATH` must converge from partial remote state. The applied command does not silently restore old desired state after remote work begins.
- `--dry-run` writes neither config nor remote state. The applied dry run may render a proposed diff and plan in memory.
- Ship does not create a Git commit. It reports the changed path and suggests reviewing it with Git when the path is in a repository.

Non-goals are autoscaling metrics or schedules, provider VM counts, accessory replicas, root or global scale mutation, cross-repository Git commits, hidden commits, and an implicit deploy from the plain preview command.

## Repository evidence

Evidence labels in the option matrix refer to this table.

| ID | Observation |
| --- | --- |
| E1 | [`scaleCmd`](../../internal/cli/deploy_commands.go#L1292) loads and resolves config, changes `Service.Scale` only in memory, prints `planner.DeploymentPlan`, and records `scale/planned`. It accepts zero and unknown-service validation happens before planning. |
| E2 | The plan's sample command fails because `testdata/sample-app/ship.yml` does not exist. Against a disposable config created by `ship init`, `ship scale production web=2` printed two web starts, left `ship.yml` at SHA-256 `23e74159680a58c17b5efd82d05e06b6592f74462ab88bfc86a027ee2f5e023c`, and wrote only a `scale/planned` event. |
| E3 | [`config.Load`](../../internal/config/config.go#L2232) reads exactly one path, unmarshals typed YAML, resolves relative user-data files from that config directory, and validates. [`ResolveEnvironment`](../../internal/config/config.go#L2357) copies root services and merges the named environment's service map over them. |
| E4 | [`mergeService`](../../internal/config/config.go#L3502) applies an environment scale only when `override.Scale > 0`. A live config experiment with root `scale: 8` and environment `scale: 0` resolved to 8, so an environment-specific zero is not currently representable. |
| E5 | The root exposes one string `--config` flag and repeatable `--env-file` flags. [`localStateDirForConfig`](../../internal/cli/shared.go#L104) places `.ship` beside the lexical config path. [`secretSourceOptions`](../../internal/cli/shared.go#L131) sends env files to secret resolution, not YAML resolution. `scaleCmd` does not inspect env files. |
| E6 | [`deployCmd`](../../internal/cli/deploy_commands.go#L319) is one command body through line 653. It loads and plans before acquiring the operation lock at lines 370 to 380. There is no callable deploy entry point that accepts an already-held lock. [`AcquireOperationLock`](../../internal/state/state.go#L200) uses a nonblocking per-environment `flock`. |
| E7 | [`fsatomic.WriteFile`](../../internal/fsatomic/fsatomic.go#L10) already supplies temp-file write, mode setting, file sync, rename, and directory sync. It does not compare an expected source identity or hash and would replace a symlink path itself. Deploy release records store a resolved config hash, and status uses that hash for drift. |
| E8 | The quick start explicitly says scale is a preview and tells the operator to edit `ship.yml` and deploy. `.gitignore` ignores ordinary `.ship/*` state, so runtime overrides would be invisible to normal review. |
| E9 | Re-encoding edited `gopkg.in/yaml.v3` nodes retained data but changed unrelated bytes: it moved a comment, emitted `!!merge`, quoted a flow scalar, normalized CRLF, and added a missing final newline. The full matrix is below. |
| E10 | Replacing only the byte span identified by a plain integer scalar node changed only requested scalar bytes in every existing-node fixture. It preserved comments, unrelated anchors and aliases, quotes, flow style, unknown keys, CRLF, and final-newline state. It safely refused an aliased service and a missing environment service block. |
| E11 | A disposable file experiment showed that `os.Rename(temp, symlinkPath)` replaced the symlink, changed the visible mode from `0640` to `0600`, and left its former target unchanged. Resolving the target first, creating the temp file in the target directory, and applying the target's `0640` mode preserved both the symlink and target mode. |
| E12 | Nine live `ship config staging --json` fixtures confirmed inherited, partial, explicit, environment-only, alias, merge-key, zero, aliased-base, and multiple-service resolution. Exact targets are listed below. |

The drift check from the planning commit showed the Plan 009 CLI split, with `scaleCmd` now in `internal/cli/deploy_commands.go`. No applied scaling or config mutation support has landed. The repository uses `gopkg.in/yaml.v3 v3.0.1`; the plan's reference to `yaml.v4` does not match `go.mod`, and the spike did not add a dependency.

## Source-of-truth options

Scores are `high`, `mixed`, or `poor`. High means the option meets the criterion with bounded work. Poor means repository behavior or an experiment exposes a blocker.

| Criterion | Patch `ship.yml`, then deploy | Persist `.ship/` runtime overrides | Keep preview-only and clarify |
| --- | --- | --- | --- |
| Durability | High once written because every later load and deploy reads the config path. [E3] | Mixed because local state is durable on one runner but is ignored by Git and may be absent on ephemeral CI. [E5, E8] | Poor because the requested count exists only in the printed plan until a person edits config. [E1, E2] |
| GitOps clarity | High because desired scale stays in the reviewed file used for deploy and drift hashing. [E3, E7] | Poor because config and ignored state could disagree with no established precedence or drift UX. [E7, E8] | Mixed because there is one source of truth, but the operator must complete the edit manually. [E8] |
| Failure recovery | High in the proposed sequence because a failed apply leaves deploy-readable desired state and existing release failure records. [E6, E7] | Poor because recovery would need to decide whether config or override wins before status, rollback, and deploy can converge. [E7, E8] | High because preview mutates no config or hosts and the documented manual deploy path remains available. [E1, E8] |
| Comment and anchor preservation | Poor today because whole-document node encoding changes unrelated presentation and merge syntax. [E9] | High because it does not rewrite YAML. [E9] | High because Ship does not rewrite YAML. [E1, E9] |
| Environment overlay semantics | Poor today because missing narrow overrides need insertion, aliased service nodes have no safe local scalar, and `scale: 0` is ignored over a base. [E4, E10, E12] | Poor because an override layer would sit above both root and environment config and require new precedence rules. [E3, E8] | Mixed because preview resolves overlays correctly for positive values, but the user must choose the source node and zero remains defective. [E4, E12] |
| CI and noninteractive use | High after implementation because `--apply` is explicit, validation is deterministic, and no prompt or Git commit is required. [E1, E3] | Mixed because writing state is easy but carrying that state between runners is not. [E5, E8] | Poor for a complete scale workflow because CI must introduce its own YAML editor before deploy. [E8] |
| `--config` paths | Mixed because one arbitrary path works, but symlink identity and the lexical `.ship` location need deliberate handling. [E3, E5, E11] | Mixed because state is derived from the lexical path and can diverge for two symlinks to one target. [E5, E11] | High because the current read-only command already accepts the one configured path without file replacement. [E1, E3] |
| Secrets risk | Mixed because a byte-for-byte backup may contain provider or extension secrets and must be mode `0600`, ignored, and never logged. [E9, E11] | High for counts alone, but this does not offset the second-source problem. [E8] | High because Ship creates no config copy. [E1] |
| Implementation complexity | Poor because it needs a source writer, zero-presence fix, checked atomic replace, symlink handling, and deploy-runner extraction. [E4, E6, E9, E11] | Mixed because the write is simple, but every config consumer, hash, status, rollback, and recovery path would need override semantics. [E3, E7] | High because help and docs can state the current behavior without production changes. [E1, E8] |
| Backward compatibility | High because plain `ship scale` remains unchanged and `--apply` is opt-in. [E1] | Poor because existing deploy and status behavior would change whenever ignored state exists. [E3, E7] | High if the command remains and its preview wording is retained instead of doing a hard rename. [E1, E8] |

Option 1 is the desired model, but it is not ready. Option 3 remains the safe shipping behavior until the writer gate passes. Option 2 is rejected, not held as a fallback.

## YAML preservation experiments

### Method

All programs and fixtures lived under `/tmp` and were not committed. The experiment parsed a single document into `yaml.Node`, located the mapping path `environments.staging.services.<service>.scale`, and tried two mutation strategies:

1. Set the scalar node value and encode the whole document with the repository's `gopkg.in/yaml.v3`.
2. For an existing plain `!!int` scalar, use its line and column to verify the original token and replace only that byte span, applying multiple replacements from the end of the file toward the start.

The acceptance rule was strict: requested scalar bytes or required new mapping bytes may change; comments, aliases, anchors, unknown fields, scalar styles, line endings, final-newline state, file mode, and symlink identity may not. A refusal before writing is acceptable for a construct without a proven local target.

### Preservation matrix

`Node encode` means full document encoding after a node edit. `Span patch` means verified replacement of an existing scalar token.

| Case | Fixture and requested edit | Node encode result | Span patch result | Decision |
| --- | --- | --- | --- | --- |
| Comments before, inline, and after a service | `web.scale: 2` to 3 in a 193-byte file | Fail, 193 to 195 bytes. Every comment survived, but `# after web` moved from service indentation to the preceding `image` value. | Pass, 193 bytes. Only `2` changed to `3`. | Whole-document encoding is not source-preserving. |
| Unrelated anchors, aliases, and merge key | Edit an explicit environment scale while keeping `&defaults`, `*defaults`, `&labels`, and `*labels` | Fail, 179 to 187 bytes. Aliases survived, but `<<: *defaults` became `!!merge <<: *defaults`. | Pass, 179 bytes. Only the scale token changed. | Preserve merge syntax byte-for-byte. |
| Service node is an alias | `services.web: *web` | Refused and left all 97 bytes unchanged. Editing the anchor would affect every alias consumer. | Refused and left all 97 bytes unchanged. | Safe refusal rule: never mutate through an aliased service node. |
| Quoted unrelated scalars | Double-quoted project and image, single-quoted registry and command | Pass, 180 bytes. Quotes stayed unchanged and only scale changed. | Pass, 180 bytes. | Existing scalar edit is safe for this case. |
| Root scale plus explicit environment scale | Root 8, staging 2 to 3 | Pass, 115 bytes. Root remained 8. | Pass, 115 bytes. | The environment scalar is the unique target. |
| Multiple assignments | `web: 2` to 3 and `worker: 4` to 6, while `api: 1` stays | Pass, 126 bytes. | Pass, 126 bytes. | Apply verified edits in descending byte-offset order. |
| Unknown provider fields, custom extensions, and opaque secret-shaped data | Edit web scale beside `x-*` mappings and `ENC[...]` | Pass, 308 bytes. All unknown structures survived. | Pass, 308 bytes. | Never decode to typed config and marshal it for the source write. |
| Flow-style document | One-line nested mappings with `app:v1` | Fail, 70 to 72 bytes. The encoder changed `app:v1` to `'app:v1'`. | Pass, 70 bytes. Flow style stayed byte-identical except scale. | Full encoding changes unrelated scalar style. |
| CRLF with final newline | Five CRLF lines, scale 2 to 3 | Fail, 72 to 67 bytes. Every CRLF became LF. | Pass, 72 bytes. CRLF and final newline stayed. | Line-ending style is part of preservation. |
| LF without final newline | Scale 2 to 3 | Fail, 66 to 67 bytes. The encoder added a final newline. | Pass, 66 bytes. No newline was added. | Final-newline state is part of preservation. |
| Missing environment `services` block | Root web scale 8, empty staging override | A simple node insertion produced the intended 42 new bytes and no unrelated change in this fixture. The same encoder is unsafe when the rest of the file contains the failing comment, merge, flow, or newline cases above. | Refused because there is no existing scalar span. | A layout-aware insertion writer must pass combined fixtures before apply ships. |
| Atomic rename at a symlink path | Real target mode `0640`, link path, new data from mode `0600` temp | Fail. The link became a regular `0600` file containing new data; the old target stayed unchanged. | Not applicable to scalar selection. | Resolve and revalidate the target, write in its directory, and preserve the link. |
| Atomic rename at the resolved target | Same fixture, but rename beside the resolved target with copied mode | Pass. The link remained a link, target data changed, and target mode remained `0640`. | Not applicable. | The checked writer must operate on the resolved regular file. |

The span patch proves that existing plain integer nodes can be edited without collateral bytes. It is not yet a complete writer because common configs inherit scale without an explicit environment block. The next experiment must prove layout-aware insertion against combinations of comments, anchors, merge keys, flow mappings, CRLF, no final newline, Unicode before target columns, multi-digit values, and multiple assignments. The writer must reject rather than re-encode when it cannot prove the insertion.

### Byte-diff examples

An existing commented scalar produces the intended one-token diff with the span strategy:

```diff
       web: # service inline
-        scale: 2 # keep scale note
+        scale: 3 # keep scale note
         image: app:v1
       # after web
```

Full node encoding changes an unrelated merge key:

```diff
       web:
-        <<: *defaults
+        !!merge <<: *defaults
         labels: *labels
-        scale: 2
+        scale: 3
```

Full node encoding also changes flow scalar style:

```diff
-environments: {staging: {services: {web: {scale: 2, image: app:v1}}}}
+environments: {staging: {services: {web: {scale: 3, image: 'app:v1'}}}}
```

The CRLF case changed from these bytes:

```text
environments:\r\n  staging:\r\n    services:\r\n      web:\r\n        scale: 2\r\n
```

to LF-only output under node encoding. The span patch changed only the `2` byte and retained every `\r\n`.

## Environment overlay targeting

Paths below are YAML mapping paths, not filesystem paths. The rule is to write the narrowest explicit override for the named environment. Root service nodes and anchors are never mutated by applied scale.

| Case | Input shape and live resolved value | Exact target | Result |
| --- | --- | --- | --- |
| Root-only inherited scale | Root `services.web.scale: 3`; staging has no service override; resolves 3 | Create `environments.staging.services.web.scale` with the requested value. | Refuse until layout-aware insertion passes. Do not change root scale. |
| Existing partial service override | Root scale 3; staging web has only `command: run-staging`; resolves scale 3 and staging command | Insert `environments.staging.services.web.scale`. | Refuse until insertion passes; keep the partial override intact. |
| Existing explicit environment scale | Root 3; staging 1; resolves 1 | Replace scalar `environments.staging.services.web.scale`. | Existing plain integer span is safe. |
| Environment-only service | No root service; staging web has full service and scale 2; resolves 2 | Replace scalar `environments.staging.services.web.scale`. | Existing plain integer span is safe. |
| Environment service is an alias | Staging `web: *web_defaults`; resolves the anchor's scale 2 | No writable local scalar exists under the environment service path. | Refuse with the alias path. Never mutate `&web_defaults`. |
| Environment service uses a merge key | Staging web mapping contains `<<: *staging_override`; resolves merged scale 2 | Insert an explicit `environments.staging.services.web.scale` beside `<<`. | This is semantically narrow and does not change the anchor, but it is refused until insertion preservation passes. |
| Explicit environment zero over nonzero root | Root 8; staging `scale: 0`; currently resolves 8 | The source scalar is `environments.staging.services.web.scale`, but typed resolution is wrong for the requested value. | Refuse applied scale until `mergeService` uses YAML key presence and resolves zero to zero. |
| Aliased root service with no environment override | Root `web: *web_defaults`; staging resolves scale 5 | Create `environments.staging.services.web.scale`. | Never edit the root alias or anchor. Refuse until insertion passes. |
| Two explicit environment assignments | Staging web 2 and worker 4; resolves 2 and 4 | Replace both `environments.staging.services.web.scale` and `environments.staging.services.worker.scale`. | Validate both spans first, then replace both in one in-memory buffer and one atomic file replacement. |

Additional targeting rules:

- The named service must exist in the resolved environment. A typo never creates a new service.
- Any duplicate mapping key on a required path is an error. The writer does not guess which duplicate is authoritative.
- A service mapping with an explicit scale plus an unrelated alias elsewhere can be edited. An alias at the service node or scale node is refused.
- A merge key inside a concrete service mapping may receive a new explicit scale only after the insertion writer passes. The anchor remains unchanged.
- The initial applied command has no global-scope flag. A future global operation requires a separate design because it affects every inheriting environment.
- `--env-file` affects secret verification during deploy and does not select or change a YAML node. It must be forwarded to the reused deploy runner and retry command.
- Ship supports one config file per invocation. There is no include or multi-config merge layer. `--config PATH` means that file alone is patched.
- A non-default config outside the working directory is allowed because it is explicit. State and backups remain under the lexical config directory's `.ship/`, while atomic replacement occurs beside the resolved target. A symlink chain must resolve to one regular file and remain identical at the pre-write recheck.

## Checked atomic write and deploy handoff

The implementation sequence is:

1. Parse assignments without mutating config. If `--apply` is absent, keep the current preview path.
2. Derive the state directory from the explicit config path and acquire the existing environment operation lock with operation `scale_apply`.
3. Check the persistent deploy lock under that operation lock. Unless the corresponding deploy override was supplied, a locked environment fails before config write.
4. Resolve the config symlink chain, require one regular target with one stable lexical link chain, capture device, inode, mode, size, modification time, and SHA-256, then read raw bytes from the opened target.
5. Parse the raw node tree and typed config. Validate every service and count, reject duplicates and unsafe YAML constructs, build the complete patched byte buffer in memory, reload that buffer as typed config, and verify every requested resolved scale. This requires the zero-presence fix.
6. Compute and print the placement preview from the proposed typed config. For `--dry-run`, stop here without backup, config write, apply events, or remote calls.
7. Immediately before writing, resolve the lexical path again and re-read identity plus SHA-256. Any change fails with `config changed while scaling; review it and rerun`.
8. Write the original bytes as a mode `0600` atomic backup under `.ship/config-backups/ENV/<UTC>-<hash>.yml`. Log only the path and hash, never config content. If backup creation or sync fails, leave config untouched.
9. Create the replacement in the resolved target directory, write patched bytes, apply the original target mode, sync it, recheck the current target identity and hash once more, rename, and sync the directory. Refuse a target with no owner, group, or other write bit even if directory permissions would allow replacement. Refuse non-regular files and hard-linked files until their semantics are designed.
10. Reload the actual path with `config.Load` and verify all resolved scales. If verification fails before remote work, atomically restore the backup, report both errors if restore also fails, and do not call deploy.
11. Invoke a shared `runDeployLocked` workflow with the already-held operation lock, proposed config path, env files, and deploy options. It must not reacquire the lock.
12. On deploy failure, keep the patched config, backup, deploy failure records, and any partial remote state. Print the exact retry command. On success, keep the backup for operator recovery and record `scale_apply/succeeded`.

The identity and hash checks detect normal editor saves between load and the final recheck. Portable filesystems do not offer a general compare-and-swap rename against an uncooperative editor, so the final check-to-rename window remains a residual risk. The implementation tests must inject an editor replacement at every available hook and prove either refusal before rename or preservation of the editor's file. Applied scale must not ship if those tests expose an undetectable overwrite path.

Git dirty state is informational, not a lock. An explicit `--apply` may patch an already-dirty config after printing that fact because the hash guard protects the exact bytes loaded. Ship does not stage, commit, reset, or clean files. Read-only targets fail before backup. Config content and backup bytes never appear in events or error messages.

### Failure boundaries

| Failure boundary | Config after failure | Remote or registry state | Recovery |
| --- | --- | --- | --- |
| Operation lock or deploy lock rejected | Original | Unchanged | Resolve the competing operation or deploy lock, then rerun the same scale command. |
| Parse, service, count, overlay, or plan validation | Original | Unchanged | Fix all reported inputs or config errors, then rerun. No subset is written. |
| File identity or hash changed | Editor's current file | Unchanged | Review the concurrent edit and rerun so the plan uses the new bytes. |
| Backup create or sync failed | Original | Unchanged | Fix `.ship` directory permissions or space and rerun. |
| Temp write, mode, sync, or rename failed | Original path remains the old file under atomic rename assumptions; backup may exist | Unchanged | Fix target directory permissions or space and rerun. Report backup path when present. |
| Post-write reload or scale verification failed, restore succeeded | Original restored | Unchanged | Report a writer defect and keep the backup evidence. Do not deploy. |
| Post-write verification and restore both failed | Unknown local config, backup retained | Unchanged | Stop. Print the config and backup paths plus hashes and require manual restoration before deploy. |
| Deploy preflight, hook, secret verification, build, or push failed | Requested scale remains desired state | Hosts unchanged before their first mutation; registry may contain build artifacts | Run the printed `ship deploy` retry after fixing the cause. |
| Release-state write, accessory ensure, secret write, or rollout failed | Requested scale remains desired state | Partial remote mutation is possible and the release is recorded failed where current deploy can do so | Run the printed `ship deploy` retry. Existing planning and rollout must converge. Do not restore old scale. |
| Post-rollout ingress, schedule, hook, notification, or release-finalization failed | Requested scale remains desired state | New replicas may already be active; release and event state describe the failure as far as current deploy supports | Inspect `ship status` and events, then run the printed deploy retry. |
| Process crash after config rename and before deploy | Requested scale remains desired state | Old remote state | Run `ship deploy ENV --config PATH`. Status should report config drift until it succeeds. |
| Success | Requested scale | Converged release with matching resolved config hash | Review and commit the config change through the repository's normal workflow. |

Scale apply events use the existing event schema: `started`, `config_written`, `deploy_started`, `failed`, and `succeeded`. A failed event includes the phase, requested service names, config hash, and retry command, but no source bytes, secrets, or secret values. Existing deploy events and release status remain authoritative for the remote phase.

## Required implementation scope

No production implementation should start until the writer gate below passes. When it does, use two phases.

### Phase 1: config semantics and source writer

- In `internal/config/config.go`, make `Service` record whether the YAML `scale` key was present, using a custom `UnmarshalYAML` field-presence bit, and change `mergeService` to apply an explicit zero. Root service behavior and the public integer `Scale` value stay intact.
- Add `internal/config/scale_patch.go` with `ScaleAssignment`, `ScaleTarget`, and `PatchEnvironmentScales(raw, env, assignments)`. It returns patched bytes and exact target metadata or a typed refusal. It uses node paths for identity, verified byte spans for replacements, and a layout-aware byte inserter for missing mappings. It never marshals the whole document.
- Add checked source replacement support in `internal/fsatomic`, named `ReplaceCheckedFile`, with explicit expected identity and hash, resolved-target handling, mode preservation, backup creation, file and directory sync, and test hooks for concurrent edits. Do not change existing callers of `WriteFile`.
- Add `internal/config/scale_patch_test.go` and extend `internal/config/config_test.go` for the full preservation and overlay matrices, including zero, aliases, merge keys, CRLF, final newline, Unicode, multi-digit counts, duplicate keys, custom extensions, symlinks, hard-link refusal, modes, read-only files, and concurrent identity changes.

The Phase 1 gate is one hermetic test suite in which every accepted patch changes only the requested scalar or required insertion bytes, every unsafe alias or structure is refused before output, environment `scale: 0` resolves to zero, symlink and mode tests pass, and every injected concurrent edit is detected or preserved. This is the experiment that must pass before the verdict can change.

### Phase 2: deploy reuse and CLI integration

- In `internal/cli/deploy_commands.go`, extract the body after config resolution and lock checks into `runDeployLocked`. Both `deployCmd` and applied scale call it. `deployCmd` still acquires its own lock; scale passes the lock it already holds.
- Extend `scaleCmd` with `--apply` and the deploy options needed for an exact handoff. Keep the no-flag path and output backward compatible.
- Add table-driven tests in `internal/cli/deploy_commands_test.go` for plain preview, applied dry run, multiple atomic assignments, validation before write, deploy-lock behavior, already-held operation lock, write failure, crash-equivalent handoff, deploy failure after config write, exact retry output, events, and a successful config-hash match.
- Extend `internal/cli/acceptance_test.go` with a fake-infrastructure apply flow and partial-rollout retry. Default tests remain hermetic.
- Update `docs/quickstart.md`, `docs/deploy-and-operate.md`, `docs/configuration/README.md`, command help, and the Ship agent skill after behavior lands. Do not introduce a state migration or runtime override record.

Backward compatibility is explicit: plain preview keeps its syntax and behavior, existing config needs no migration, and there is no `.ship` override to import. The zero-presence fix changes an existing environment `scale: 0` from being ignored to being honored; call this correction out in release notes because it can intentionally scale a service to zero on the next deploy.

## Validation record

The spike ran these repository commands from the clean Plan 011 worktree:

| Command | Result |
| --- | --- |
| `git diff --stat 93da974..HEAD -- internal/cli internal/config docs README.md` | Exit 0. Drift was the Plan 009 CLI split and related documentation; no applied writer existed. |
| `go run ./cmd/ship scale production web=2 --config testdata/sample-app/ship.yml` | Exit 1 because the planned fixture file is absent. |
| `ship init` plus the same scale command against a disposable config | Exit 0. Printed the production plan with `web.1` and `web.2`, left config hash unchanged, and recorded only `scale/planned`. |
| `go test ./internal/config -count=1` | Exit 0. |
| `go test ./internal/cli -run 'Test.*Scale' -count=1` | Exit 0 with no matching test names; current scale coverage is embedded in acceptance tests. |
| `go test ./internal/cli -run '^TestAcceptanceDryRunFlowFromBlankProject$' -count=1` | Exit 0 and exercised preview scale in the disposable workflow. |
| `go test ./internal/config ./internal/cli -run 'Test.*Scale\|TestLoad\|TestResolveEnvironment' -count=1` | Exit 0 for both packages; the CLI package had no matching names. |
| Nine `ship config staging --json` overlay fixtures | Exit 0. Resolved scales were 3, 3, 1, 2, 2, 2, 8, 5, and web 2 plus worker 4, respectively. |
| Disposable YAML node, byte-span, symlink, and mode program | Exit 0 with the matrix results recorded above. |
| `git diff --check` | Exit 0. Only this design artifact was present in the worktree. |

VERDICT: KEEP_PREVIEW_ONLY_PENDING_YAML_WRITER
