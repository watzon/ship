---
name: ship
description: >
  Deploy and operate Docker apps with Ship on ordinary Linux servers. Use when the user
  mentions Ship, ship.yml, ship deploy, ship provision, ship accessories, ship secrets,
  or wants to set up, migrate to, or switch from Kamal, Capistrano, raw Docker/SSH deploys,
  Docker Compose on VMs, Ansible-driven deploys, or similar tools. Also use for /ship,
  "switch from Kamal to Ship", "migrate to Ship", or day-2 ops like rollback, scaling,
  ingress, accessories, and recovery.
---

# Ship

Ship is a single-binary deployment tool for Docker apps on plain Linux servers. It is Kamal-inspired: YAML config, host pools, horizontally scaled services, single-primary accessories, SSH-framed agents, managed Caddy ingress, encrypted secrets, and release rollback.

Repo docs live in `docs/`. This skill is the agent playbook for setup, migration, and operations.

## When to load references

- **Migrating from Kamal** → read `references/migration-from-kamal.md` first.
- **Migrating from other tools** → read `references/migration-from-other-tools.md`.
- **Config translation examples** → read `references/config-mapping.md`.

## Install

Requires Go 1.26+ for `go install`, and Docker on the machine that runs deploys.

### Option A: `go install` (recommended for dev and CI)

Pin an asset-backed release tag (check [GitHub Releases](https://github.com/watzon/ship/releases) for the newest):

```bash
go install github.com/watzon/ship/cmd/ship@v0.4.6
```

`@latest` is acceptable for interactive use, but never for CI: it can jump to
a tag whose release assets have not finished publishing. Avoid `@main` when
you will install agents on servers — a `@main` build reports a development
version and cannot download release binaries for remote hosts.

From a Ship repo clone:

```bash
git clone https://github.com/watzon/ship.git
cd ship
go install ./cmd/ship
```

Ensure `$(go env GOPATH)/bin` is on your `PATH` (or set `GOBIN`).

### Option B: release binary

Download a published asset from [GitHub Releases](https://github.com/watzon/ship/releases). Archives are named `ship_<version>_<os>_<arch>.tar.gz` for `linux`/`darwin` × `amd64`/`arm64`.

```bash
VERSION=v0.4.6                          # release tag
VER="${VERSION#v}"                      # filename uses version without v prefix
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in x86_64) ARCH=amd64 ;; aarch64|arm64) ARCH=arm64 ;; esac

curl -fsSL -O "https://github.com/watzon/ship/releases/download/${VERSION}/checksums.txt"
curl -fsSL -o "ship_${VER}_${OS}_${ARCH}.tar.gz" \
  "https://github.com/watzon/ship/releases/download/${VERSION}/ship_${VER}_${OS}_${ARCH}.tar.gz"
grep "ship_${VER}_${OS}_${ARCH}.tar.gz" checksums.txt | grep -E '^[0-9a-f]{64} ' | sha256sum -c -
tar -xzf "ship_${VER}_${OS}_${ARCH}.tar.gz" ship
install -m 755 ship ~/.local/bin/ship   # or sudo install to /usr/local/bin
rm ship "ship_${VER}_${OS}_${ARCH}.tar.gz" checksums.txt
```

(On macOS use `shasum -a 256 -c -` in place of `sha256sum -c -`.)

### Two rules that prevent stranded setups

1. **Agent installs need either the Ship checkout or published release
   assets.** `ship provision apply` and `ship agent upgrade` place a
   host-platform Ship binary on each server. Ship cross-compiles it only when
   run inside the `github.com/watzon/ship` source tree; from any other
   directory it downloads `ship_<version>_<os>_<arch>.tar.gz` for its own
   version from GitHub Releases and verifies it against `checksums.txt`. So
   always install a version whose assets exist — verify with
   `ship release check vX.Y.Z` before pinning it in CI.
2. **When resolution fails, run the `Fix:` line.** The error lists every
   strategy Ship attempted (local binary, cross-compile, release download)
   with the reason each failed, and ends with a single `Fix:` command — run
   that instead of falling back to manual binary copying.

### Verify

```bash
ship --help
ship version
```

## First-time setup workflow

Run these in order unless the user already has pieces in place:

1. `ship init` — creates `ship.yml`, `.ship/`, `.ship/secrets.example`, and `.gitignore` entries.
2. Edit `ship.yml` for registry, provider, host pools, services, health checks, ingress, accessories, and secret names.
3. Initialize secrets:
   ```bash
   age-keygen -o ~/.config/ship/identity.txt
   age-keygen -y ~/.config/ship/identity.txt
   ship secrets init production --recipient age1...
   export SHIP_SECRETS_IDENTITY_FILE=~/.config/ship/identity.txt
   DATABASE_URL=... ship secrets set production DATABASE_URL
   ```
4. Validate before mutating:
   ```bash
   ship doctor
   ship config production
   ship hosts production
   ship provision plan production
   ship plan production
   ship plan production --observed   # when hosts already have agents
   ```
5. Provision and bootstrap (skip provision for manual/existing hosts):
   ```bash
   ship --dry-run provision apply production
   ship provision apply production --yes
   ship --dry-run agent install production
   ship agent install production
   ```
6. Deploy:
   ```bash
   ship --dry-run deploy production
   ship deploy production
   ```

Always prefer `--dry-run` on mutating commands until the user confirms the plan.

## Core concepts

| Ship concept | Meaning |
| --- | --- |
| `project` | App name; prefixes containers, networks, state |
| `registry` | Base image registry for built/pushed images |
| `environments.<env>` | Per-env overrides (like Kamal destinations) |
| `hosts.pools` | Named host groups (`web`, `worker`, `ingress`, …) |
| `services.<name>` | Stateless app processes placed across pools |
| `accessories.<name>` | Stateful single-primary services (DB, Redis, …) |
| `secrets` | Encrypted with age in `.ship/secrets/` |
| `ingress.caddy` | Managed TLS reverse proxy |
| `services.<name>.ingress` | Domains, redirects, proxy health for a service |

Pick **one provider per environment**: cloud provisioner (Hetzner, AWS, …) or inventory provider (manual, terraform, pulumi, ansible, ssh_config).

## Command cheat sheet

### Planning and config

```bash
ship config ENV [--json]
ship hosts ENV [--json]
ship provision plan ENV [--json]
ship plan ENV [--json]
ship plan ENV --observed [--json]
ship scale ENV SERVICE=N [SERVICE=N...]
ship doctor [--json]
```

### Provision and agent

```bash
ship provision apply ENV [--yes]
ship provision decommission ENV [--yes]
ship migrate ENV HOST [--yes] [--keep-server] [--artifact NAME=PATH]
ship agent install ENV
ship agent upgrade ENV [--json]
ship version [ENV] [--json]
```

`ship migrate` replaces the server behind one logical host: it provisions a
replacement from current pool settings (edit pool `size`/`location` first to
vertically scale or relocate), moves accessory data via backup/restore, restarts
the host's replicas from the current release, updates ingress, and deletes the
old server. Cloud-provisioned pools only; accessories on the host need
backup/restore commands configured.

### Deploy and operate

```bash
ship deploy ENV
ship promote SOURCE_ENV TARGET_ENV
ship status ENV
ship ps ENV [--service NAME] [--json]
ship health ENV [SERVICE] [--json]
ship logs ENV SERVICE [--replica N] [--failed] [--lines N]
ship exec ENV SERVICE -- COMMAND
ship restart ENV [SERVICE]
ship inspect ENV [--json]
ship support ENV [--json]
ship events ENV
ship releases ENV [--json]
ship releases diff ENV --from OLD --to NEW [--json]
ship lock ENV / ship unlock ENV
ship maintenance enable|disable|status ENV
ship prune ENV
```

### Accessories

```bash
ship accessory deploy ENV [NAME]
ship accessory status ENV NAME
ship accessory backup ENV NAME
ship accessory restore ENV NAME --artifact PATH [--yes]
ship accessory logs ENV NAME
ship accessory exec ENV NAME -- COMMAND
```

### Secrets

```bash
ship secrets init ENV --recipient age1...
ship secrets set ENV NAME            # reads env var NAME unless --value
ship secrets list ENV
ship secrets verify ENV
ship secrets render ENV --dry-run
ship secrets diff ENV
ship secrets export ENV [--redacted]
```

### Recovery

```bash
ship recover ENV
ship rollback ENV [--to RELEASE_ID] [--allow-data-rollback] [--allow-secret-drift]
```

Global flag: `--dry-run` on most mutating commands.

## Agent rules for migrations

When the user says "switch from Kamal to Ship" (or similar):

1. **Read the source config first.** For Kamal: `config/deploy.yml`, `config/deploy.<dest>.yml`, `.kamal/secrets*`, hooks under `.kamal/hooks/`, and any accessory/proxy/role definitions.
2. **Do not assume a converter exists.** Ship has no `ship import`. Translate config manually using `references/config-mapping.md`.
3. **Prefer reusing existing hosts.** Use `provider.manual`, `provider.ssh_config`, `provider.terraform`, `provider.ansible`, or `provider.pulumi` when servers already exist. Only provision new VMs if the user wants a greenfield cutover.
4. **Separate config from secrets.** Map secret *names* in `ship.yml`; migrate values into age-encrypted `.ship/secrets/` with `ship secrets set`.
5. **Plan the cutover explicitly.** Document: DNS/TLS, maintenance window, accessory data continuity, Kamal container cleanup, and rollback path.
6. **Validate incrementally.** `ship doctor` → `ship plan` → dry-run provision/agent → dry-run deploy → real deploy.
7. **Clean up old tooling after success.** Stop/remove Kamal proxy containers, old app containers, and `.kamal` runtime on hosts once Ship is healthy.

## Common config tasks

### Release-phase migrations (Rails, etc.)

```yaml
services:
  web:
    release:
      command: bin/rails db:migrate
      replica: 1
      timeout_seconds: 600
```

### Rolling deploy tuning

```yaml
services:
  web:
    rolling:
      max_surge: 1
      max_unavailable: 0
      health_retries: 5
      drain_timeout_seconds: 10
```

### Ingress with TLS and redirects

```yaml
ingress:
  caddy:
    image: caddy:2

services:
  web:
    health:
      http: /up
    ingress:
      domains: [example.com]
      redirects:
        - from: [www.example.com]
          to: https://example.com
```

### Hooks

```yaml
hooks:
  pre_deploy: [./scripts/check-freeze-window]
  post_deploy: [./scripts/smoke-production]
  deploy_failed: [./scripts/notify-deploy-failed]
```

Root hooks run before environment hooks. Hook env includes `SHIP_PROJECT`, `SHIP_ENVIRONMENT`, `SHIP_RELEASE`, `SHIP_CONFIG`, `SHIP_CONFIG_DIR`.

## Troubleshooting shortcuts

| Symptom | Commands |
| --- | --- |
| Deploy failed, old release still live | `ship recover ENV`, `ship logs ENV SERVICE --failed`, `ship events ENV` |
| Health check failures | `ship health ENV SERVICE`, tune `rolling.health_retries` |
| Config changed but not deployed | `ship status ENV` (config drift), `ship plan ENV --observed` |
| Secret mismatch blocks rollback | `ship secrets diff ENV`, fix secrets or use `--allow-secret-drift` |
| Accessory restore needed | `ship accessory status`, `ship accessory backup`, dry-run restore |
| Fleet agent version drift | `ship version ENV --json`, `ship agent upgrade ENV` |

## Documentation map

| Topic | Repo path |
| --- | --- |
| Quick start | `docs/quickstart.md` |
| Config reference | `docs/configuration/README.md` |
| Providers | `docs/configuration/providers/README.md` |
| Deploy and operate | `docs/deploy-and-operate.md` |
| Recovery | `docs/recovery.md` |
| Sample config | `internal/config/config.go` → `Sample()` |

When editing Ship itself (not deploying an app), run `go test ./...` and `go build ./cmd/ship`.
