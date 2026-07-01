# Migrating from Kamal to Ship

There is no `ship import kamal` command. Migration is a guided translation plus a controlled cutover. This document is the step-by-step playbook an agent should follow.

## What maps cleanly

| Kamal | Ship |
| --- | --- |
| `config/deploy.yml` | `ship.yml` (root defaults + `environments.<env>`) |
| `kamal deploy -d staging` | `ship deploy staging` with `environments.staging` overrides |
| `service: myapp` | `project: myapp` |
| `image: ghcr.io/org/app` | `registry: ghcr.io/org/app` + per-service image ref/build |
| `servers:` host lists | `hosts.pools.<pool>.hosts` or `count` + cloud provider |
| Kamal roles (`web`, `job`, …) | Separate `services.<name>` with `pool:` assignment |
| `env.clear` / `env.secret` | `secrets` list + `ship secrets set` (age-encrypted) |
| `.kamal/secrets` | `.ship/secrets/*.age` + `.ship/secrets/*.recipients` |
| `accessories:` | `accessories:` (similar shape; Ship manages placement explicitly) |
| `registry:` | `registry:` at root |
| `builder:` | `services.<name>.image.build`, `buildpack`, BuildKit cache/secrets |
| `ssh:` | `ssh:` (root, env, and pool overrides) |
| `hooks` / `.kamal/hooks/` | `hooks.pre_deploy`, `pre_build`, `post_deploy`, `deploy_failed` |
| `logging:` | `logging:` (root or per-service) |
| `volumes:` | `services.<name>.volumes` or `accessories.<name>.volumes` |
| `labels:` | `services.<name>.labels` |
| Kamal cron | `services.<name>.schedules` |
| `kamal app exec` | `ship exec ENV SERVICE -- COMMAND` |
| `kamal app logs` | `ship logs ENV SERVICE` |
| `kamal accessory *` | `ship accessory *` |
| `retain_containers` / image cleanup | `ship prune ENV` |

## What differs — read before translating

### Proxy and ingress

Kamal uses **kamal-proxy** (Traefik-based) with per-role `proxy:` config. Ship uses a **managed Caddy ingress** container on `ingress` pool hosts.

Translation checklist:

- Move public domains from Kamal `proxy.host` / role proxy config to `services.<name>.ingress.domains`.
- Move health check paths to `services.<name>.health.http` and optionally `services.<name>.ingress.health`.
- TLS is automatic via Caddy; no Traefik label passthrough.
- Redirect rules → `services.<name>.ingress.redirects`.
- If Kamal published ports directly (no proxy), use `services.<name>.ports` or `publish`.
- Plan an `ingress` pool (often 1–2 hosts) unless all traffic terminates on app hosts with explicit publish rules.

### Roles vs services

Kamal roles are variants of one service image. Ship models each role as its own **service** entry (usually sharing the same image build).

Example Kamal pattern:

```yaml
# config/deploy.yml (Kamal)
service: myapp
image: ghcr.io/acme/myapp
servers:
  web:
    hosts: [1.2.3.4, 5.6.7.8]
  job:
    hosts: [1.2.3.4]
    cmd: bin/jobs
```

Ship equivalent:

```yaml
# ship.yml
project: myapp
registry: ghcr.io/acme/myapp

environments:
  production:
    provider:
      manual: {}
    hosts:
      pools:
        web:
          hosts: [1.2.3.4, 5.6.7.8]
        worker:
          hosts: [1.2.3.4]

services:
  web:
    image:
      build: .
    command: ./bin/web
    pool: web
    scale: 2
    ingress:
      domains: [example.com]

  job:
    image:
      build: .
    command: bin/jobs
    pool: worker
    scale: 1
```

### Environments / destinations

Kamal merges `config/deploy.yml` + `config/deploy.<dest>.yml`. Ship merges root `ship.yml` + `environments.<env>` overrides. Put shared services/accessories at root; put provider, host counts, domains, and env-specific scale under each environment.

### Secrets

Kamal reads plaintext/dotenv from `.kamal/secrets` and `.kamal/secrets.<dest>`. Ship uses **age-encrypted** secret stores:

```bash
age-keygen -o ~/.config/ship/identity.txt
ship secrets init production --recipient age1...
export SHIP_SECRETS_IDENTITY_FILE=~/.config/ship/identity.txt

# For each secret name from Kamal env.secret:
VAR_VALUE=... ship secrets set production VAR_NAME
```

Commit `.ship/secrets/*.age` and `.ship/secrets/*.recipients`. Never commit identity files.

Map Kamal `env.clear` values into `ship.yml` only when they are truly non-secret config (prefer env-specific yaml overrides for non-secret env-specific settings).

### Accessories

Both tools have `accessories`, but Ship treats them as **explicitly managed** single-primary containers:

```bash
ship accessory deploy production postgres   # first-time or image change
ship accessory status production postgres
```

Translate Kamal accessory images, volumes, ports, env/secrets, and host placement. Ship places accessories on a configured `pool` (commonly `worker`). Verify backup/restore config if the database is production-critical:

```yaml
accessories:
  postgres:
    image: postgres:17
    pool: worker
    primary: true
    volumes: [postgres-data:/var/lib/postgresql/data]
    backup:
      command: pg_dumpall
      restore_command: psql -f "$SHIP_BACKUP_ARTIFACT"
      required: true
```

### Builder and image tags

| Kamal `builder` | Ship |
| --- | --- |
| `arch: amd64` | `image.platform` or `image.platforms` |
| `remote` builder | `image.builder` (Buildx builder name) |
| `cache` options | `image.cache_from` / `cache_to` |
| `secrets` | `image.secrets` |
| `args` | Dockerfile build args via `image.build` context (use Dockerfile ARG/ENV) |

Ship publishes immutable release tags and optional aliases via `image.tags`.

### Hooks

Kamal hook names map to Ship hook keys:

| Kamal hook | Ship |
| --- | --- |
| `pre-deploy` | `hooks.pre_deploy` |
| `pre-build` | `hooks.pre_build` |
| `post-deploy` | `hooks.post_deploy` |
| `pre-connect` | No direct equivalent; use `pre_deploy` or provisioning scripts |
| `docker-setup` | Handled by `ship agent install` / `ship provision apply` |

Ship exports `SHIP_*` env vars to hooks (see main SKILL.md).

### Features without a direct Ship equivalent

| Kamal feature | Migration note |
| --- | --- |
| `asset_path` (CSS/JS bridging) | No built-in equivalent. Use versioned asset filenames, CDN, or shared volume strategy. |
| `error_pages_path` | Use Caddy custom error handling or app-level error pages. |
| `boot.limit` / `boot.wait` | Use `services.<name>.rolling` (surge, canary, drain, health retries). |
| `minimum_version` | N/A |
| `run_directory: .kamal` | Ship state: `/var/lib/ship` on hosts, `.ship/` locally |
| `aliases` (kamal CLI shortcuts) | Use shell aliases or project scripts; Ship has no alias config. |

## Recommended migration procedure

### Phase 0 — Discovery

Read and inventory:

1. `config/deploy.yml` and all `config/deploy.*.yml`
2. `.kamal/secrets*` (note names, not values in logs)
3. `.kamal/hooks/*`
4. Accessory definitions and data directories on hosts
5. Current server IPs, SSH users, bastion/jump setup
6. DNS records and TLS termination path
7. Registry and image naming conventions

Output a short migration plan for the user: target environments, pool layout, service list, accessory strategy, cutover type (in-place vs new hosts).

### Phase 1 — Scaffold Ship config

```bash
ship init
```

Translate Kamal → `ship.yml` using the mapping above. Start from `internal/config/config.go` `Sample()` and `docs/configuration/README.md`.

For **existing Kamal servers**, prefer:

```yaml
environments:
  production:
    provider:
      manual: {}
    hosts:
      pools:
        web:
          user: deploy
          hosts: [existing-ip-1, existing-ip-2]
```

Or `provider.ssh_config` if hosts are already in `~/.ssh/config`.

### Phase 2 — Secrets and registry

1. `ship secrets init <env> --recipient ...` per environment
2. Migrate each Kamal secret with `ship secrets set`
3. Ensure local Docker is logged into the registry (`docker login` or credential helper)
4. `ship secrets verify <env>`

### Phase 3 — Bootstrap hosts (in-place)

On existing Kamal hosts, Ship will install alongside Kamal temporarily:

```bash
ship doctor
ship hosts production
ship --dry-run provision apply production   # manual provider: records hosts, installs Docker deps
ship provision apply production --yes
ship --dry-run agent install production
ship agent install production
ship version production --json
```

`ship agent install` uploads the Ship binary and enables the agent service. It does not remove Kamal.

### Phase 4 — Rehearse

```bash
ship plan production
ship plan production --observed
ship --dry-run deploy production
```

Confirm placement, ingress domains, image build/push plan, release commands, and accessory actions.

### Phase 5 — Cutover

Typical in-place cutover:

1. Optional: `ship maintenance enable production --message "Deploying"`
2. Deploy accessories first if they are new or changed:
   ```bash
   ship accessory deploy production postgres
   ```
   If reusing an existing Postgres data volume on the same host, align `volumes` paths with the existing Docker volume or host path before first deploy.
3. `ship deploy production`
4. Verify:
   ```bash
   ship status production
   ship health production web
   ship ps production
   ship logs production web --lines 100
   ```
5. `ship maintenance disable production` if enabled
6. Update DNS if ingress hosts changed

### Phase 6 — Decommission Kamal

After Ship is healthy and traffic is confirmed:

On each host (via SSH):

- Stop Kamal proxy: `docker ps` and stop/remove kamal-proxy container
- Stop old Kamal app containers (`<service>-<role>-<hash>` pattern)
- Optionally remove `.kamal/` runtime directory
- Remove Kamal systemd units or cron if any were added manually

Locally:

- Archive `config/deploy.yml` and `.kamal/` (do not delete until the user confirms)
- Remove Kamal from CI/CD; replace with `ship deploy <env>`

```bash
ship prune production   # clean old images on hosts
```

### Phase 7 — Rollback plan

If cutover fails:

- Ship keeps the previous healthy release if deploy health fails
- `ship rollback production` with `--allow-data-rollback` when accessories are involved
- Re-enable Kamal proxy/containers only if you have not removed them yet; otherwise restore from documented Kamal release tag

Document the rollback decision before cutover.

## CI/CD replacement

| Kamal CI | Ship CI |
| --- | --- |
| `kamal deploy -d production` | `ship deploy production` |
| `kamal build` | `ship deploy` builds as part of deploy (or pre-build in CI with docker/buildx, then `image.ref` digest) |
| `kamal app exec` | `ship exec production SERVICE -- ...` |
| Secrets in CI env | `SHIP_SECRETS_IDENTITY_FILE` + `ship secrets set` locally; in CI, inject secrets via env vars before deploy |

Use `ship plan production --json` and `ship doctor --json` as CI gates.

## Staging-first strategy

When the user has Kamal destinations:

1. Translate `deploy.staging.yml` → `environments.staging`
2. Deploy and validate on staging with Ship
3. `ship promote staging production` to promote tested image digests to production (skips rebuild; uses target env secrets/ingress/rollout)

## Questions to ask the user

If not inferable from the repo:

1. Reuse existing Kamal hosts or provision fresh ones?
2. Keep the same Postgres/Redis data volumes in place?
3. Can there be a maintenance window?
4. Does ingress move to dedicated Ship Caddy hosts or stay on app hosts?
5. Which Kamal roles become separate Ship services?
6. Are there Kamal features in use with no Ship equivalent (`asset_path`, custom error pages)?