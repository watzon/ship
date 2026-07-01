# Recovery


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
ship logs production web --failed --lines 200
ship logs production web --lines 200
ship inspect production
ship deploy production
```

Health failures stop promotion and keep the previous healthy release as current. Use `ship logs ENV SERVICE --failed` to inspect the newest failed release container when it is still present, or `--release RELEASE_ID` for a specific release from `ship releases`. Ingress reload happens only after rollout health passes. Slow-starting services can set `services.<service>.rolling.health_retries` and `health_interval_seconds` to retry the same health check before failing the rollout.

Rollback:

```bash
ship recover production
ship --dry-run rollback production --to RELEASE_ID --allow-data-rollback
ship rollback production --to RELEASE_ID --allow-data-rollback
```

Use `--allow-data-rollback` when configured accessories make data compatibility a manual decision. Real rollbacks also compare the target release's recorded secret digests with currently rendered secrets before touching hosts; if they differ, Ship blocks before mutation and records a blocked rollback event. Use `ship secrets diff ENV` to inspect drift, restore the intended secret values, or pass `--allow-secret-drift` when you intentionally want the old image to run with the current secrets.

Accessory restore:

```bash
ship accessory status production postgres
ship accessory backup production postgres
ship --dry-run accessory restore production postgres --artifact /var/lib/ship/backups/postgres.backup
ship accessory restore production postgres --artifact /var/lib/ship/backups/postgres.backup --yes
```

Restore validates the saved placement, observed topology, artifact path, and restore check before running the restore command. `ship accessory backup` records both the local artifact path and, when `backup.export_command` prints one, the exported artifact URI in accessory state for incident handoff and off-host retention audits.

