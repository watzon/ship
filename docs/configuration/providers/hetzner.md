# Hetzner Cloud

Configure `environments.<env>.provider.hetzner`.

## Example

```yaml
environments:
  production:
    provider:
      hetzner:
        location: hel1
        server_type: cx23
        image: ubuntu-24.04
        ssh_allowed_cidrs: [203.0.113.0/24]
        ssh_keys:
          - ship-key
```

Hetzner provisioning manages a private network and firewall by default. Use `ssh_firewall: external` if you manage SSH access outside Ship, or set explicit `ssh_allowed_cidrs` for Ship-managed SSH rules.


## Credentials

Set `HCLOUD_TOKEN`.

[← All providers](README.md) · [Configuration overview](../README.md)
