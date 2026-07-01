# Development


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

## Safety notes

Commands that can touch real infrastructure support the global `--dry-run` flag where mutation is possible. Provider and agent interactions are injectable in tests, so normal CI does not require Docker, SSH hosts, registry credentials, or cloud credentials.

Production use should start with `ship doctor`, `ship provision plan`, and `ship --dry-run deploy`.

## Sample app

`testdata/sample-app/` contains a tiny Dockerized Go app used by the acceptance tests:

- `Dockerfile`
- HTTP service command `/app/sample-app server` with `/up`
- worker command `/app/sample-app worker`
- healthcheck command `/app/sample-app healthcheck`
- optional `DATABASE_URL` accessory dependency signal

The normal test suite references this fixture without requiring Docker.
