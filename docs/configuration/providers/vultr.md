# Vultr

Configure `environments.<env>.provider.vultr`.

## Example

```yaml
environments:
  production:
    provider:
      vultr:
        region: ewr
        plan: vc2-2c-4gb
        os_id: 2284
        ssh_allowed_cidrs: [203.0.113.0/24]
        ssh_key_ids:
          - your-vultr-ssh-key-id
        backups: true
        ipv6: true
        ddos_protection: true
        vpc_ids:
          - 00000000-0000-4000-8000-000000000000
        user_scheme: limited
```

Vultr requires `region`, `plan`, and exactly one of `os_id`, `image_id`, `snapshot_id`, or `app_id`. Vultr provisioning manages a firewall group by default with SSH restricted to `ssh_allowed_cidrs` and HTTP/HTTPS open for ingress. Use `firewall_group_id` for an existing group, `ssh_firewall: external` if SSH is handled outside Ship, or `firewall.enabled: false` to skip managed firewall creation. Optional host features include `hostname`, `firewall_group_id`, `backups`, `ipv6`, `ddos_protection`, `activation_email`, `enable_vpc`, `vpc_ids`, `vpc_only`, `disable_public_ipv4`, `reserved_ipv4`, `user_scheme`, `script_id`, and Marketplace `app_variables`.


## Credentials

Set `VULTR_API_KEY`.

[← All providers](README.md) · [Configuration overview](../README.md)
