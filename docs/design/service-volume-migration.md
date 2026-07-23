# Service volume migration contract

## Decision

Ship should implement an opt-in, application-defined service volume migration policy. Phase 1 must move only Docker named volumes named by that policy. It must refuse undeclared service volumes unless the operator acknowledges each source with a repeatable `--ignore-volume SERVICE=SOURCE` flag. It must never copy a bind mount, relay an artifact through an unbounded memory buffer, or claim zero downtime or zero data loss.

Generic named-volume tar remains deferred. The local experiment in this document preserved the tested metadata, but a concurrent writer produced an application-inconsistent restore. Provider snapshots and bind copying are not phase 1 fallbacks.

This design is based on local `main` at `dbb9770`, after Plans 001, 007, and 009. It does not change accessory migration semantics.

## Repository evidence

- `internal/config/config.go` defines `Service.Volumes` as `[]string`. `validateVolumeSpecs` checks only nonempty text, newlines, and the presence of a colon. It does not determine the mount type, target, or valid options.
- `mergeService` replaces the base volume slice when an environment override has a nonempty volume slice. It does not append. An explicit empty override cannot currently clear the base list.
- `internal/deployment/deployment.go` passes each nonempty string directly to `docker ... -v`. Docker, not Ship, supplies the final interpretation.
- `internal/cli/migrate.go` warns that service data is not migrated. The current order is replacement creation, bootstrap, accessory backup and restore, host-fact repoint, service rollout, old workload stop, and optional old-server deletion.
- Plan 007 tests show that a failure before host-fact repoint keeps the old fact, while a rollout failure after repoint keeps the replacement fact and both servers. The current release record is not replaced by migration.
- `internal/agent/rpc.go` registers all RPCs in `rpcHandlers`. Existing useful methods include `exec_container`, `run_oneoff_container`, `ensure_volume`, `health_check`, `read_file`, and `write_file`.
- Existing file RPCs encode a whole file as a JSON string. `read_file` reads the whole file into memory, and `write_file` accepts whole content in its request. They are unsuitable for volume artifacts.
- `internal/transport/ssh.go` `SSH.CopyTo` connects two SSH processes through an OS pipe. It relays through the controller without holding the whole payload in memory. It has cancellation and a default ten-minute command timeout, but no size, free-space, checksum, atomic destination, or partial-file contract.
- Accessory backup is the closest safety pattern. `internal/accessory/accessory.go` generates a constrained artifact path, writes a temporary file, requires it to be nonempty, and renames it. Accessory migration then streams it with `SSH.CopyTo`, checks that the target file is nonempty, restores, stops the old accessory, and saves state. It does not checksum the transfer or provide service-level application consistency.
- `accessory.NamedVolumes` uses a simple first-colon split. That helper is adequate only for its narrow current use and must not become the service-volume parser.

Docker's own documentation distinguishes [Docker-managed volumes](https://docs.docker.com/engine/storage/volumes/) from [host bind mounts](https://docs.docker.com/engine/storage/bind-mounts/). The future implementation must use Docker-compatible parsing with Linux semantics, such as a maintained Moby parser, rather than adding another colon splitter.

## Current volume-spec taxonomy

### Classification rule

The effective environment config is classified before any provider mutation. "Current valid" below means the string passes Ship's current `validateVolumeSpecs`, not that Docker accepts it.

| Current string class | Example | Ship accepts | Docker class or result | Portable to another host | Discoverable without guessing | Writable | Phase 1 automatic eligibility |
|---|---|---:|---|---|---|---|---|
| Valid named source, absolute target | `uploads:/app/uploads` | yes | named volume | yes as an identity, data is host-local | yes, by normalized source `uploads` | yes unless `ro` | yes, only when explicitly listed in one policy |
| Named source with valid options | `uploads:/app/uploads:ro` | yes | named volume | yes as an identity | yes | no for this mount | yes, because another mount or helper may restore it; policy still owns the source once |
| Absolute source | `/srv/config:/app/config:ro` | yes | bind mount | generally no | yes, but it crosses the host filesystem boundary | no for this mount | no; must be explicitly ignored |
| Relative path source | `./data:/app/data` | yes | Docker 29 resolved it as a bind against the CLI working directory | no | no, because the Ship agent working directory is not a storage contract | normally yes | no; classify as ambiguous and refuse automatic migration |
| Bare relative-looking source without a slash | `data:/app/data` | yes | named volume | yes as an identity | yes | yes | yes when selected |
| Empty source | `:/app/cache` | yes | Docker 29 rejected it | no | no | unknown | unsupported |
| Invalid or relative target | `data:app/data` | yes | Docker rejects or interprets outside Ship's contract | no | no | unknown | unsupported |
| Unknown or malformed option list | `data:/app/data:ro,unknown` | yes | Docker-dependent rejection | no | no | unknown | unsupported |
| Windows drive syntax on Linux | `C:\data:/app/data` | yes | Docker 29 rejected it as an invalid mode | no | no | unknown | unsupported on Ship's Linux hosts |
| Extra colon fields | `data:/app/data:ro:extra` | yes | rejected or platform-dependent | no | no | unknown | unsupported |
| Target-only anonymous mount | `/app/cache` | no | Docker supports an anonymous volume | no stable source identity | no | yes | unsupported because current config rejects it and migration cannot name it |

Options do not create a new data identity. `ro`, `rw`, `z`, `Z`, propagation values, and comma-separated flags belong to the mount attachment. The data identity is the parsed source and type. The controlled Docker 29 test accepted `ro,z`, `ro,Z`, and `ro,rprivate` for a bind. The inspect result reported `RW=false` and `Propagation=rprivate`. `z` and `Z` can relabel host files on SELinux hosts, which is another reason bind mounts cannot be copied or recreated automatically.

### Aliasing and topology

| Case | Observed meaning | Contract rule |
|---|---|---|
| One named source mounted at two targets | Both mounts refer to one host volume. The experiment inspected the same volume name at `/mnt/a` and `/mnt/b`. | Normalize and transfer the source once. Preserve the service's original mount strings for rollout. |
| One named source used by two containers on one host | Both containers attach the same host volume. | The migration identity is `(logical host, normalized source)`, not a container target. |
| Same source name on two hosts | With the local driver, each host has independent storage despite the same text name. | Migrate only the source host's identity to its replacement. Do not merge replicas across hosts. |
| Multiple replicas of one service on one host | They can be concurrent writers to the same local volume. | `executor.replica` selects one command executor. The declared consistency command must cover all writers, and Ship stops all source containers using the selected volume before repoint. |
| Same source used by different services | Writer ownership and command ownership are ambiguous. | Phase 1 rejects the migration, even if only one policy lists the source. A later multi-service policy would need an explicit writer set and one owner. |
| Environment override | `mergeService` replaces a nonempty base volume slice. | Resolve the environment first, then classify. A policy inherited from the base must still cover the effective list or validation fails. |

The policy schema below can disambiguate database replicas and multiple volumes owned by one service. It deliberately rejects cross-service shared named volumes rather than guessing an owner.

## Strategy evidence matrix

Scores are `high`, `medium`, or `low` for fitness. Effort is the inverse: `high` means more implementation work.

| Strategy | Application consistency | Downtime | Portability | Security | Metadata fidelity | Size and streaming | Retry safety | Provider neutrality | Effort | Evidence and decision |
|---|---|---|---|---|---|---|---|---|---|---|
| Application-defined backup and restore | high when the declared commands are correct | application-dependent; quiesced mode holds writes, snapshot mode can remain writable | high when the application format is portable | medium; commands are explicit but privileged orchestration needs tight context | high at the logical data layer because the application owns its format | high with file-backed host staging and `SSH.CopyTo` | medium; requires stable operation IDs, checksums, and clean target volumes | high | high | Accessories prove command, artifact, timeout, and restore-check patterns. `exec_container` and the service image provide an application context. Implement first. |
| Generic tar of named volumes | low during writes, high only after a proven stop or freeze | write stop required for a defensible snapshot | medium; named local volumes are portable, drivers may not be | medium with fixed helper mounts, low if paths or archive entries are unchecked | medium; the local GNU tar test preserved the tested mode, UID/GID, symlink, sparse allocation, and xattr, but not every filesystem feature | high with a pipe, provided size and disk limits exist | medium only into operation-created empty volumes | high for the local driver | medium | Concurrent-write experiment restored `account=1` and `ledger=0`. Defer `crash_consistent` until broader metadata and hostile-archive tests pass. |
| Host rsync or tar of bind mounts | low without application quiescence | write stop required | low because host paths and filesystems differ | low; root access to arbitrary or relative host paths is a major boundary | medium at best, filesystem and tool dependent | high for streaming but potentially unbounded on disk | low; destination ownership and preexisting paths are unsafe to reset | high | medium | Current validation accepts absolute and relative binds, and Docker resolved `./data` against the invocation directory. Automatic copying is rejected. |
| Provider snapshot and reattachment | low to medium without an application freeze | potentially low | low across providers, regions, zones, and volume types | medium; provider credentials and attachment permissions expand scope | high for supported block-device snapshots | high because the provider moves blocks outside the controller | provider-specific | low | high | Current provider volume config is unrelated to ordinary Docker local volumes, and no provider snapshot contract exists. Defer as a separate mode. |

No strategy is universally safe. Application mode is a user-authored data protocol. Ship enforces sequencing, bounded transport, and residual-state rules, but it cannot prove that a backup command captures every writer.

## Exact phase 1 policy

### YAML

```yaml
services:
  database:
    image:
      ref: registry.example.com/acme/database:2026-07
    scale: 2
    volumes:
      - database-data:/var/lib/database/data
      - database-wal:/var/lib/database/wal
      - /srv/database.conf:/etc/database/database.conf:ro
    volume_migration:
      mode: application
      volumes:
        - database-data
        - database-wal
      executor:
        replica: 1
        image: service
      consistency:
        strategy: quiesced
        require_maintenance: true
        quiesce:
          command: databasectl quiesce-writes
          timeout_seconds: 120
        resume:
          command: databasectl resume-writes
          timeout_seconds: 120
      backup:
        command: databasectl backup --output -
        timeout_seconds: 3600
      restore:
        command: databasectl restore --input -
        timeout_seconds: 3600
      verify:
        command: databasectl verify-restored-data
        timeout_seconds: 120
      artifact:
        max_bytes: 21474836480
      owners:
        database-data: "999:999"
        database-wal: "999:999"
```

The example database spans a data volume and a WAL volume but produces one application-consistent artifact. The bind `/srv/database.conf` is intentionally outside the policy. Migration must refuse until the operator supplies `--ignore-volume database=/srv/database.conf`. The flag acknowledges that Ship will not copy that source. It does not turn the bind into an eligible source.

`backup.command` writes artifact bytes to stdout. `restore.command` consumes them from stdin. Diagnostic text goes to stderr. Ship does not expose an arbitrary host artifact path inside the container. It sets `SHIP_MIGRATION_ARTIFACT=-`, `SHIP_MIGRATION_ID`, and `SHIP_MIGRATION_SERVICE` to nonsecret values.

`quiesce.command` and source backup run with `docker exec` in the current release's selected replica on the source host. Restore and verify run in a temporary target container made from the exact current service image. The helper mounts only selected named volumes at their configured targets, receives the same scoped environment and secret env file as the service, joins the managed network, publishes no ports, has no restart policy, and never joins ingress. `executor.image` supports only `service` in phase 1. A purpose-built helper image is deferred until image provenance, registry auth, entrypoint, and tool compatibility have a separate design.

For `consistency.strategy: quiesced`, `quiesce` and `resume` are required and must be idempotent. The operator asserts that quiesce covers every writer. If `require_maintenance` is true, Ship refuses unless the existing environment maintenance state is enabled before quiesce. Ship does not toggle maintenance automatically in phase 1.

For `consistency.strategy: application_snapshot`, quiesce and resume must be absent. The backup command must produce an application-consistent artifact during writes, and the operator explicitly accepts loss of successful writes after its consistency point. This mode does not mean crash-consistent tar.

### Go shape

```go
type Service struct {
    // Existing fields omitted.
    Volumes          []string                `yaml:"volumes"`
    VolumeMigration *ServiceVolumeMigration `yaml:"volume_migration"`
}

type ServiceVolumeMigration struct {
    Enabled     *bool                    `yaml:"enabled"`
    Mode        string                   `yaml:"mode"`
    Volumes     []string                 `yaml:"volumes"`
    Executor    VolumeMigrationExecutor  `yaml:"executor"`
    Consistency VolumeConsistencyPolicy  `yaml:"consistency"`
    Backup      VolumeMigrationCommand   `yaml:"backup"`
    Restore     VolumeMigrationCommand   `yaml:"restore"`
    Verify      VolumeMigrationCommand   `yaml:"verify"`
    Artifact    VolumeMigrationArtifact  `yaml:"artifact"`
    Owners      map[string]string        `yaml:"owners"`
}

type VolumeMigrationExecutor struct {
    Replica int    `yaml:"replica"`
    Image   string `yaml:"image"`
}

type VolumeConsistencyPolicy struct {
    Strategy           string                  `yaml:"strategy"`
    RequireMaintenance bool                    `yaml:"require_maintenance"`
    Quiesce            *VolumeMigrationCommand `yaml:"quiesce"`
    Resume             *VolumeMigrationCommand `yaml:"resume"`
}

type VolumeMigrationCommand struct {
    Command        string `yaml:"command"`
    TimeoutSeconds int    `yaml:"timeout_seconds"`
}

type VolumeMigrationArtifact struct {
    MaxBytes int64 `yaml:"max_bytes"`
}
```

The block is opt-in because a nil pointer means no policy. `enabled: false` is the only explicit way for an environment override to disable an inherited policy. A nonnil environment policy replaces the whole base policy. It is never deep-merged. `mode: application` is the only accepted phase 1 mode. `crash_consistent` is a reserved future spelling and must return an unsupported-mode validation error until its gate passes.

Validation after environment resolution must enforce all of the following:

- Every effective mount is parsed with Docker-compatible Linux semantics.
- Every selected source appears in the effective service mount list and parses as a named volume.
- Every unselected source requires an exact `SERVICE=SOURCE` ignore flag at migration time.
- Empty sources, invalid targets or modes, Windows paths, target-only mounts, and ambiguous relative binds are rejected before provider mutation.
- Selected sources are unique after normalization. Repeated targets collapse to one identity.
- A selected source cannot be mounted by a different service on the migrating host. Multiple replicas of the owning service require a valid `executor.replica`.
- `owners` keys equal a subset of selected sources and values are numeric `UID:GID` pairs. Ship changes only the root of a new empty target volume before restore. It never recursively changes restored ownership.
- `artifact.max_bytes` is required and positive. The implementation must also impose an overflow-safe hard ceiling.
- Command text is nonempty, contains no NUL, and has a positive timeout no greater than 24 hours. Defaults must not mean unlimited.
- Snapshot and quiesced consistency fields follow the rules above.

Service-level commands are intentional. A database may span data and WAL volumes but produce one transactionally consistent artifact. Per-volume commands would create independent consistency points and could not represent that transaction. The policy's `volumes` list declares coverage and target preparation, while one backup and one restore define the data protocol.

Existing deploy hooks are not reused. Their working directory, output handling, retry boundary, and artifact lifecycle do not provide a bounded migration protocol.

## Artifact identity and transport

Each migration creates and persists a random operation ID before provider mutation. Retries use the same ID. A new user-requested migration gets a new ID.

The host artifact path is generated by Ship:

```text
/var/lib/ship/migrations/<environment>/<operation-id>/<service>.artifact
```

Components must pass the same state-name validation used for other Ship state. The directory is `0700`, complete files are `0600` and owned by root, and writes use `<service>.artifact.part` followed by `fsync` and atomic rename. Phase 1 supports only a regular local artifact. It does not accept a user path, directory, symlink, device, FIFO, external URI, or object-store upload.

The source agent writes backup stdout directly to the `.part` file while counting bytes and computing SHA-256. It aborts at `max_bytes + 1`, removes the partial file, and never returns artifact bytes in JSON. A complete artifact record contains operation ID, service, exact size, digest, source logical host, current release ID, selected sources, and command exit status.

Before transfer, the target checks that free bytes exceed artifact size plus `max(5% of size, 64 MiB)`. The controller uses the existing two-SSH-process relay model because it does not distribute SSH credentials between hosts or add a network listener. Fixed internal agent stream commands read the source artifact and receive the target artifact. The OS pipe is bounded, so controller memory does not grow with artifact size. The receiver enforces the expected size and `max_bytes`, hashes while writing, rejects excess or short input, removes `.part` on cancellation, and renames only after the expected SHA-256 matches.

This needs fixed, ID-based agent entry points rather than user-built shell paths. A suitable split is control RPCs for prepare, command execution, inspect, verify, and cleanup, plus internal `ship agent artifact-send` and `ship agent artifact-receive` commands for raw SSH stdin and stdout. The existing JSON `read_file` and `write_file` RPCs are explicitly forbidden for artifacts.

### Threat and resource controls

| Risk | Required control | Current evidence or gap |
|---|---|---|
| Unbounded controller memory | Stream through the `SSH.CopyTo` pipe; never encode the artifact in RPC JSON. | `SSH.CopyTo` already pipes processes. File RPCs buffer and are rejected. |
| Source or target disk exhaustion | Required `max_bytes`, source counter, target free-space reserve, and no unlimited timeout. | Current accessory copy has none. |
| Truncation or corruption | Record size and SHA-256 at source, enforce both at receiver, verify again before restore. | Agent binary installation already uses SHA-256, but accessory artifacts do not. |
| Cancellation or broken SSH | Context kills both SSH processes; receiver trap removes `.part`; complete source artifact remains. | `CopyTo` has context cancellation but no artifact cleanup contract. |
| Path traversal | Generate paths from validated state names. Stream endpoints accept IDs, not paths. Confirm the resolved path stays below the migration root. | Accessory restore uses `filepath.Rel`; generic file RPCs are too broad. |
| Symlink or special-file swap | Open staging files with no-follow semantics, require a regular file, use private directories, and reject devices, FIFOs, sockets, and hard-link tricks. | Not present in current copy commands. |
| Malicious archive entries | Phase 1 does not unpack artifacts. The explicit restore command owns the format. A future tar mode must reject absolute paths, `..`, devices, and links escaping the target before extraction. | Generic tar remains deferred. |
| Root filesystem access | Mount only selected named volumes into the helper. Never mount or copy a bind source. | Current mount strings may name arbitrary host paths. |
| Ownership damage | Apply optional numeric ownership only to a newly created empty volume root. Restore and verify own final file metadata. | Existing `ensure_volume` performs recursive chown, so phase 1 needs narrower behavior. |
| Secret disclosure | Reuse the scoped remote env file. Do not place secret values in RPC parameters, command text, events, or argv. Suppress successful command output. Cap failure stderr at 64 KiB and redact every value loaded from the scoped env file before returning it. | Current command result output has no general secret redactor. |
| Controller disk exposure | Do not spool locally. The controller holds only operation metadata and the bounded pipe. | Compatible with current relay design. |
| First-use SSH trust | Honor the existing identity, jump host, options, and known-hosts configuration. Document that `accept-new` has first-contact trust semantics. | `transport.SSH` already supplies these options. |

## Consistency, authority, downtime, and order

### Authority rules

- Before a successful host-fact repoint, the source copy is authoritative. A verified target is staged and must not accept writes.
- After a successful repoint, the target copy is authoritative. The old service remains stopped or quiesced and must never be resumed automatically.
- An error from fact writing is not interpreted by its return value alone. The recovery path re-reads the atomically written fact and follows the observed old or new address.
- Restore verification must pass before repoint. No exception or force flag bypasses this gate.
- The current release ID does not change. Rollout converges that release onto the replacement.
- Complete artifacts use the persisted operation ID, so retry does not create a second backup accidentally.
- Target restore is retried only after Ship deletes and recreates volumes that this operation created and that no container uses. Ship refuses to reset a preexisting or unowned target volume.
- Ship never removes an old Docker volume. Phase 1 requires `--keep-server` for a migration with a volume policy, so provider deletion cannot implicitly destroy the recoverable source. Source deletion remains a separate, explicit operator action after validation.

### State diagram

```text
[validated, source authoritative]
  -> [replacement bootstrapped]
  -> [empty target volumes owned by operation]
  -> [source quiesced, or snapshot consistency point recorded]
  -> [source artifact complete and checksummed]
  -> [target artifact complete and checksummed]
  -> [target restored]
  -> [target application verification passed]
  -> [accessories migrated by the existing contract]
  -> [old service writers stopped]
  -> [host fact repointed, target authoritative]
  -> [target resume command, when quiesced]
  -> [current release rolled out and ingress committed]
  -> [old workloads stopped, old server retained]
  -> [migration complete]

Any failure before fact repoint -> source remains authoritative -> resume the
source and abandon staging, or resume the same operation from its last complete
stage.

Any failure after fact repoint -> target remains authoritative -> keep the
source stopped -> resume target rollout or cleanup. Never rerun backup from the
old host.
```

Service-volume staging occurs before current accessory movement. This keeps source-side dependencies available to the application backup. Target verification must not claim cross-component transactional consistency. After service verification, accessories follow their existing backup, transfer, restore, old stop, and state-save contract. Only then does service cutover proceed.

### Phase semantics

| Phase | Old service and writes | Target | Host fact | Data-loss and downtime meaning |
|---|---|---|---|---|
| Validation and provisioning | running, writable | absent, then empty host | old | none caused by volume migration |
| Quiesced consistency point | service remains present, declared writer set must reject writes | empty volumes | old | write downtime starts; Ship cannot prove the command covers hidden writers |
| Application snapshot point | running and writable | empty volumes | old | successful writes after the application's snapshot are outside the artifact and may be lost |
| Backup and transfer | quiesced stays blocked; snapshot stays writable | artifact only | old | quiesced write downtime continues; snapshot loss window grows |
| Restore and verify | same as previous phase | restored but no service writer or ingress | old | no target writes are allowed |
| Accessory move | same as previous phase | service data verified; accessories use current contract | old | no cross-component atomicity is claimed |
| Source writer stop | all containers using selected sources are stopped | verified and still inactive | old | full service downtime starts; snapshot loss window ends here |
| Fact repoint | stopped | verified and designated authoritative | changes to target | commit boundary; only target may be resumed |
| Target resume and rollout | stopped | resume, start, health check, ingress commit | target | downtime ends only after health and ingress commit |
| Completed with `--keep-server` | stopped, volumes intact | running and writable | target | recoverable old source remains; later writes exist only on target |

For quiesced mode, a correct application command should prevent accepted writes after the consistency point. That is an operator assertion, not a Ship zero-loss guarantee. For snapshot mode, Ship explicitly reports the measured time from the recorded snapshot point to source writer stop as the possible data-loss window. There is no second synchronization pass in phase 1.

`--ignore-volume SERVICE=SOURCE` is repeatable and exact. It must be present for every effective volume source that lacks policy coverage, including binds. Dry-run prints policy-covered and ignored sources, consistency strategy, maximum bytes, and stage order. Ignored data is never copied, and its loss is included in the confirmation text and event record.

## Partial failure and recovery matrix

In the table, "release unchanged" means the current release record keeps the same ID. A partial rollout may have containers, but it does not create a new release. Complete artifacts are retained on failure; `.part` files are removed.

| Failure | Old and new servers | Write availability and authority | Artifact state | Host fact | Current release | Target volumes | Exact safe retry or cleanup |
|---|---|---|---|---|---|---|---|
| Quiesce fails | both remain | old is authoritative; writes may be blocked if the command partly succeeded | none | old | unchanged | empty, operation-owned | Run bounded `resume` on old. If it succeeds, retry quiesce with the same operation. If it fails, keep both servers and report manual application recovery. Never start target. |
| Backup command fails | both remain | old authoritative; blocked in quiesced mode, writable in snapshot mode | source `.part` removed | old | unchanged | empty, operation-owned | Fix the command, then retry backup with the same ID, or run source resume and abandon. No target reset is needed. |
| Artifact transfer is interrupted | both remain | old authoritative; consistency behavior unchanged | complete source retained; target `.part` removed | old | unchanged | empty, operation-owned | Recheck source size and digest, target free space, then retry transfer. Do not rerun backup. |
| Checksum mismatch | both remain | old authoritative | complete source retained; target `.part` or bad complete file removed | old | unchanged | empty, operation-owned | Retry transfer once from the recorded source artifact. Repeated mismatch stops for transport or disk investigation. Do not restore. |
| Target volume creation or ownership fails | both remain | old authoritative | source artifact may be complete and retained | old | unchanged | absent or partially created, never restored | Remove only unused volumes recorded as created by this operation, recreate them, apply root ownership, and resume. Refuse cleanup if a volume predates the operation or is attached. |
| Restore fails | both remain | old authoritative | both complete artifacts retained | old | unchanged | tainted and operation-owned | Remove the target migration helper. Delete and recreate only operation-owned target volumes, then rerun restore from the verified target artifact. Do not layer a second restore over partial data. |
| Restore verification fails | both remain | old authoritative | both complete artifacts retained | old | unchanged | restored but untrusted | Keep target inactive. Inspect bounded verification diagnostics. After fixing, either rerun verify if it is read-only or recreate volumes and restore. Repoint is forbidden. |
| Host-fact repoint reports failure after verified restore | both remain | authority follows the fact observed by a mandatory re-read; target has not accepted writes before the attempt | both complete artifacts retained | observed old or target | unchanged | verified | If fact is old, retry the atomic repoint or resume old and abandon. If fact is target, keep old stopped and continue target resume and rollout. Never guess from the error alone. |
| Service rollout fails after restore | both remain | target authoritative but may be unavailable; old stays stopped | complete artifacts retained | target | unchanged | verified, with possible partial target containers | Resume rollout of the same release and operation. Reuse verified volumes and do not restore again. Plan 007 compensation handles partial containers; operator may run the documented convergence path. |
| Old-server delete fails | both remain | target authoritative and writable; old stopped | retention follows completed operation policy | target | unchanged and healthy on target | verified and active | Phase 1 volume migration cannot reach this through `ship migrate` because `--keep-server` is required. If a later explicit source cleanup fails, retry deletion by recorded provider ID only. Never rerun migration or backup. |

Additional post-repoint failures, including target resume failure, release-state sync failure, or old workload stop warning, use the same rule as rollout failure: target authority follows the fact, old writers remain stopped, and retry resumes the recorded stage. A failure while moving accessories occurs before repoint, so source service authority remains old; the operator follows the existing Plan 007 accessory residual guidance and does not discard the staged service artifact or target volumes.

No row requires the operator to infer the authoritative data copy from timestamps or file contents. Authority comes from the observed host fact and the persisted operation stage.

## Controlled Docker experiments

### Environment

- Docker client and Linux server: `29.4.0`
- Storage driver: `overlayfs`
- Helper: `alpine:3.22`, GNU tar `1.35`, attr `2.5.2`
- Date: 2026-07-17
- Limits: one Docker Desktop Linux engine and one filesystem. ACLs, SELinux relabel results, hard links, device rejection, multiple volume drivers, interrupted extraction, and cross-architecture restore were not exhaustively tested.

### Mount parsing

The test created disposable containers with `docker create`, then used `docker inspect ... .Mounts`. Core commands were:

```sh
host_bind_dir=$(mktemp -d -t ship-plan013-bind.XXXXXX)
docker create --name ship-plan013-parse-a-ctx9ae233 \
  -v ship-plan013-parse-volume-ctx9ae233:/mnt/a \
  -v ship-plan013-parse-volume-ctx9ae233:/mnt/b:ro alpine:3.22 true
docker create --name ship-plan013-parse-b-ctx9ae233 \
  -v ship-plan013-parse-volume-ctx9ae233:/mnt/shared alpine:3.22 true
docker create --name ship-plan013-bind-ctx9ae233 \
  -v "$host_bind_dir:/mnt/config:ro" alpine:3.22 true
docker create --name ship-plan013-z-ctx9ae233 \
  -v "$host_bind_dir:/mnt/options:ro,z" alpine:3.22 true
docker create --name ship-plan013-Z-ctx9ae233 \
  -v "$host_bind_dir:/mnt/options:ro,Z" alpine:3.22 true
docker create --name ship-plan013-prop-ctx9ae233 \
  -v "$host_bind_dir:/mnt/options:ro,rprivate" alpine:3.22 true
docker create --name ship-plan013-relative-ctx9ae233 \
  -v './data:/mnt/data' alpine:3.22 true
docker create --name ship-plan013-empty-ctx9ae233 \
  -v ':/mnt/data' alpine:3.22 true
docker create --name ship-plan013-windows-ctx9ae233 \
  -v 'C:\data:/mnt/data' alpine:3.22 true
```

Results: repeated named mounts and a second container reported the same Docker volume name; absolute and `./data` sources reported `Type=bind`; `./data` resolved to the command working directory; empty source and Windows drive syntax were rejected. Bind options `ro,z`, `ro,Z`, and `ro,rprivate` were accepted in this environment.

### Metadata round trip

The source initialization and stream were:

```sh
docker run --rm -v ship-plan013-meta-src-ctx9ae233:/data alpine:3.22 sh -eu -c '
  apk add --no-cache attr >/dev/null
  mkdir -p /data/nested/deeper
  printf "plain-content\n" > /data/nested/plain.txt
  printf "#!/bin/sh\nexit 0\n" > /data/nested/deeper/tool.sh
  chmod 0751 /data/nested/deeper/tool.sh
  printf "owned\n" > /data/nested/owned.txt
  chown 1234:2345 /data/nested/owned.txt
  ln -s nested/plain.txt /data/plain-link
  truncate -s 67108864 /data/sparse.bin
  printf "tail" | dd of=/data/sparse.bin bs=1 seek=67108860 conv=notrunc 2>/dev/null
  setfattr -n user.ship_plan013 -v preserved /data/nested/plain.txt
'

docker run --rm -v ship-plan013-meta-src-ctx9ae233:/from:ro alpine:3.22 \
  sh -eu -c 'apk add --no-cache tar attr acl >/dev/null; tar --numeric-owner --xattrs --acls --sparse -C /from -cpf - .' |
docker run --rm -i -v ship-plan013-meta-dst-ctx9ae233:/to alpine:3.22 \
  sh -eu -c 'apk add --no-cache tar attr acl >/dev/null; tar --numeric-owner --xattrs --acls --sparse -C /to -xpf -'
```

Result: `PASS` for the tested manifest. Source and destination matched for nested files, executable mode `0751`, numeric ownership `1234:2345`, symlink target `nested/plain.txt`, sparse logical size `67108864` with `8` reported blocks, and xattr `user.ship_plan013=preserved`.

This result is evidence for a later spike, not permission to implement generic tar. Metadata fidelity depends on the exact tar, flags, filesystem, security modules, and archive validation.

### Concurrent writer

The writer intentionally updated two application-correlated files in separate steps while tar ran:

```sh
docker run --rm -v ship-plan013-live-src-ctx9ae233:/data alpine:3.22 sh -eu -c '
  printf "0\n" > /data/account.txt
  printf "0\n" > /data/ledger.txt
  printf "idle\n" > /data/phase
'
docker run -d --name ship-plan013-writer-ctx9ae233 \
  -v ship-plan013-live-src-ctx9ae233:/data alpine:3.22 sh -eu -c '
  i=1
  while :; do
    printf "%s\n" "$i" > /data/account.txt
    printf "account-written\n" > /data/phase
    sleep 5
    printf "%s\n" "$i" > /data/ledger.txt
    printf "ledger-written\n" > /data/phase
    sleep 1
    i=$((i+1))
  done
'
docker run --rm -v ship-plan013-live-src-ctx9ae233:/from:ro alpine:3.22 \
  tar -C /from -cf - . |
docker run --rm -i -v ship-plan013-live-dst-ctx9ae233:/to alpine:3.22 \
  tar -C /to -xpf -
```

Result: `PASS_INCONSISTENCY_DEMONSTRATED`. The restored values were `account=1 ledger=0 phase=account-written`. Tar copied a filesystem state that violated the application's two-file invariant. A helper archive is not application-consistent merely because it is byte-complete and metadata-preserving.

### Cleanup

All experiment containers and the four metadata and live-test volumes were removed. Final filtered checks returned empty output:

```sh
docker container rm --force \
  ship-plan013-writer-ctx9ae233 \
  ship-plan013-parse-a-ctx9ae233 ship-plan013-parse-b-ctx9ae233 \
  ship-plan013-bind-ctx9ae233 ship-plan013-z-ctx9ae233 \
  ship-plan013-Z-ctx9ae233 ship-plan013-prop-ctx9ae233 \
  ship-plan013-relative-ctx9ae233
docker volume rm \
  ship-plan013-parse-volume-ctx9ae233 \
  ship-plan013-meta-src-ctx9ae233 ship-plan013-meta-dst-ctx9ae233 \
  ship-plan013-live-src-ctx9ae233 ship-plan013-live-dst-ctx9ae233
docker ps -a --filter name=ship-plan013 --format '{{.Names}}'
docker volume ls --filter name=ship-plan013 --format '{{.Name}}'
```

## Bounded future implementation plan

### Required phase 1

- `internal/config/config.go`: add the exact policy types, atomic environment override behavior, effective-config validation, numeric owner validation, and mode and consistency constants.
- A focused internal mount-classification package: integrate Docker-compatible Linux parsing, return normalized type, source, target, and options, and reject ambiguous or unsupported syntax. Do not reuse `accessory.namedVolume`.
- `internal/state/state.go`: add a persisted `VolumeMigrationOperation` with stage, operation ID, source and target facts, release ID, source identities, artifact size and digest, created target volumes, authority observation, and timestamps.
- `internal/agent/rpc.go`: register control methods such as `service_volume_backup`, `service_volume_prepare_target`, `service_volume_restore`, `service_volume_verify`, `service_volume_resume`, `service_volume_inspect_artifact`, and `service_volume_cleanup_partial`. The registry will advertise them automatically. Add fixed internal artifact send and receive commands for raw SSH streaming.
- `internal/transport/ssh.go`: add a checked copy wrapper around the existing process pipe. It must accept expected size and digest metadata, propagate cancellation, distinguish source and destination failures, and never spool or buffer the complete payload.
- `internal/cli/migrate.go`: parse exact ignore flags, refuse before mutation, create or resume the operation record, add the stages in this document, stage service volumes before accessories, re-read facts after repoint errors, require `--keep-server`, and print dry-run consistency and loss windows.
- `internal/deployment/deployment.go`: expose a target migration helper action and a rollout entry that does not allow ingress or target writers before the authority commit. Reuse current release images and existing transactional rollout compensation.
- `docs/deploy-and-operate.md` and config reference material: document commands as trusted code, stdout and stdin artifact rules, maintenance, ignored volumes, downtime, loss windows, recovery, and source retention.

Phase 1 must add hermetic tests for:

- config parsing, full-block environment replacement, disable behavior, modes, commands, timeouts, size limits, and owners;
- every taxonomy class, including relative paths, Windows-looking paths, options, repeated targets, environment volume replacement, shared replicas, and cross-service rejection;
- RPC registry negotiation and every new parameter and path validation rule;
- artifact size enforcement, free-space failure, checksum mismatch, cancellation, partial cleanup, symlink races, traversal, special-file rejection, bounded stderr, and secret redaction;
- dry-run and exact `--ignore-volume SERVICE=SOURCE` matching;
- quiesced and snapshot stage order, recorded loss-window timestamps, maintenance preflight, and no target writer before repoint;
- every row of the partial-failure matrix with Plan 007's provider, host-fact, release, container, event, and recovery assertions;
- same-operation retry after interruption, clean-volume restore retry, post-verify resume, fact re-read after ambiguous write error, rollout convergence, and `--keep-server` enforcement;
- proof that file RPCs and controller buffers never receive artifact bytes.

### Deferred work and gates

- `crash_consistent` named-volume tar: run the metadata suite on supported Linux filesystems and volume drivers, add hostile archive and interruption tests, require all writers stopped, and define hard link, ACL, xattr, sparse, UID/GID, symlink, device, and whiteout behavior. The concurrent-write result forbids use under live writers.
- Purpose-built helper images: define digest pinning, registry auth, platform selection, entrypoint, scoped secrets, and compatibility with service mount targets.
- Provider snapshots: create separate provider capabilities and zone, attach, detach, encryption, and application-freeze contracts.
- Direct host-to-host transfer: consider only if it avoids distributing durable credentials and preserves the same bounds and integrity checks.
- Cross-service shared volumes, external artifact URIs, second-pass synchronization, replication, and automated source deletion each require separate designs.

```json
{"verdict":"IMPLEMENT_APPLICATION_DEFINED_VOLUME_MIGRATION"}
```
