# Google Compute Engine

Configure `environments.<env>.provider.gcp`.

## Example

```yaml
environments:
  production:
    provider:
      gcp:
        project_id: my-gcp-project
        zone: us-central1-a
        machine_type: e2-medium
        image_project: ubuntu-os-cloud
        image: family/ubuntu-2404-lts
        network: default
        ssh_allowed_cidrs: [203.0.113.0/24]
        network_tags: [ship-web]
        metadata:
          enable-oslogin: "TRUE"
        boot_disk:
          size_gb: 40
          type: pd-balanced
        shielded_vm:
          vtpm: true
          integrity_monitoring: true
```

Google Compute Engine provisioning manages zonal VM instances with Ship ownership labels and creates targeted VPC firewall rules by default: HTTP/HTTPS from the internet and SSH only from `ssh_allowed_cidrs`. Use `ssh_firewall: external` if SSH is handled elsewhere, or `firewall.managed: false` to skip managed firewall creation. Optional host features include `image_project`, `network`, `subnetwork`, `network_tags`, `metadata`, `service_account`, `scopes`, `external_ip`, `boot_disk`, and Shielded VM settings.


## Credentials

Set `GOOGLE_APPLICATION_CREDENTIALS` or `GCP_ACCESS_TOKEN`.

[← All providers](README.md) · [Configuration overview](../README.md)
