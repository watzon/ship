# Config mapping reference

Side-by-side translation patterns for agents authoring `ship.yml`.

## Minimal Kamal → Ship

### Kamal

```yaml
# config/deploy.yml
service: acme
image: ghcr.io/acme/acme

registry:
  server: ghcr.io
  username: acme
  password:
    - KAMAL_REGISTRY_PASSWORD

servers:
  web:
    hosts:
      - 203.0.113.10
      - 203.0.113.11

env:
  secret:
    - DATABASE_URL
    - SESSION_SECRET

proxy:
  ssl: true
  host: app.example.com
  healthcheck:
    path: /up

builder:
  arch: amd64

accessories:
  db:
    image: postgres:16
    host: 203.0.113.20
    port: 5432
    env:
      secret:
        - POSTGRES_PASSWORD
    directories:
      - data:/var/lib/postgresql/data
```

### Ship

```yaml
# ship.yml
project: acme
registry: ghcr.io/acme/acme

ingress:
  caddy:
    image: caddy:2

environments:
  production:
    provider:
      manual: {}
    hosts:
      pools:
        web:
          user: deploy
          hosts:
            - 203.0.113.10
            - 203.0.113.11
        worker:
          user: deploy
          hosts:
            - 203.0.113.20
        ingress:
          count: 1   # or list explicit hosts

services:
  web:
    image:
      build: .
      platform: linux/amd64
    pool: web
    scale: 2
    ports: [3000]
    health:
      http: /up
    ingress:
      domains: [app.example.com]
    secrets:
      - DATABASE_URL
      - SESSION_SECRET

accessories:
  db:
    image: postgres:16
    pool: worker
    primary: true
    volumes:
      - data:/var/lib/postgresql/data
    secrets:
      - POSTGRES_PASSWORD

secrets:
  - SESSION_SECRET
```

Registry auth: Ship uses local Docker credentials (`docker login`) and syncs needed auth to hosts during deploy — no `registry.password` in yaml.

## Multi-role Kamal app

### Kamal

```yaml
servers:
  web:
    hosts: [10.0.0.1, 10.0.0.2]
  workers:
    hosts: [10.0.0.3]
    cmd: bundle exec sidekiq
```

### Ship

```yaml
services:
  web:
    image: { build: . }
    command: bundle exec puma
    pool: web
    scale: 2
    ingress:
      domains: [app.example.com]

  workers:
    image: { build: . }
    command: bundle exec sidekiq
    pool: worker
    scale: 1
```

## Kamal destination overlay

### Kamal

```yaml
# config/deploy.staging.yml
servers:
  web:
    hosts: [10.0.1.1]
proxy:
  host: staging.example.com
```

### Ship

```yaml
environments:
  staging:
    hosts:
      pools:
        web:
          hosts: [10.0.1.1]
    services:
      web:
        scale: 1
        ingress:
          domains: [staging.example.com]
```

Environment `services` and `accessories` entries are deep-merged into the root definitions. You only need to specify the fields that differ for that environment (image, pool, ingress, env, and so on). New services or accessories can still be defined only under an environment.

## SSH / bastion

### Kamal

```yaml
ssh:
  user: deploy
  proxy: deploy@bastion.example.com
  keys: ["~/.ssh/id_ed25519"]
```

### Ship

```yaml
ssh:
  identity_file: ~/.ssh/id_ed25519

environments:
  production:
    ssh:
      jump_host: deploy@bastion.example.com
    hosts:
      pools:
        web:
          user: deploy
```

## Health checks

| Kamal | Ship |
| --- | --- |
| `proxy.healthcheck.path` | `services.<name>.health.http` |
| `proxy.healthcheck.interval` | `services.<name>.ingress.health.interval_seconds` |
| `boot.wait` / custom cmd | `services.<name>.health.command` |
| Docker `HEALTHCHECK` | `services.<name>.runtime.health_cmd` (Docker-native only) |

Deploy promotion uses Ship health gates (`health.http` or `health.command`), not Docker health alone.

## Environment variables

| Kamal | Ship |
| --- | --- |
| `env.clear: FOO: bar` | Prefer yaml config; non-secret runtime env is usually in the image or `command` |
| `env.secret: [FOO]` | List under `secrets:` + `ship secrets set` |
| Per-role `env` | `services.<name>.secrets` or accessory `secrets` |

## Cron

### Kamal

```yaml
# config/deploy.yml
aliases:
  logs: app logs -f
```

```yaml
# .kamal/cron or accessory cron patterns
```

### Ship

```yaml
services:
  web:
    schedules:
      nightly:
        cron: "0 3 * * *"
        command: bin/rails runner 'Cleanup.run'
        replica: 1
        timeout_seconds: 300
```

## Useful validation commands after translation

```bash
ship config production
ship config production --json
ship hosts production
ship plan production
ship secrets verify production
ship doctor staging   # limit remote checks to one environment
```

`ship agent install` enables the systemd unit on hosts that already have `/usr/local/bin/ship`. The binary is uploaded during `ship provision apply` (or `ship agent upgrade`), and Ship cross-compiles or downloads a matching linux/amd64 (or arm64) release when your local CLI is a different OS/arch.

Co-located ingress (app and Caddy on the same host, shared Docker network) uses Docker service aliases such as `web:3000` for upstreams. Dedicated ingress pools reach app hosts via contact addresses or public IPs.

Fix validation errors before any `--dry-run provision apply` or `--dry-run deploy`.