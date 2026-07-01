# Proxmox VE

Configure `environments.<env>.provider.proxmox`.

## Example

```yaml
environments:
  production:
    provider:
      proxmox:
        api_url: https://pve.example.com:8006/api2/json
        node: pve1
        template_id: 9000
        storage: local-zfs
        full_clone: true
        pool: ship
        bridge: vmbr0
        vlan: 30
        memory_mb: 2048
        cores: 2
        ciuser: deploy
        ssh_keys:
          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...
        tags: [platform]
        agent: true
        onboot: true
    hosts:
      pools:
        web:
          count: 2
```

Proxmox provisioning clones QEMU VMs from a cloud-init-capable template, applies Ship ownership tags, configures cloud-init SSH user/keys and DHCP by default, optionally sets memory, cores, network bridge/VLAN, Proxmox pool, on-boot behavior, and starts VMs after creation. Ship waits for Proxmox asynchronous clone/config/start tasks, lists VMs by Ship tags, deletes VMs with owned disks during decommission, and uses the QEMU guest agent `network-get-interfaces` endpoint to discover a public SSH address when available. Use `insecure_skip_tls_verify: true` only for trusted Proxmox clusters with self-signed certificates.


## Credentials

Set `PROXMOX_API_TOKEN` or `PVE_API_TOKEN`.

[← All providers](README.md) · [Configuration overview](../README.md)
