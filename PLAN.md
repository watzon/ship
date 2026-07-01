# Ship V1 Implementation Plan

Ship V1 is a Kamal-inspired deployment tool for Docker applications on ordinary Linux servers, with horizontal scaling built in from the start and without a Kubernetes-style control plane.

The guiding product promise for V1:

> A single Go binary can provision a small fleet, install lightweight agents, build and deploy Docker services, scale replicas across host pools, route traffic through managed Caddy ingress, manage single-primary accessories, verify secrets, and roll back releases.

## Phase 0: Project Foundation

Status: completed.

### Tasks

- [x] Create `/Users/watzon/Projects/personal/ship` as a Go module: `github.com/watzon/ship`.
- [x] Add a single binary entrypoint at `cmd/ship`.
- [x] Use `spf13/cobra` for the command tree.
- [x] Use `gopkg.in/yaml.v3` for `ship.yml`.
- [x] Add package boundaries for config, planning, scheduling, agent RPC, SSH transport, Docker, Hetzner, ingress, accessories, secrets, and release state.
- [x] Add README, MIT license, `.gitignore`, and initialize Git.

### Gates

- [x] `go test ./...` passes.
- [x] `go build ./cmd/ship` passes.
- [x] `ship --help` shows the intended V1 command surface.
- [x] The repo has no generated binary or local state checked in.

## Phase 1: Config, Planning, And Dry Runs

Status: mostly completed.

### Tasks

- [x] Implement `ship init` to create `ship.yml`, `.ship/`, and `.ship/secrets.example`.
- [x] Define and validate the V1 YAML config shape:
  - project metadata
  - registry
  - Hetzner provider config
  - host pools
  - services
  - health checks
  - ingress domains
  - accessories
  - required secrets
- [x] Implement deterministic host expansion from pool counts and explicit host lists.
- [x] Implement deterministic service placement using round-robin spread within each pool.
- [x] Implement `ship provision plan ENV`.
- [x] Implement `ship plan ENV`.
- [x] Implement `ship scale ENV SERVICE=N` as a dry-run planning command.
- [x] Implement readable plan rendering for build, push, start, ingress, accessory, and backup-check actions.

### Remaining Tasks

- [ ] Split planning into typed action structs rather than display-only actions.
- [ ] Add plan diffs against observed state, not only desired-state previews.
- [ ] Add config rendering/explain commands if the schema grows.
- [ ] Add JSON output mode for plans for future automation.

### Gates

- [x] `ship init` creates a valid sample project.
- [x] `ship plan production` renders a complete deterministic plan from the sample config.
- [x] Repeated runs of the same config produce byte-for-byte stable plans.
- [x] Invalid configs return actionable validation errors.
- [x] Scaling a service changes only the intended service placement.

### Testing

- Unit tests for config parsing, validation, missing pools, invalid scale values, and default values.
- Unit tests for deterministic host expansion and placement.
- Unit tests for plan rendering.
- CLI smoke tests for `init`, `provision plan`, `plan`, and `scale --dry-run`.

## Phase 2: Local Tooling And Doctor Checks

Status: completed for V1 local and remote prerequisite checks.

### Tasks

- [x] Implement `ship doctor`.
- [x] Validate local Docker availability.
- [x] Validate local SSH availability.
- [x] Validate `HCLOUD_TOKEN` presence for Hetzner operations.
- [x] Validate required environment-backed secrets.
- [x] Report pass/fail checks clearly.

### Remaining Tasks

- [x] Validate registry authentication without assuming `:latest` exists.
- [x] Validate Docker BuildKit support.
- [x] Validate that configured build contexts and Dockerfiles exist.
- [x] Validate SSH reachability for explicit hosts and saved provisioned host facts.
- [x] Validate remote host prerequisites:
  - Linux
  - Docker installed
  - systemd available
  - writable `/var/lib/ship`
  - installed `/usr/local/bin/ship`
- [x] Add `--json` output for doctor results.

### Gates

- [x] `ship doctor` can run before any provisioning.
- [x] Missing local dependencies are reported independently.
- [x] Missing secrets are reported by name, without printing secret values.
- [x] Doctor can distinguish warnings from hard failures.
- [x] Doctor does not mutate local or remote state.

### Testing

- [x] Unit tests for doctor check result aggregation.
- [x] Unit tests for secret digest generation and missing secret reporting.
- [x] Integration tests using fake executables on `PATH` for Docker and SSH.
- [x] Integration tests for explicit and saved host checks using a fake SSH transport.

## Phase 3: Hetzner Provisioning

Status: first API client shape exists; reconciliation is not complete.

### Tasks

- [x] Implement Hetzner desired server planning from host pools.
- [x] Create servers with labels:
  - `managed-by=ship`
  - project name
  - environment name
  - pool name
- [x] Attach configured SSH keys.
- [x] Wait for server creation actions to complete.
- [x] Wait for SSH readiness.
- [x] Bootstrap each server:
  - install Docker
  - create `/var/lib/ship`
  - copy the `ship` binary to `/usr/local/bin/ship`
  - install and enable the Ship agent systemd unit
- [x] Persist provisioned host facts in `.ship/environments/ENV/hosts.json`.

### Remaining Tasks

- [x] Implement idempotent reconciliation:
  - discover existing Hetzner servers by labels
  - create missing servers
  - leave matching servers unchanged
  - report unmanaged or extra servers without deleting by default
- [x] Add `ship provision apply ENV --yes` confirmation behavior.
- [x] Add server delete/decommission flow as a separate explicit command.
- [ ] Support configured firewalls for SSH, HTTP, and HTTPS.
- [ ] Support private networking where available.
- [x] Add robust retry/backoff around SSH readiness and bounded Hetzner action polling.

### Gates

- [x] Running `ship provision apply ENV` twice is safe and idempotent.
- [x] A partially completed provisioning run can be resumed.
- [x] Every successfully provisioned host ends with Docker running and `ship agent status` reachable.
- [x] Host facts are persisted locally.
- [x] No destructive infrastructure changes happen without an explicit command and confirmation.

### Testing

- Unit tests for desired server generation.
- [x] Fake Hetzner API tests for create/no-op/partial-existing scenarios.
- [x] Fake Hetzner API tests for action polling, action timeouts, API errors, and decommission.
- [x] Integration tests against a local fake HTTP server.
- Optional live Hetzner smoke test behind `SHIP_LIVE_HETZNER=1`.

## Phase 4: Agent RPC And Remote Host Operations

Status: basic newline-delimited JSON RPC exists over SSH.

### Tasks

- [x] Keep the control model controllerless:
  - CLI computes plans.
  - CLI invokes agent RPC over SSH.
  - Agent listens on stdin/stdout only.
  - No open agent port is required.
- [x] Implement agent methods:
  - `status`
  - `pull`
  - `run_container`
  - `stop_container`
  - `logs`
- [x] Install the agent as a systemd service.

### Remaining Tasks

- [ ] Add request IDs and structured error codes to RPC.
- [ ] Add agent methods for:
  - Docker inspect
  - list Ship-managed containers
  - health check execution
  - write files atomically
  - read release state
  - write release state
  - Caddy config reload
  - accessory backup/restore
- [ ] Add host-level locks so concurrent deploys do not collide.
- [ ] Add agent version negotiation.
- [ ] Add binary upload/install flow.
- [ ] Add remote state directory migrations.

### Gates

- [ ] Agent RPC round trips are deterministic and line-delimited.
- [ ] Agent methods are idempotent where possible.
- [ ] Agent errors include enough context to diagnose host failures.
- [ ] The CLI can talk to all hosts in a pool and continue reporting per-host failures.
- [ ] Agent install can be re-run safely.

### Testing

- Unit tests for RPC framing, invalid JSON, unknown methods, and agent errors.
- Unit tests for each agent method using fake Docker operations.
- Integration tests with local SSH simulation or process transport.
- Integration tests that execute `ship agent rpc` as a subprocess.

## Phase 5: Docker Build, Registry, And Image Resolution

Status: basic Docker CLI wrapper exists.

### Tasks

- [x] Build images locally from service `image.build` and `image.dockerfile`.
- [x] Push images to the configured registry.
- [x] Allow services to specify a prebuilt `image.ref`.
- [x] Pull images on target hosts through the agent.

### Remaining Tasks

- [x] Generate immutable image tags per release, such as git SHA plus timestamp.
- [x] Resolve pushed images to immutable digests.
- [x] Deploy by digest, not mutable tags.
- [x] Support registry auth checks without requiring a specific existing tag.
- [x] Support build args and target stages.
- [x] Support multi-platform decisions explicitly.
- [ ] Add build log streaming.
- [ ] Add image pruning policy for old releases.

### Gates

- [x] Deploys never depend on a mutable tag once an image has been pushed.
- [x] Build failures stop before any host is mutated.
- [x] Push failures stop before any host is mutated.
- [x] Every host pulls the same image digest for a given release.
- [x] Dry runs never build, push, pull, or start containers.

### Testing

- [x] Unit tests for image tag and digest selection.
- [x] Unit tests for Docker command construction.
- Integration tests with local Docker for build, run, logs, and stop.
- [x] Optional registry integration test using a local registry container.

## Phase 6: Service Deployment And Rolling Updates

Status: deploy currently builds/pushes, computes placements, calls agent pull/run, writes release metadata, and supports dry-run.

### Tasks

- [ ] Convert plans into executable typed actions.
- [ ] Create release metadata before mutation begins.
- [x] Start new replicas according to deterministic placement.
- [ ] Run health gates before routing traffic.
- [ ] Add healthy replicas to ingress.
- [ ] Drain old replicas.
- [ ] Stop old replicas.
- [x] Mark release healthy or failed.
- [x] Keep previous release metadata for rollback.

### Remaining Tasks

- [ ] Implement observed-state inspection before deploy.
- [ ] Implement container naming and labels consistently:
  - project
  - environment
  - service
  - replica
  - release
- [ ] Implement rolling strategy knobs:
  - max unavailable
  - max surge
  - drain timeout
  - health timeout
- [ ] Implement safe retries after partial failure.
- [ ] Implement deployment locks.
- [ ] Implement rollback apply, not just rollback target selection.
- [ ] Handle zero-scale services.
- [ ] Handle removed services and orphaned containers.

### Gates

- [ ] A failed health check prevents traffic shift.
- [ ] A failed deploy leaves the previous healthy release serving traffic.
- [ ] Re-running deploy after a host failure converges safely.
- [ ] Rollback restores the previous image and ingress target set.
- [ ] Service placement remains stable unless scale, pools, or host inventory changes.

### Testing

- Unit tests for release state transitions.
- Unit tests for rollback target selection.
- Unit tests for rolling-update action ordering.
- Fake-agent integration tests for successful deploy, failed pull, failed health, and partial host failure.
- Local Docker integration tests for container lifecycle.

## Phase 7: Caddy Ingress

Status: Caddyfile generation exists; remote rollout is not complete.

### Tasks

- [x] Generate Caddy config from healthy service replicas and service ingress domains.
- [ ] Manage Caddy as a container on ingress hosts.
- [x] Write generated Caddy config to Ship state.

### Remaining Tasks

- [ ] Add agent methods to write Caddy config atomically.
- [ ] Start or update the Caddy container on ingress hosts.
- [ ] Reload Caddy after config changes.
- [ ] Route only to healthy replicas.
- [ ] Support drain behavior during rolling updates.
- [ ] Support HTTP-to-HTTPS and automatic TLS defaults.
- [ ] Add validation with `caddy validate`.
- [ ] Add rollback of ingress config when deploy fails.

### Gates

- [ ] Caddy config validates before reload.
- [ ] Reload failure does not discard the previous working config.
- [ ] Only healthy replicas appear as upstreams.
- [ ] Ingress hosts can be rebuilt from Ship state.
- [ ] Dry-run shows ingress changes without touching hosts.

### Testing

- Unit tests for generated Caddy config.
- Unit tests for domain sorting and upstream sorting.
- Integration tests using a Caddy container for config validation.
- Fake-agent tests for atomic write/reload behavior.

## Phase 8: Accessories

Status: accessory config, planning, and restore guardrails exist.

### Tasks

- [x] Support single-primary accessories.
- [x] Support volumes.
- [x] Support backup command declarations.
- [x] Require explicit restore checks for guarded restore flows.

### Remaining Tasks

- [ ] Implement `ship accessory deploy`.
- [ ] Implement `ship accessory status`.
- [ ] Implement `ship accessory backup`.
- [ ] Implement `ship accessory restore`.
- [ ] Place each accessory on one eligible host.
- [ ] Persist accessory placement in state.
- [ ] Add volume creation and ownership handling.
- [ ] Add backup artifact storage configuration.
- [ ] Add restore dry-run and restore confirmation.
- [ ] Block destructive restore without explicit confirmation.
- [ ] Add failover command that moves a single-primary accessory only after backup/restore checks.

### Gates

- [ ] Accessories are never silently replicated.
- [ ] Backup-required accessories cannot deploy without a backup command.
- [ ] Restore requires explicit confirmation and a valid backup artifact.
- [ ] Accessory placement is stable across deploys.
- [ ] Restore and failover actions are auditable in release/event state.

### Testing

- Unit tests for accessory validation.
- Unit tests for accessory placement.
- Fake-agent tests for deploy, backup, restore, and failure modes.
- Local Docker integration tests for volume-backed accessory containers.

## Phase 9: Secrets

Status: completed for V1 environment-backed secrets.

### Tasks

- [x] Declare required secrets in `ship.yml`.
- [x] Verify required secrets are present in the local environment.
- [x] Print short digests without exposing values.
- [x] Generate `.ship/secrets.example`.

### Remaining Tasks

- [x] Render service and accessory env files for remote hosts.
- [x] Transfer env files securely over SSH.
- [x] Store only digests in release metadata.
- [x] Detect secret drift across hosts.
- [x] Add `ship secrets diff`.
- [x] Add `ship secrets render --dry-run` with redacted values.
- [x] Keep provider integrations such as SOPS, 1Password, Vault, or Doppler out of V1 unless needed later.

### Gates

- [x] Missing secrets block deploy before any host mutation.
- [x] Secret values are never printed.
- [x] Release metadata includes secret digests, not values.
- [x] All hosts receive consistent secret material for a release.

### Testing

- Unit tests for missing/present secrets and digest stability.
- Unit tests for redaction.
- Fake-agent tests for remote env file write.
- Integration tests using temporary env files and local process transport.

## Phase 10: Status, Logs, Events, And Observability

Status: basic desired placement status and agent log calls exist.

### Tasks

- [x] Implement `ship status ENV`.
- [x] Implement `ship logs ENV SERVICE`.
- [x] Show current release from local release state.

### Remaining Tasks

- [x] Inspect observed containers on every host.
- [x] Show desired versus observed state.
- [x] Add event timeline:
  - provision
  - deploy
  - scale
  - rollback
  - accessory backup/restore
  - ingress reload
- [x] Add structured logs for deploy operations.
- [x] Add `ship inspect ENV`.
- [x] Add JSON output modes.
- [x] Add log streaming and follow mode.

### Gates

- [x] Status makes drift obvious.
- [x] Logs can be fetched per service and per replica.
- [x] Deploy failures leave enough timeline data to debug the failure.
- [x] JSON output is stable enough for automation.

### Testing

- Unit tests for status aggregation.
- Fake-agent tests for mixed healthy/unhealthy host states.
- Integration tests for log retrieval from local Docker containers.

## Phase 11: Rollback And Recovery

Status: previous release selection exists.

### Tasks

- [x] Store release metadata locally.
- [x] Select previous release for rollback.

### Remaining Tasks

- [x] Store release metadata remotely on each host.
- [x] Implement rollback execution:
  - pull previous image digests
  - start previous replicas
  - health check
  - switch ingress
  - drain failed/current replicas
  - mark rollback result
- [x] Detect rollback blockers for accessories; no migrations config exists yet.
- [x] Add `ship rollback ENV --to RELEASE`.
- [x] Add failed deploy recovery commands.

### Gates

- [x] Rollback works without rebuilding images.
- [x] Rollback does not require a registry tag lookup when digests are already known.
- [x] Failed rollback preserves enough state for manual recovery.
- [x] Rollback refuses unsafe accessory/data operations unless explicitly confirmed.

### Testing

- Unit tests for rollback selection and blocker detection.
- Fake-agent integration tests for successful rollback and failed rollback.
- Caddy rollback tests.
- Local Docker integration tests for image switchback.

## Phase 12: End-To-End V1 Acceptance

Status: completed for CI-safe V1 acceptance; destructive live Hetzner full-cycle remains manual.

### Tasks

- [x] Create a sample app fixture with:
  - Dockerfile
  - HTTP health endpoint
  - worker command
  - optional accessory dependency
- [x] Create a fake infrastructure test harness for normal CI.
- [x] Create optional live Hetzner acceptance tests behind explicit env flags.
- [x] Document the V1 happy path from blank repo to deployed app.
- [x] Document recovery workflows:
  - failed provision
  - failed deploy
  - failed health check
  - rollback
  - accessory restore

### Gates

- [x] From a clean checkout, a developer can run the dry-run flow end to end.
- [x] Against fake infrastructure, CI verifies provision, deploy, scale, logs, status, recovery, and rollback.
- [ ] Against live Hetzner with explicit credentials, the sample app can be provisioned, deployed, scaled, rolled back, and destroyed. A read-only `SHIP_LIVE_HETZNER=1` gate exists; destructive full-cycle automation is not enabled by default.
- [x] README and command help match actual behavior.
- [x] V1 commands are idempotent enough to retry after interruption.

### Testing

- CI default:
  - [x] unit tests
  - [x] fake provider tests
  - [x] fake agent tests
  - [x] local Docker tests that do not require cloud credentials
- Optional CI/manual:
  - [x] local registry integration
  - [x] Caddy validation
  - [x] read-only live Hetzner acceptance test behind `SHIP_LIVE_HETZNER=1`
- Release gate:
  - [x] `go test ./...`
  - [x] `go build ./cmd/ship`
  - [x] dry-run CLI smoke test from a temp project
  - [x] no generated artifacts in Git status

## Phase 13: Follow-Up Hardening And Live Readiness

Status: completed. CI-safe hardening is covered locally, and the fully destructive live Hetzner full-cycle has passed with real disposable infrastructure.

### Tasks

- [x] Reconcile stale unchecked items from earlier phases against the implemented V1 behavior, then either mark them complete, move them here, or delete them as obsolete.
- [x] Teach `ship doctor` to read saved host facts so remote checks use provisioned contact addresses instead of logical host names.
- [x] Run a destructive live Hetzner acceptance flow behind explicit operator opt-in:
  - provision sample app hosts
  - install agents
  - deploy the sample app
  - scale a service
  - roll back a release
  - destroy/decommission all created infrastructure
- [x] Implement the explicit server delete/decommission flow required by the live acceptance cleanup path.
- [x] Finish provisioning bootstrap hardening beyond the existing install command:
  - wait for SSH readiness
  - install Docker/systemd prerequisites
  - upload or install the Ship agent binary
  - add retry/backoff around Hetzner actions and SSH readiness
- [x] Add local registry integration coverage for build, push, digest resolution, pull, and rollback-style digest reuse without depending on external registry state.
- [x] Add Caddy validation coverage for generated configs when a local Caddy binary is available; leave container management out of V1.
- [x] Complete ingress hardening that is in V1 scope:
  - validate generated Caddy config in an integration test
  - record host-specific reload failures
  - improve rollback coverage for ingress config failures
  - verify behavior when services have no explicit health checks
- [x] Finish deploy strategy knobs already present in the schema:
  - `max_surge`
  - `max_unavailable`
  - drain timing
- [x] Add deployment and host lock timeouts so a stuck remote lock, SSH command, or Hetzner action cannot block forever.
- [x] Enforce agent protocol/version negotiation before mutating remote state.
- [x] Improve rollback/recovery durability beyond the current local/remote state model:
  - make remote release-state writes less prone to partial-success ambiguity
  - refine rollback target selection when current and previous releases overlap
  - add more fixed-port rollback coverage
- [x] Improve observability commands that are in V1 scope:
  - make `logs --follow` behave like a real follow mode or rename/document the bounded polling behavior
  - add more structured event detail for failed ingress reloads and remote host failures
  - add JSON output for doctor if still useful
- [x] Tighten secrets behavior beyond V1 environment-backed secrets:
  - validate missing secrets during dry runs where safe
  - revisit secret digest salting/truncation tradeoffs
- [x] Add accessory follow-up operations beyond V1 deploy/backup/restore:
  - failover command for single-primary accessories
  - stronger backup/restore integration tests
  - persisted contact-address coverage for accessory state
- [x] Add image lifecycle improvements beyond V1 digest deploys:
  - build log streaming
  - remote/image pruning policy
  - explicit registry auth validation that does not assume `:latest`
- [x] Revisit lower-priority planning UX and defer nonessential automation output from V1:
  - typed/JSON plan output
  - plan diffs against observed state
  - config render/explain command

### Gates

- [x] `ship doctor` works after Hetzner provisioning without requiring DNS records for logical host names.
- [x] A fully destructive live Hetzner acceptance run can create and clean up all infrastructure without manual steps.
- [x] Local registry and Caddy integration tests can run on a developer machine without cloud credentials.
- [x] Long-running remote operations have bounded waits, useful errors, and retry guidance.
- [x] Remaining unchecked items from Phases 1-12 are either completed, intentionally deferred here, or removed as obsolete.

### Testing

- [x] Unit tests for host-fact-aware doctor remote checks.
- [x] Fake-provider tests for explicit decommission/destroy.
- [x] Local registry integration tests.
- [x] Caddy validation tests.
- [x] Optional destructive live Hetzner acceptance test behind explicit env flags.
- [x] Regression tests for bounded SSH/action timeouts.

## V1 Completion Definition

Ship V1 is complete when:

- A new user can run `ship init`, edit `ship.yml`, run `ship doctor`, provision Hetzner hosts, install agents, deploy services, scale a service, inspect status/logs, and roll back a release.
- Deploys are based on immutable image digests.
- Agents are installed and contacted only through SSH-framed RPC.
- Caddy ingress routes only to healthy replicas.
- Accessories are supported as single-primary services with explicit backup and restore flows.
- Secrets are verified, transferred securely, and tracked by digest only.
- Core operations are idempotent and recoverable after partial failure.
- Unit, fake integration, Docker integration, and optional live Hetzner tests cover the main V1 workflows.
