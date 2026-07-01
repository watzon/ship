# Amazon Lightsail

Configure `environments.<env>.provider.lightsail`.

## Example

```yaml
environments:
  production:
    provider:
      lightsail:
        region: us-east-1
        availability_zone: us-east-1a
        bundle_id: nano_3_0
        blueprint_id: ubuntu_24_04
        key_pair_name: ship-production
        ip_address_type: dualstack
        ssh_allowed_cidrs: [203.0.113.0/24]
        add_ons:
          - type: AutoSnapshot
            snapshot_time_of_day: "06:00"
    hosts:
      pools:
        web:
          count: 2
        worker:
          count: 1
          location: us-east-1b
          size: small_3_0
          image: ubuntu_24_04
```

Lightsail provisioning creates AWS Lightsail instances through the JSON API with Ship ownership tags, waits until each new instance has a public address, and deletes by instance name during decommission. Ship manages each instance's public ports by default with HTTP/HTTPS open publicly and SSH restricted to `ssh_allowed_cidrs`; set `firewall.managed: false` to leave existing Lightsail port rules untouched, or `ssh_firewall: external` to omit Ship-managed SSH while still publishing HTTP/HTTPS. Optional host features include `key_pair_name`, `ip_address_type`, `add_ons` such as `AutoSnapshot`, `force_delete_add_ons`, and create-wait tuning through `wait_timeout_seconds` and `poll_interval_seconds`.


## Credentials

Set `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`.

[← All providers](README.md) · [Configuration overview](../README.md)
