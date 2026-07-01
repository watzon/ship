# Scaleway Instances

Configure `environments.<env>.provider.scaleway`.

## Example

```yaml
environments:
  production:
    provider:
      scaleway:
        project_id: 00000000-0000-0000-0000-000000000000
        zone: fr-par-1
        commercial_type: DEV1-S
        image: ubuntu_noble
        ssh_allowed_cidrs: [203.0.113.0/24]
        enable_ipv6: true
        dynamic_ip_required: true
        volumes:
          "0":
            size: 20000000000
            volume_type: sbs_volume
```

Scaleway provisioning manages Instances with Ship ownership tags and creates a stateful security group by default. The managed group drops inbound traffic by default, opens HTTP/HTTPS publicly, and opens SSH only from `ssh_allowed_cidrs`. Use `security_group.id` with `security_group.managed: false` for an existing security group, or `ssh_firewall: external` if SSH is handled outside Ship. Optional host features include `enable_ipv6`, `dynamic_ip_required`, `routed_ip_enabled`, `boot_after_create`, and custom `volumes`.


## Credentials

Set `SCW_SECRET_KEY`.

[← All providers](README.md) · [Configuration overview](../README.md)
