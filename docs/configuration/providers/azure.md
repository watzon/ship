# Azure Virtual Machines

Configure `environments.<env>.provider.azure`.

## Example

```yaml
environments:
  production:
    provider:
      azure:
        subscription_id: 00000000-0000-0000-0000-000000000000
        resource_group: rg-ship-production
        location: eastus
        vm_size: Standard_B2s
        image: Canonical:ubuntu-24_04-lts:server:latest
        admin_username: deploy
        ssh_public_key: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...
        virtual_network: ship-vnet
        subnet: default
        ssh_allowed_cidrs: [203.0.113.0/24]
        os_disk:
          size_gb: 40
          type: Premium_LRS
```

Azure provisioning manages VMs, per-host network interfaces, optional static public IPs, and a Network Security Group by default. The managed NSG opens HTTP/HTTPS publicly and SSH only from `ssh_allowed_cidrs`. Use `security_group.id` with `security_group.managed: false` for an existing NSG, `ssh_firewall: external` if SSH is handled outside Ship, or `public_ip: false` for private-only VMs. Optional host features include `subnet_id`, `os_disk`, `public_ip`, and `disable_password_login`.


## Credentials

Set `AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, and `AZURE_CLIENT_SECRET`, or `AZURE_ACCESS_TOKEN` for a short-lived ARM token.

[← All providers](README.md) · [Configuration overview](../README.md)
