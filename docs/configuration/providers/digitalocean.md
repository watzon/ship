# DigitalOcean

Configure `environments.<env>.provider.digitalocean`.

## Example

```yaml
environments:
  production:
    provider:
      digitalocean:
        region: nyc3
        size: s-2vcpu-4gb
        image: ubuntu-24-04-x64
        ssh_allowed_cidrs: [203.0.113.0/24]
        ssh_keys:
          - 3b:16:bf:e4:8b:00:8b:b8:59:8c:a9:d3:f0:19:45:fa
        monitoring: true
        vpc_uuid: 00000000-0000-4000-8000-000000000000
```

DigitalOcean provisioning manages Droplets with Ship ownership tags and creates a Cloud Firewall by default. Use `ssh_firewall: external` if SSH is handled elsewhere, or set explicit `ssh_allowed_cidrs` for Ship-managed SSH rules. Optional host features include `vpc_uuid`, `monitoring`, `backups`, and `ipv6`.


## Credentials

Set `DIGITALOCEAN_TOKEN`.

[← All providers](README.md) · [Configuration overview](../README.md)
