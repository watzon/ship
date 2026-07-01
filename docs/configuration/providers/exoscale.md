# Exoscale

Configure `environments.<env>.provider.exoscale`.

## Example

```yaml
environments:
  production:
    provider:
      exoscale:
        zone: ch-gva-2
        instance_type: 21624abb-764e-4def-81d7-9fc54b5957fb
        template: 35c00b1d-4b12-4fcb-9d31-7a0b0a0f0000
        disk_size_gb: 50
        ssh_allowed_cidrs: [203.0.113.0/24]
        ssh_keys:
          - deploy
        public_ip_assignment: dual
        secure_boot: true
        tpm: true
```

Exoscale provisioning uses the zone-local API, marks instances with native Ship labels, base64-encodes cloud-init `user_data`, and creates a managed Security Group by default. The managed group opens HTTP/HTTPS publicly and opens SSH only from `ssh_allowed_cidrs`. Use `security_group.id` with `security_group.managed: false` for an existing group, add additional `security_groups`, or set `ssh_firewall: external` when SSH is handled elsewhere. Optional host features include `disk_size_gb`, `public_ip_assignment` (`inet4`, `dual`, or `none`), multiple `ssh_keys`, `anti_affinity_groups`, `deploy_target`, `auto_start`, `secure_boot`, `tpm`, and `application_consistent_snapshot`.


## Credentials

Set `EXOSCALE_API_KEY` and `EXOSCALE_API_SECRET`.

[← All providers](README.md) · [Configuration overview](../README.md)
