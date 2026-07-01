# Providers

Each environment picks one provider under `environments.<env>.provider`. Examples and credential requirements for each integration are on the linked pages.

## Cloud provisioning

Ship creates and deletes servers through the provider API.

- [Hetzner Cloud](hetzner.md)
- [Vultr](vultr.md)
- [DigitalOcean](digitalocean.md)
- [Linode](linode.md)
- [AWS EC2](aws.md)
- [Amazon Lightsail](lightsail.md)
- [Google Compute Engine](gcp.md)
- [Azure Virtual Machines](azure.md)
- [Scaleway Instances](scaleway.md)
- [OpenStack](openstack.md)
- [Civo](civo.md)
- [UpCloud](upcloud.md)
- [OVHcloud Public Cloud](ovhcloud.md)
- [Oracle Cloud Infrastructure](oci.md)
- [Exoscale](exoscale.md)
- [cloudscale.ch](cloudscale.md)
- [Latitude.sh](latitude.md)
- [Kamatera](kamatera.md)
- [Proxmox VE](proxmox.md)

## Infrastructure-as-code and inventory

Ship reads host lists from external tools. These providers never create or delete servers.

- [Terraform / OpenTofu](terraform.md)
- [Pulumi](pulumi.md)
- [Ansible inventory](ansible.md)

## Existing hosts

Point Ship at machines you already operate.

- [OpenSSH config](ssh-config.md)
- [Manual hosts](manual.md)

Pool-level `location`, `size`, and `image` overrides, cloud-init `user_data`, and custom host labels are documented in the [configuration overview](../README.md).

To implement a new provider in Ship itself, see [Adding a provider](../../providers.md).
