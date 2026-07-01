# cloudscale.ch

Configure `environments.<env>.provider.cloudscale`.

## Example

```yaml
environments:
  production:
    provider:
      cloudscale:
        zone: rma1
        flavor: flex-4-2
        image: debian-13
        ssh_keys:
          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...
        volume_size_gb: 50
        user_data_handling: pass-through
        use_public_network: true
        use_ipv6: true
        server_group:
          managed: true
```

cloudscale.ch provisioning manages servers with native Ship tags, passes cloud-init `user_data` directly, supports public/private network flags or explicit interfaces, and can create a managed anti-affinity server group. Optional host features include `volume_size_gb`, `bulk_volume_size_gb`, extra `volumes`, `interfaces`, `use_private_network`, `use_ipv6`, `user_data_handling` (`pass-through` or `extend-cloud-config`), `server_groups`, `server_group.uuid`, and `anti_affinity_with`. cloudscale.ch does not expose a cloud firewall API; keep SSH restricted with OS firewalling, private networking, or external network controls.


## Credentials

Set `CLOUDSCALE_API_TOKEN`.

[← All providers](README.md) · [Configuration overview](../README.md)
