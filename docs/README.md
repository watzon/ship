# Ship documentation

Ship is a deployment tool for Docker applications on ordinary Linux servers. This folder has the guides; the [project README](../README.md) is just the overview.

## Guides

### [Quick start](quickstart.md)

Install the CLI, initialize a project, set up encrypted secrets, configure your provider, and run your first deploy. Start here if you are new to Ship.

### [Configuration](configuration/README.md)

`ship.yml` overview: environments, host pools, SSH settings, pool overrides, cloud-init, and labels.

### [Providers](configuration/providers/README.md)

Per-provider YAML examples, firewall defaults, optional fields, and credentials for each supported cloud or inventory source.

### [Deploy and operate](deploy-and-operate.md)

Day-2 operations: rollouts, scaling, logs, exec, maintenance mode, Caddy ingress, build options, hooks, webhooks, schedules, and accessory management.

### [Recovery](recovery.md)

What to do when provisioning, deploys, or health checks fail. Covers `ship recover`, rollback, and accessory restore.

### [Development](development.md)

How to run the test suite, optional live Hetzner gates, and local integration tests. Also describes the sample app fixture in `testdata/sample-app/`.

### [Adding a provider](providers.md)

Implementation contract for new cloud or inventory providers. Read this if you are extending Ship, not if you are deploying an app.

## Typical workflow

1. `ship init` and edit `ship.yml`
2. `ship doctor` to check local tools and credentials
3. `ship provision plan ENV` then `ship provision apply ENV`
4. `ship agent install ENV`
5. `ship secrets init ENV` and `ship secrets set ENV NAME`
6. `ship --dry-run deploy ENV`, then `ship deploy ENV`

Most commands accept `--json` for scripting and `--dry-run` to preview changes without mutating hosts.