# Oracle Cloud Infrastructure

Configure `environments.<env>.provider.oci`.

## Example

```yaml
environments:
  production:
    provider:
      oci:
        region: us-ashburn-1
        compartment_id: ocid1.compartment.oc1..aaaa
        availability_domain: Uocm:US-ASHBURN-AD-1
        shape: VM.Standard.E4.Flex
        image_id: ocid1.image.oc1.iad.aaaa
        subnet_id: ocid1.subnet.oc1.iad.aaaa
        ssh_authorized_keys:
          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...
        ssh_allowed_cidrs: [203.0.113.0/24]
        network_security_group:
          vcn_id: ocid1.vcn.oc1.iad.aaaa
        shape_config:
          ocpus: 2
          memory_gb: 16
        boot_volume_size_gb: 80
        preserve_boot_volume: false
```

OCI provisioning manages Compute instances with Ship ownership free-form tags, hydrates the primary VNIC public IP for SSH, and creates a managed Network Security Group by default. The managed NSG allows outbound traffic, opens HTTP/HTTPS publicly, and opens SSH only from `ssh_allowed_cidrs`. Use `network_security_group.id` with `network_security_group.managed: false` for an existing NSG, add additional `nsg_ids`, set `ssh_firewall: external` when SSH is handled elsewhere, or set `assign_public_ip: false` for private instances. Optional host features include flexible `shape_config`, `boot_volume_size_gb`, `preserve_boot_volume`, custom `metadata`, `freeform_tags`, and `user_data`.


## Credentials

Set `OCI_TENANCY_OCID`, `OCI_USER_OCID`, `OCI_FINGERPRINT`, and `OCI_PRIVATE_KEY` or `OCI_PRIVATE_KEY_FILE`.

[← All providers](README.md) · [Configuration overview](../README.md)
