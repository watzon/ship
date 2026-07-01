# Pulumi

Configure `environments.<env>.provider.pulumi`.

## Example

```yaml
environments:
  production:
    provider:
      pulumi:
        working_dir: infra
        stack: production
        output: ship_hosts
        user: deploy
    hosts:
      pools:
        web:
          labels:
            tier: edge
        worker:
          user: worker
```

The Pulumi provider never creates or deletes servers; it runs `pulumi stack output --json`, reads the configured stack output, and treats those hosts as existing Ship targets. Set `binary` for an alternate Pulumi wrapper, `stack` to avoid depending on the selected local stack, `working_dir` to run from a Pulumi project directory, and `show_secrets: true` only when host addresses are intentionally exported as Pulumi secrets. The `ship_hosts` output accepts the same pool map or object-list shape as the Terraform provider, including per-host `user` and `port`.


## Credentials

Ship reads host inventory from Pulumi stack output. You need the configured `pulumi` binary and whatever login, backend, and cloud credentials Pulumi requires. Ship does not read cloud credentials directly.

[← All providers](README.md) · [Configuration overview](../README.md)
