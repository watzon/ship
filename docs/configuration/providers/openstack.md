# OpenStack

Configure `environments.<env>.provider.openstack`.

## Example

```yaml
environments:
  production:
    provider:
      openstack:
        region: GRA11
        flavor: b2-7
        image: ubuntu-24.04
        network: public
        key_name: ship-production
        ssh_allowed_cidrs: [203.0.113.0/24]
        floating_ip:
          network_id: ext-net-id
        security_groups:
          - ship-web
        availability_zone: nova
        config_drive: true
        metadata:
          owner: platform
        tags:
          - ship
```

OpenStack provisioning works with public clouds and private clouds that expose Keystone, Nova, and Neutron, including providers whose public cloud is OpenStack-compatible. Ship authenticates with `OS_AUTH_TOKEN` plus `OS_COMPUTE_API_URL`, Keystone application credentials, or project-scoped username/password credentials; set `OS_NETWORK_API_URL` or `provider.openstack.network_url` when using direct-token Neutron features. The provider creates servers with Ship ownership metadata for reconciliation, creates a managed Neutron security group by default with SSH restricted to `ssh_allowed_cidrs` and HTTP/HTTPS public, and can allocate or attach Neutron floating IPs. Use `floating_ip.network_id` to allocate a new external address, or `floating_ip.address` to attach a reserved address; optional fields include `subnet_id`, `fixed_ip_address`, `description`, `dns_name`, `dns_domain`, `qos_policy_id`, and `distributed`. Ship also supports Nova `user_data`, `config_drive`, `key_name`, extra `security_groups`, `availability_zone`, and `scheduler_hints`. Use `security_group.managed: false` if you manage OpenStack security groups outside Ship.


## Credentials

Set OpenStack `OS_*` credentials (`OS_AUTH_URL`, application credentials, or username/password).

[← All providers](README.md) · [Configuration overview](../README.md)
