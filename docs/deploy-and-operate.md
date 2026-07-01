# Deploy and operate


Provisioning reconciles provider servers by Ship ownership metadata, writes local host facts, waits for SSH, installs Docker prerequisites, uploads the local `ship` binary, and enables the Ship agent service. It does not delete extra servers during apply; cleanup is an explicit decommission command.

```bash
ship provision apply production --yes
ship --dry-run agent install production
ship agent install production
ship version production
ship --dry-run agent upgrade production
ship agent upgrade production
```

To remove Ship-managed provider servers for an environment:

```bash
ship --dry-run provision decommission production
ship provision decommission production --yes
```

Deploy builds or resolves service images, writes release state, rolls services through the SSH-framed agent, runs health checks, and promotes only healthy releases.

```bash
ship --dry-run deploy production
ship deploy production
ship promote staging production
ship status production
ship hosts production
ship version production
ship agent upgrade production --json
ship ps production
ship health production
ship maintenance status production
ship inspect production
ship support production --json
ship events production
ship releases production
ship restart production web --replica 1
ship logs production web --replica 1 --lines 200
ship accessory logs production postgres --lines 200
ship exec production web --replica 1 -- bin/rails db:migrate
ship accessory exec production postgres -- psql -c 'select 1'
ship prune production
ship lock production --message "database maintenance"
ship unlock production
```

Tune service rollout behavior with `services.<service>.rolling`. Ship supports surge and unavailable limits, health retry pacing, drain waits, and canary gates. `canary_pause_seconds` pauses the rollout after the first new replica passes health checks; set `canary_replicas` to require a larger first batch before the pause. Ingress promotion still happens only after the full rollout succeeds.

```yaml
services:
  web:
    rolling:
      max_surge: 1
      max_unavailable: 0
      canary_replicas: 1
      canary_pause_seconds: 60
      health_retries: 5
      health_interval_seconds: 3
      drain_timeout_seconds: 10
```

Each release stores a hash of the resolved `ship.yml` environment config. `ship status` and `ship inspect --json` report config drift when the current config differs from the config that produced the deployed release, so operators can spot undeployed changes before chasing container drift.

`ship ps ENV` lists observed Ship-managed service, ingress, and accessory containers for the desired placement. Use `--all` to include extra old or wrong-release containers, `--service NAME` to narrow the view, and `--json` for automation.

`ship hosts ENV` lists logical hosts, pools, SSH users, contact addresses, and SSH overrides from the same resolved inventory deploys use. Use `--json` for inventory exports or automation that needs to know where Ship will connect.

`ship version` prints the local Ship binary version and supported agent protocol range. `ship version ENV` also asks every resolved host agent for its version, protocol, Docker readiness, state directory, and supported RPC methods; use `--json` for fleet audits and upgrade checks.

`ship agent upgrade ENV` uploads the currently running local Ship binary to every resolved host through the SSH-framed agent RPC, verifies the SHA-256 checksum, and records upgrade events per host. Use `--dry-run` to preview the target path and digest, and `--json` for upgrade reports.

`ship health ENV [SERVICE]` runs the configured command or HTTP health checks against the current release without deploying. Use `--replica N` to check one placed service replica and `--json` for monitors or incident tooling.

`ship support ENV` collects a redacted incident bundle with resolved config, host inventory, doctor checks, observed status, recent releases, accessories, and recent events. Use `--json` for the complete artifact, and tune noisy sections with `--events-limit` and `--releases-limit`.

`ship maintenance enable ENV` replaces ingress routing with a generated Caddy 503 page for all configured ingress domains; `ship maintenance disable ENV` restores normal ingress from the current config. Deploys preserve an enabled maintenance page, so traffic returns only through an explicit disable. Use `--message` to customize the body and `ship maintenance status ENV --json` for automation.

`ship logs ENV SERVICE` reads logs from the current release by default. Use `--release RELEASE_ID` to inspect a specific release container, or `--failed` to fetch logs for the newest failed release that included the service. `ship accessory logs ENV NAME` reads logs from the persisted accessory container after checking saved placement and observed topology; use `--lines`, `--follow`, and `--json` the same way as service logs.

Managed Caddy ingress publishes TCP 80, TCP 443, and UDP 443. UDP 443 enables HTTP/3/QUIC for clients that support it, while browsers fall back to HTTP/2 or HTTP/1.1 when QUIC is unavailable. Ship-managed firewalls and security groups open UDP 443 alongside HTTP/HTTPS; when you use an external firewall, open UDP 443 yourself for HTTP/3.

Managed Caddy ingress can also serve redirect-only domains. This is useful for canonical `www` to apex redirects, renamed products, or legacy domains that should keep their paths and query strings:

```yaml
services:
  web:
    ingress:
      domains: [example.com]
      redirects:
        - from: [www.example.com, old.example.com]
          to: https://example.com
          code: 308
```

Redirects preserve the original URI by default, so `/pricing?ref=old` becomes `https://example.com/pricing?ref=old`. Set `preserve_uri: false` for landing-page redirects that should always go to the exact `to` URL.

Managed Caddy ingress also protects live traffic after deploy. Ship enables passive upstream failure quarantine by default, and when a service defines `health.http`, the generated reverse proxy performs active background checks with the same path so unhealthy replicas are pulled out of load balancing until they recover. Tune or disable this per service with `services.<service>.ingress.health`:

```yaml
services:
  web:
    health:
      http: /up
    ingress:
      domains: [example.com]
      health:
        interval_seconds: 10
        timeout_seconds: 3
        fails: 2
        passes: 1
        try_duration_seconds: 5
        passive_fail_duration_seconds: 30
        passive_max_fails: 1
        unhealthy_status: [5xx]
```

Use `ingress.health.path` to check a different proxy-only endpoint, or `ingress.health.enabled: false` to omit Ship-managed proxy health directives for that service.

Set root `logging` defaults, or `services.<service>.logging` overrides, to pass Docker logging driver settings to service containers and release one-offs. This is useful for disk-safe rotation with `json-file` or for routing logs to drivers such as `local`, `journald`, `fluentd`, or `awslogs`.

```yaml
logging:
  driver: json-file
  options:
    max-size: 10m
    max-file: "3"

services:
  web:
    logging:
      options:
        tag: "{{.Name}}"
```

Use `services.<service>.image.cache_from` and `cache_to` to opt into Docker BuildKit external caches for faster repeat deploys and CI builds. Add `image.secrets` and `image.ssh` when builds need private package registries or Git dependencies without baking credentials into image layers. Set `image.sbom` or `image.provenance` to publish Buildx attestations alongside the image for supply-chain audits.

When cache, secret, or SSH fields are set, Ship uses `docker buildx build --load` and then pushes the image through the normal deploy flow. When SBOM or provenance is enabled, Ship uses `docker buildx build --push` so the registry receives the attestation metadata, then skips the separate `docker push`:

```yaml
services:
  web:
    image:
      build: .
      tags:
        - latest
        - production
      builder: ship-cloud
      platforms:
        - linux/amd64
        - linux/arm64
      pull: true
      no_cache_filter:
        - install
      cache_from:
        - type=registry,ref=ghcr.io/acme/app:build-cache
      cache_to:
        - type=registry,ref=ghcr.io/acme/app:build-cache,mode=max
      secrets:
        - id=npm_token,env=NPM_TOKEN
        - id=bundle,src=.bundle/credentials
      ssh:
        - default
      sbom: true
      provenance: mode=max
```

Use `image.tags` to publish stable service-prefixed aliases alongside the immutable release tag. For service `web`, `tags: [latest, production]` publishes `repo:web-latest` and `repo:web-production`, while deploys still resolve and roll out the release-specific digest. Use `image.platform` for a single target platform. Use `image.platforms` for a multi-platform image; Ship publishes those builds directly with Buildx so the registry receives a multi-architecture manifest before deploy digest resolution. Set `image.builder` to an existing Buildx builder name, such as a Docker Build Cloud builder or an SSH-backed builder created with `docker buildx create`, when builds should run somewhere other than the default local builder. Set `image.pull: true` to refresh base images, `image.no_cache: true` for a fully fresh rebuild, or `image.no_cache_filter` to bypass cache only for named Dockerfile stages.

Cache entries use Docker's BuildKit cache backend syntax, so registry, GitHub Actions, local, and other BuildKit-supported cache exporters can be used where your Docker builder supports them. Build secrets are available in Dockerfiles with `RUN --mount=type=secret,id=npm_token ...`, and SSH mounts with `RUN --mount=type=ssh ...`. `sbom` and `provenance` accept either booleans or Docker Buildx option strings such as `generator=...` or `mode=max`.

For apps without a Dockerfile, set `services.<service>.image.buildpack` to build with Cloud Native Buildpacks through the local `pack` CLI. Ship still tags the image with the immutable release tag, supports `image.tags` aliases through `pack --tag`, resolves the final registry digest, and deploys that digest like any Docker-built image. Set `publish: true` to have `pack build --publish` write directly to the registry and skip Ship's separate `docker push`.

```yaml
services:
  web:
    image:
      build: .
      tags: [latest]
      buildpack:
        builder: paketobuildpacks/builder-jammy-base
        buildpacks:
          - paketo-buildpacks/nodejs
        env:
          BP_NODE_RUN_SCRIPTS: build
        descriptor: project.production.toml
        pull_policy: if-not-present
        publish: true
```

Buildpack mode is intentionally separate from Dockerfile/Buildx mode. Use `image.buildpack.builder` for the Buildpacks builder image; `image.builder` remains the Docker Buildx builder selector.

Use `services.<service>.volumes` to mount Docker volumes or existing host paths into service containers, restarts, and release one-offs. Specs use Docker `source:target[:mode]` syntax, so named volumes and bind mounts stay familiar:

```yaml
services:
  web:
    volumes:
      - uploads:/app/uploads
      - /srv/myapp/config:/app/config:ro
```

Use `services.<service>.ports` or `accessories.<name>.ports` for simple same-port TCP publishing such as `3000:3000`. Use `publish` for full Docker publish specs, including loopback-only ports, remapped ports, and UDP:

```yaml
services:
  web:
    ports: [3000]
    publish:
      - 127.0.0.1:8080:80
      - 5353:5353/udp

accessories:
  postgres:
    publish:
      - 127.0.0.1:15432:5432
```

Use `services.<service>.resources` and `accessories.<name>.resources` to pass Docker CPU and memory constraints to service containers, restarts, release one-offs, and accessory containers. Supported fields are `cpus`, `memory`, `memory_reservation`, `memory_swap`, `cpu_shares`, `cpuset_cpus`, and `pids_limit`.

```yaml
services:
  worker:
    resources:
      cpus: "0.5"
      memory: 512m
      memory_reservation: 256m
      pids_limit: 256

accessories:
  postgres:
    resources:
      cpus: "2"
      memory: 2g
      memory_reservation: 1g
```

Use root `runtime`, `environments.<env>.runtime`, `services.<service>.runtime`, and `accessories.<name>.runtime` for Docker runtime security and kernel knobs. Root and environment runtime settings are applied as a shared baseline for every service and accessory; service or accessory settings can add list values, override scalar values, and override map keys. Boolean `true` values in the baseline enable that flag for every resolved container. Ship applies service runtime settings to long-running service containers, restarts, and release one-offs; accessory settings apply to accessory deploy and failover containers:

```yaml
runtime:
  read_only: true
  init: true
  security_opt:
    - no-new-privileges:true
  cap_drop: [NET_RAW]

environments:
  production:
    runtime:
      dns:
        - 1.1.1.1
      tmpfs:
        - /tmp:rw,size=64m

services:
  web:
    runtime:
      user: "1000:1000"
      workdir: /app
      stop_signal: SIGTERM
      stop_timeout_seconds: 30
      health_cmd: curl -fsS http://127.0.0.1:3000/up || exit 1
      health_interval: 10s
      health_timeout: 3s
      health_start_period: 20s
      health_retries: 3
      mounts:
        - type=bind,source=/srv/cache,target=/cache,readonly
      add_hosts:
        - host.docker.internal:host-gateway
      ulimits:
        - nofile=262144:262144

accessories:
  opensearch:
    runtime:
      entrypoint: /usr/local/bin/docker-entrypoint.sh
      no_healthcheck: true
      shm_size: 1g
      sysctls:
        vm.max_map_count: "262144"
      ulimits:
        - memlock=-1:-1
```

Supported runtime fields are `privileged`, `read_only`, `init`, `user`, `workdir`, `hostname`, `entrypoint`, `ipc`, `pid`, `cgroupns`, `stop_signal`, `stop_timeout_seconds`, `shm_size`, `gpus`, `no_healthcheck`, `health_cmd`, `health_interval`, `health_timeout`, `health_start_period`, `health_retries`, `cap_add`, `cap_drop`, `group_add`, `security_opt`, `sysctls`, `ulimits`, `mounts`, `add_hosts`, `dns`, `dns_search`, `dns_options`, `devices`, `device_cgroup_rules`, and `tmpfs`. Docker-native healthcheck fields make container state visible to Docker and host-level monitoring; Ship's deploy health gates still decide whether a release is promoted.

Use `services.<service>.labels` and `accessories.<name>.labels` to add Docker labels for observability, chargeback, backup agents, or external automation. Ship applies these labels to service containers, release one-offs, and accessory containers while reserving its own labels such as `project`, `environment`, `service`, `accessory`, `replica`, and `release`.

```yaml
services:
  web:
    labels:
      com.example.team: platform
      com.example.tier: frontend

accessories:
  postgres:
    labels:
      com.example.role: database
```

Ship creates and joins a per-environment Docker network on each host before starting service, release, accessory, or Caddy ingress containers. The default network name is `ship-<project>-<env>` with the Docker `bridge` driver, which gives co-located containers stable Docker DNS names without exposing accessory traffic publicly. Service containers get their service name as a network alias, and accessory containers get their accessory name as a network alias. Add `network_aliases` for compatibility names:

```yaml
docker:
  network:
    name: ship-production
    driver: bridge

services:
  web:
    network_aliases:
      - app

accessories:
  postgres:
    network_aliases:
      - database

environments:
  staging:
    docker:
      network:
        enabled: false
```

Configured network names cannot be Docker's built-in `bridge`, `host`, or `none` networks.

Long-running service, accessory, and Caddy ingress containers default to Docker `restart_policy: unless-stopped`, so they come back after host or Docker daemon restarts unless you intentionally stopped them. Override per service or accessory with `no`, `always`, `unless-stopped`, `on-failure`, or `on-failure:N`. Release commands remain one-off containers and do not inherit restart policies.

```yaml
services:
  web:
    restart_policy: unless-stopped
  worker:
    restart_policy: on-failure:5

accessories:
  postgres:
    restart_policy: always
```

Caddy ingress persists `/data` and `/config` in Docker named volumes by default so automatic HTTPS certificates, private keys, OCSP staples, and Caddy runtime state survive container replacement and host restarts. Ship derives deterministic names from the project and environment, or you can set explicit volume names:

```yaml
ingress:
  caddy:
    data_volume: ship-example-production-caddy-data
    config_volume: ship-example-production-caddy-config
```

`ship releases ENV` shows local release history newest-first, including current and rollback-target markers, release status, image digests, and config hashes. Current release pointers are tracked per environment, so staging deploys cannot disturb production rollback state. Use `--json` for automation or incident timelines. `ship releases diff ENV --from OLD --to NEW` compares two release records across config hash, service image digests, and secret digest names without exposing secret values; use `--json` for deployment review gates.

`ship promote SOURCE_ENV TARGET_ENV` creates a fresh target-environment release from the exact image digests recorded on the source environment's current release, or from `--release RELEASE_ID`. Promotion skips build and pre-build hooks, then uses the target environment's secrets, registry auth, release commands, rollout health checks, maintenance preservation, schedules, post-deploy hooks, and release-state sync. Use `--dry-run` to preview target ingress and secret readiness without touching hosts.

For automatic release-phase work such as database migrations, configure `services.<service>.release`. Ship runs the command once from the new image after secrets and registry auth are synced, but before the rollout starts; a failure marks the release failed and stops deployment.

```yaml
services:
  web:
    release:
      command: bin/rails db:migrate
      replica: 1
      timeout_seconds: 600
```

For local lifecycle gates, notifications, and smoke checks, configure `hooks.pre_deploy`, `hooks.pre_build`, `hooks.post_deploy`, and `hooks.deploy_failed` at the root or under an environment. Root hooks run before environment hooks. Hooks run from the `ship.yml` directory, record `deploy_hook` events, and receive `SHIP_PROJECT`, `SHIP_ENVIRONMENT`, `SHIP_HOOK`, `SHIP_RELEASE`, `SHIP_CONFIG`, `SHIP_CONFIG_DIR`, and, for `deploy_failed`, `SHIP_FAILURE`.

```yaml
hooks:
  pre_deploy:
    - ./scripts/check-freeze-window
  pre_build:
    - command: ./scripts/build-policy
      timeout_seconds: 30
      env:
        POLICY_MODE: strict
  post_deploy:
    - ./scripts/smoke-production
  deploy_failed:
    - ./scripts/notify-deploy-failed
```

For remote release notifications, configure `notifications.webhooks` at the root or under an environment. Root webhooks run before environment webhooks, notification failures are recorded as `notification` events without failing the deploy, and payloads include the project, environment, operation, status, release, message, image digests, and time. Use `url_env` for secret endpoints, custom `headers` for bearer tokens or routing keys, and `events` to filter delivery. Supported events are `deploy:succeeded`, `deploy:failed`, `promote:succeeded`, `promote:failed`, `rollback:succeeded`, and `rollback:failed`; use `*` or prefixes like `deploy:*` for broader subscriptions.

```yaml
notifications:
  webhooks:
    - url_env: SHIP_DEPLOY_WEBHOOK
      events: [deploy:*, promote:succeeded, rollback:failed]
      timeout_seconds: 5
      headers:
        X-Ship-Environment: production
```

`ship exec` runs the command inside the current release container over the SSH-framed agent. Use `--all` to run once per placed replica, `--replica N` for one replica, `--timeout SECONDS` for long-running migrations, and `--json` for structured output. `ship accessory exec ENV NAME -- COMMAND` runs inside the persisted accessory container after checking the saved placement and observed topology, which is useful for database shells, cache inspection, and one-off administrative commands.

`ship restart ENV [SERVICE]` recreates current-release service containers without building or promoting a new release. Use `--replica N` with a service to restart one placed replica; restart reuses deployed secret env-file paths and runs configured health checks before reporting success.

`ship lock ENV` prevents real deploys to that environment until `ship unlock ENV` clears the lock. Locked deploy attempts are recorded as blocked events. Use `ship deploy ENV --ignore-lock` only for an intentional operator override. Ship also takes a local operation lock around real deploy, rollback, and prune runs so two Ship processes cannot mutate the same environment at once.

Recurring service schedules are deploy-managed. Add `services.<service>.schedules.<name>` with a five-field cron expression and Ship syncs `/etc/cron.d/ship-<project>-<env>-*` on deploy so jobs point at the current release container:

```yaml
services:
  web:
    schedules:
      cleanup:
        cron: "17 * * * *"
        command: bin/rails cleanup
        replica: 1
        timeout_seconds: 300
```

Accessory backups can be scheduled the same way. Add `accessories.<name>.backup.schedule` and Ship syncs a cron file to the persisted accessory host on deploy. Scheduled backups require `backup.command` and a saved accessory placement, so run `ship accessory deploy ENV NAME` once before relying on the recurring job. Add `backup.export_command` to copy the completed artifact to off-host storage such as S3, R2, B2, restic, or rclone remotes; Ship runs it with `SHIP_BACKUP_ARTIFACT` set and records the first output line as the exported artifact URI.

```yaml
accessories:
  postgres:
    backup:
      command: pg_dumpall
      export_command: 'aws s3 cp "$SHIP_BACKUP_ARTIFACT" s3://ship-backups/postgres/$(basename "$SHIP_BACKUP_ARTIFACT") && printf "s3://ship-backups/postgres/%s\n" "$(basename "$SHIP_BACKUP_ARTIFACT")"'
      export_timeout_seconds: 900
      artifact_dir: /var/lib/ship/backups
      schedule:
        cron: "13 3 * * *"
        timeout_seconds: 600
```

Accessories are managed explicitly:

```bash
ship accessory deploy production postgres
ship accessory status production postgres
ship accessory backup production postgres
ship --dry-run accessory restore production postgres --artifact /var/lib/ship/backups/postgres-20260630T120000.000000000Z.backup
ship accessory restore production postgres --artifact /var/lib/ship/backups/postgres-20260630T120000.000000000Z.backup --yes
```

