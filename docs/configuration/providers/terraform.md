# Terraform / OpenTofu

Configure `environments.<env>.provider.terraform`.

## Example

```yaml
environments:
  production:
    provider:
      terraform:
        working_dir: infra
        workspace: production
        binary: tofu
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

The Terraform provider never creates or deletes servers; it runs `terraform output -json` (or `tofu output -json` when `binary: tofu` is set), reads the configured output, and treats those hosts as existing Ship targets. This makes any Terraform-supported cloud, hypervisor, bare-metal provider, or home lab inventory usable with Ship's agent install, secrets, deployment, logs, and status workflows. The output can be a pool map such as `{"web":["203.0.113.10"],"worker":[{"name":"worker-1","address":"203.0.113.20","port":2222}]}` or a list of objects with `name`, `address`, `pool`, optional `id`, optional `user`, and optional `port` fields.


## Credentials

Ship reads host inventory from Terraform output. You need the configured `terraform` or `tofu` binary and whatever credentials Terraform itself requires. Ship does not read cloud credentials directly.

[← All providers](README.md) · [Configuration overview](../README.md)
