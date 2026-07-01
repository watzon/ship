# Civo

Configure `environments.<env>.provider.civo`.

## Example

```yaml
environments:
  production:
    provider:
      civo:
        region: lon1
        size: g3.small
        image: ubuntu-noble
        network_id: 00000000-0000-0000-0000-000000000000
        ssh_key_id: 11111111-1111-1111-1111-111111111111
        ssh_allowed_cidrs: [203.0.113.0/24]
        public_ip: true
        initial_user: deploy
```

Civo provisioning manages instances with Ship ownership tags and creates a managed firewall by default. The managed firewall opens HTTP/HTTPS publicly and SSH only from `ssh_allowed_cidrs`. Use `firewall.id` with `firewall.managed: false` for an existing firewall, or `ssh_firewall: external` if SSH access is handled outside Ship. Optional host features include `ssh_key_id`, `initial_user`, `public_ip`, `reverse_dns`, `private_ipv4`, `allowed_ips`, and `network_bandwidth_limit`.


## Credentials

Set `CIVO_TOKEN`.

[← All providers](README.md) · [Configuration overview](../README.md)
