# Ship

Ship is a Kamal-inspired deployment tool for running Docker applications on ordinary Linux servers, with horizontal scaling as a first-class concept and no Kubernetes control plane.

The v1 shape is intentionally small:

- one `ship` Go binary
- YAML config in `ship.yml`
- Hetzner Cloud provisioning
- Docker Engine on hosts
- SSH-framed agent RPC, with no open agent port
- deterministic service placement across host pools
- manual scaling through `ship scale`
- Caddy ingress config generation
- single-primary accessories with backup/restore guardrails
- environment-backed secrets verification

## Quick Start

Build or install the CLI:

```bash
go install ./cmd/ship
ship --help
```

From a blank application repo:

```bash
ship init

# Edit ship.yml for your registry, Hetzner location/server type, host pools,
# service image build/ref, health checks, ingress domains, accessories, and secrets.
ship doctor
ship provision plan production
ship --dry-run provision apply production
ship --dry-run agent install production
ship plan production
ship --dry-run deploy production
```

When the dry-run output looks right and credentials are present:

```bash
export HCLOUD_TOKEN=...
ship provision apply production --yes
ship agent install production
ship secrets verify
ship deploy production
ship status production
ship logs production web --lines 100
```

`ship scale` previews deterministic placement for a new service count. To make the change durable in V1, update `ship.yml` and deploy.

```bash
ship scale production web=10
ship --dry-run deploy production
ship deploy production
```

## Sample App

`testdata/sample-app/` contains a tiny Dockerized Go app used by the acceptance tests:

- `Dockerfile`
- HTTP service command `/app/sample-app server` with `/up`
- worker command `/app/sample-app worker`
- healthcheck command `/app/sample-app healthcheck`
- optional `DATABASE_URL` accessory dependency signal

The normal test suite references this fixture without requiring Docker.

## Config

Ship reads `ship.yml` from the current directory by default. The generated starter config includes:

- `environments.production.provider.hetzner`
- host pools for `web`, `worker`, and `ingress`
- stateless `web` and `worker` services
- a single-primary `postgres` accessory
- required secrets under `secrets`

Useful commands while editing config:

```bash
ship provision plan production
ship plan production
ship secrets verify
ship secrets render production --dry-run
```

## Deploy And Operate

Provisioning reconciles Hetzner servers by Ship labels, writes local host facts, waits for SSH, installs Docker prerequisites, uploads the local `ship` binary, and enables the Ship agent service. It does not delete extra servers during apply; cleanup is an explicit decommission command.

```bash
ship provision apply production --yes
ship --dry-run agent install production
ship agent install production
```

To remove Ship-managed Hetzner servers for an environment:

```bash
ship --dry-run provision decommission production
ship provision decommission production --yes
```

Deploy builds or resolves service images, writes release state, rolls services through the SSH-framed agent, runs health checks, and promotes only healthy releases.

```bash
ship --dry-run deploy production
ship deploy production
ship status production
ship inspect production
ship events production
ship logs production web --replica 1 --lines 200
```

Accessories are managed explicitly:

```bash
ship accessory deploy production postgres
ship accessory status production postgres
ship accessory backup production postgres
ship --dry-run accessory restore production postgres --artifact /var/lib/ship/backups/postgres-20260630T120000.000000000Z.backup
ship accessory restore production postgres --artifact /var/lib/ship/backups/postgres-20260630T120000.000000000Z.backup --yes
```

## Recovery

Failed provision:

```bash
ship provision plan production
ship provision apply production --yes
```

Provisioning is label-based and safe to retry. Existing matching servers are reported as `exists`; extra matching servers are reported but not deleted.

Failed deploy:

```bash
ship recover production
ship events production
ship status production
ship --dry-run deploy production
ship deploy production
```

A failed deploy records a failed release and keeps the previous healthy release as current.

Failed health check:

```bash
ship recover production
ship logs production web --lines 200
ship inspect production
ship deploy production
```

Health failures stop promotion. Ingress reload happens only after rollout health passes.

Rollback:

```bash
ship recover production
ship --dry-run rollback production --to RELEASE_ID --allow-data-rollback
ship rollback production --to RELEASE_ID --allow-data-rollback
```

Use `--allow-data-rollback` when configured accessories make data compatibility a manual decision.

Accessory restore:

```bash
ship accessory status production postgres
ship accessory backup production postgres
ship --dry-run accessory restore production postgres --artifact /var/lib/ship/backups/postgres.backup
ship accessory restore production postgres --artifact /var/lib/ship/backups/postgres.backup --yes
```

Restore validates the saved placement, observed topology, artifact path, and restore check before running the restore command.

## Acceptance Tests

Default CI-safe coverage:

```bash
go test ./...
go build ./cmd/ship
```

The Phase 12 acceptance tests use fake Hetzner, fake Docker, and fake agents to verify the dry-run path and provision/deploy/scale/logs/status/recover/rollback workflow.

Optional read-only live Hetzner gate:

```bash
SHIP_LIVE_HETZNER=1 HCLOUD_TOKEN=... go test ./internal/cli -run TestLiveHetznerAcceptanceGate -count=1
```

Set `SHIP_LIVE_HETZNER_PROJECT` to override the label selector project. The live gate is skipped by default and does not create or destroy servers.

Optional destructive live Hetzner full-cycle gate:

```bash
SHIP_LIVE_HETZNER_DESTRUCTIVE=1 \
HCLOUD_TOKEN=... \
SHIP_LIVE_HETZNER_SSH_KEY=... \
SHIP_LIVE_HETZNER_REGISTRY=ttl.sh/your-public-test-repo \
go test ./internal/cli -run TestDestructiveLiveHetznerFullCycle -count=1
```

The destructive gate creates two servers, bootstraps agents, deploys the sample app, scales it, rolls back, and decommissions the servers. Use a disposable project name and a registry repository that the new hosts can pull from.

Optional local integrations, with no cloud credentials:

```bash
SHIP_LOCAL_REGISTRY_INTEGRATION=1 go test ./internal/docker -run TestLocalRegistryIntegrationBuildPushResolvePull -count=1
go test ./internal/ingress -run TestGeneratedCaddyfileValidatesWithCaddyBinary -count=1
```

The registry test starts a local Docker `registry:2` container. The Caddy validation test runs when a local `caddy` binary is available.

## Safety Notes

Commands that can touch real infrastructure support the global `--dry-run` flag where mutation is possible. Provider and agent interactions are injectable in tests, so normal CI does not require Docker, SSH hosts, registry credentials, or cloud credentials.

Production use should start with `ship doctor`, `ship provision plan`, and `ship --dry-run deploy`.
