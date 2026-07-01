# Latitude.sh

Configure `environments.<env>.provider.latitude`.

## Example

```yaml
environments:
  production:
    provider:
      latitude:
        project: proj_lxWpD699qm6rk
        site: ASH
        plan: c2-small-x86
        operating_system: ubuntu_24_04_x64_lts
        ssh_allowed_cidrs: [203.0.113.0/24]
        ssh_keys:
          - ssh-key-id-or-public-key
        user_data: |
          #cloud-config
          package_update: true
        raid: raid-1
        billing: hourly
        disk_layout:
          - count: 2
            role: os
            raid_level: raid-1
            filesystem: ext4
            mount_point: /
```

Latitude.sh provisioning manages bare-metal servers through the JSON:API, filters by project, attaches native Latitude tags for Ship ownership after create, and keeps deterministic hostnames prefixed as `ship-<project>-<environment>-` as a fallback. Ship creates a Latitude.sh Firewall by default, configures HTTP/HTTPS public rules plus SSH rules from `ssh_allowed_cidrs`, and assigns the firewall to both new and existing Ship hosts during reconcile. Use `firewall.id` with `firewall.managed: false` for an existing firewall, or `ssh_firewall: external` if SSH access is handled outside Ship. Latitude's server create endpoint accepts a `user_data` ID, so Ship can either use `user_data_id` or create a project-scoped Latitude user-data object from `user_data`/`user_data_file` automatically. Optional host features include `raid`, `disk_layout`, `ipxe`, `billing`, and `delete_reason`.


## Credentials

Set `LATITUDE_API_TOKEN` or `LATITUDESH_BEARER`.

[← All providers](README.md) · [Configuration overview](../README.md)
