# UpCloud

Configure `environments.<env>.provider.upcloud`.

## Example

```yaml
environments:
  production:
    provider:
      upcloud:
        zone: fi-hel1
        plan: 1xCPU-1GB
        template: 01000000-0000-4000-8000-000030240200
        ssh_keys:
          - ssh-ed25519 AAAA...
        ssh_allowed_cidrs: [203.0.113.0/24]
        metadata: true
        ipv6: true
```

UpCloud provisioning manages cloud servers with native Ship ownership labels, clones from an OS template, enables SSH-key login, and creates per-server firewall rules by default. The managed rules open HTTP/HTTPS publicly and SSH only from `ssh_allowed_cidrs`. Use `ssh_firewall: external` if SSH access is handled outside Ship, or `firewall.managed: false` to leave per-server rules alone. Optional host features include `storage_size_gb`, `storage_tier`, `username`, `metadata`, `ipv6`, `utility_network`, `private_network_id`, `simple_backup`, `server_group`, and `timezone`.


## Credentials

Set `UPCLOUD_USERNAME` and `UPCLOUD_PASSWORD`.

[← All providers](README.md) · [Configuration overview](../README.md)
