# Migrating from other tools to Ship

Ship has no automated importers. Use the patterns below based on what the user is leaving.

## Docker Compose on one or more VMs

**Typical source:** `compose.yml`, manual `docker compose up`, ad-hoc systemd units.

**Approach:**

1. Map each Compose service to a Ship `services.<name>` entry.
2. Map `depends_on` to Ship's Docker network (`ship-<project>-<env>`) — services resolve each other by service name; accessories by accessory name.
3. Move stateful services to `accessories` with `primary: true` and explicit volumes.
4. Move published ports:
   - Public HTTP → `ingress` + `services.<name>.ingress.domains`
   - Internal only → no ingress; rely on Docker network DNS
   - Host-bound debug ports → `publish: ["127.0.0.1:PORT:PORT"]`
5. List existing VM IPs under `provider.manual` or `provider.ssh_config`.
6. `ship agent install` → `ship accessory deploy` (for stateful) → `ship deploy`.

**Watch out:** Compose bind mounts become `volumes` entries; named volumes need consistent volume names across cutover.

## Capistrano / Fabric / custom SSH scripts

**Typical source:** `config/deploy.rb`, release directories under `/var/www`, Puma/Passenger on host.

**Approach:**

1. Containerize the app if not already Dockerized (Ship deploys containers).
2. Replace host release dirs with immutable image digests per Ship release.
3. Map roles (`web`, `db`, `sidekiq`) to Ship services and pools.
4. Replace `cap production deploy` with `ship deploy production`.
5. Replace remote `rake db:migrate` with `services.<name>.release.command` or `ship exec`.

**Watch out:** Host-installed Ruby/Node runtimes move into the image. Cron on the host → `services.<name>.schedules`.

## Ansible / Terraform / Pulumi-managed inventory

**Approach:** Keep your infra tooling; add Ship for deploys.

| Tool | Ship provider |
| --- | --- |
| Terraform/OpenTofu outputs | `provider.terraform` |
| Pulumi stack outputs | `provider.pulumi` |
| Ansible inventory | `provider.ansible` |
| OpenSSH config | `provider.ssh_config` |

Flow:

1. Ensure outputs expose host names, addresses, pools, SSH user/port.
2. `ship provision apply` records hosts (no cloud mutations for these providers).
3. `ship agent install` → `ship deploy`.

This is often the lowest-friction path when leaving a DIY deploy layer but keeping the same servers.

## Kubernetes / Nomad / ECS (downshift to VMs)

Not a drop-in migration. Requires:

1. Container images already built for the app (reuse in `image.ref` or `image.build`).
2. Explicit decision on state: move managed DBs to Ship accessories or keep external managed services.
3. Ingress/DNS cutover plan (Ship Caddy replaces Ingress/ALB for HTTP).
4. Replace orchestrator health/readiness with `services.<name>.health`.

Ship targets "a few Linux servers," not cluster schedulers. Expect to simplify topology.

## GitHub Actions / GitLab CI raw `docker push` + SSH

**Approach:**

1. Keep CI image build if desired, or let `ship deploy` build.
2. If CI builds and pushes, pin deploy with digest refs:
   ```yaml
   services:
     web:
       image:
         ref: ghcr.io/acme/app@sha256:...
   ```
3. Store `SHIP_SECRETS_IDENTITY_FILE` in CI secrets for `ship secrets` operations.
4. Replace SSH restart steps with `ship deploy` or `ship restart`.

## Heroku / Fly.io / Render (platform to self-hosted)

1. Export env vars → `ship secrets set` for secret values; yaml for non-secrets.
2. Map `Procfile` process types to separate Ship services (`web`, `worker`).
3. Map platform Postgres/Redis to Ship accessories or external managed DB.
4. Map platform hostname to `services.web.ingress.domains`.
5. Provision servers via a cloud provider in `ship.yml` or point at existing VPS.

## Generic migration checklist

Regardless of source tool:

1. **Inventory** — hosts, processes, volumes, secrets, DNS, TLS, cron, hooks
2. **Containerize** — Ship requires Docker images
3. **Choose provider** — provision new or adopt existing (`manual`, `terraform`, …)
4. **Translate config** — `ship init`, then author `ship.yml`
5. **Secrets** — age-encrypted `.ship/secrets/`
6. **Bootstrap** — `ship doctor`, `ship agent install`
7. **Rehearse** — `--dry-run` on provision and deploy
8. **Cutover** — maintenance window, deploy, verify, decommission old stack
9. **Operate** — `ship status`, `ship logs`, `ship rollback`, `ship accessory backup`

## When to recommend against migrating

Flag these to the user before proceeding:

- Heavy reliance on platform-specific features (autoscaling groups with complex rules, service mesh, multi-region active-active)
- Non-containerized apps the user does not want to Dockerize
- Stateful clusters (Postgres HA, Redis Cluster) beyond Ship's single-primary accessory model
- Need for Kamal `asset_path`-style zero-downtime static asset bridging without filename hashing

Offer alternatives: keep external DB, use manual provider only for deploy layer, or migrate incrementally (staging first, then `ship promote`).