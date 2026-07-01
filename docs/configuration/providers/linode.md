# Linode

Configure `environments.<env>.provider.linode`.

## Example

```yaml
environments:
  production:
    provider:
      linode:
        region: us-east
        type: g6-standard-2
        image: linode/ubuntu24.04
        ssh_allowed_cidrs: [203.0.113.0/24]
        authorized_keys:
          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...
        private_ip: true
        backups: true
```

Linode provisioning manages Linodes with Ship ownership tags and creates an Akamai Cloud Firewall by default. Use `ssh_firewall: external` if SSH is managed elsewhere, or set explicit `ssh_allowed_cidrs` for Ship-managed SSH rules. Optional host features include `authorized_users`, `private_ip`, and `backups`.


## Credentials

Set `LINODE_TOKEN`.

[← All providers](README.md) · [Configuration overview](../README.md)
