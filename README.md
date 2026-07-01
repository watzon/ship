# Ship

Ship is a Kamal-inspired deployment tool for running Docker applications on ordinary Linux servers, with horizontal scaling as a first-class concept and no Kubernetes control plane.

The v1 shape is intentionally small:

- one `ship` Go binary
- YAML config in `ship.yml`
- Hetzner Cloud, Vultr, DigitalOcean, Linode, AWS EC2, Amazon Lightsail, Google Compute Engine, Azure Virtual Machines, Scaleway Instances, OpenStack clouds, Civo instances, UpCloud servers, OVHcloud Public Cloud instances, Oracle Cloud Infrastructure Compute, Exoscale Compute, cloudscale.ch servers, Latitude.sh bare metal, Kamatera cloud servers, Proxmox VE VMs, Terraform/OpenTofu/Pulumi-managed hosts, Ansible inventory hosts, OpenSSH config inventory, and existing SSH host provisioning
- Docker Engine on hosts
- SSH-framed agent RPC, with no open agent port
- configurable SSH port, identity file, known_hosts file, jump host, and OpenSSH options
- deterministic service placement across host pools
- manual scaling through `ship scale`
- managed per-environment Docker networks for service, accessory, and ingress containers
- custom Docker labels for service and accessory containers
- Docker BuildKit external cache import/export, build secrets, SSH mounts, SBOMs, and provenance attestations
- Caddy ingress config generation
- canonical and legacy-domain redirects through managed Caddy ingress
- Caddy upstream health checks with passive failure quarantine
- single-primary accessories with backup/restore guardrails
- environment-backed secrets verification

## Quick Start

Build or install the CLI:

```bash
go install ./cmd/ship
ship --help
```

From a blank application repo:

```bash
ship init

# Edit ship.yml for your registry, provider settings, host pools,
# service image build/ref, health checks, ingress domains, accessories, and secrets.
ship doctor
ship provision plan production
ship --dry-run provision apply production
ship --dry-run agent install production
ship hosts production
ship plan production
ship --dry-run deploy production
```

Create an encrypted secret store for each environment. Commit `.ship/secrets/*.age` and `.ship/secrets/*.recipients`; keep identity files private.

```bash
age-keygen -o ~/.config/ship/identity.txt
age-keygen -y ~/.config/ship/identity.txt
ship secrets init staging --recipient age1...
ship secrets init production --recipient age1...
export SHIP_SECRETS_IDENTITY_FILE=~/.config/ship/identity.txt
DATABASE_URL=... ship secrets set production DATABASE_URL
SESSION_SECRET=... ship secrets set production SESSION_SECRET
ship secrets list production
ship secrets export production --redacted
```

`ship secrets set ENV NAME` reads the value from environment variable `NAME` unless `--value` is provided. `ship secrets export ENV` prints plaintext dotenv output for local inspection; prefer `--redacted` in logs or shared terminal output.

Ship uses your local Docker credentials for deploys. Log in with `docker login` or configure `DOCKER_AUTH_CONFIG`/Docker credential helpers on the machine running `ship`; during deploy, Ship copies only the needed registry auth entry to each target host before pulling service, accessory, or custom Caddy images. This lets fresh hosts pull from private registries such as GHCR, Docker Hub private repos, ECR-compatible registries, or self-hosted registries without manual `docker login` on every server.

When the dry-run output looks right and credentials are present:

```bash
export HCLOUD_TOKEN=...     # Hetzner
# or
export VULTR_API_KEY=...    # Vultr
# or
export DIGITALOCEAN_TOKEN=... # DigitalOcean
# or
export LINODE_TOKEN=...     # Linode
# or
export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... # AWS EC2 or Lightsail
# or
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/service-account.json # Google Compute Engine
# or
export GCP_ACCESS_TOKEN=... # Google Compute Engine short-lived token
# or
export AZURE_TENANT_ID=... AZURE_CLIENT_ID=... AZURE_CLIENT_SECRET=... # Azure
# or
export AZURE_ACCESS_TOKEN=... # Azure short-lived ARM token
# or
export SCW_SECRET_KEY=... # Scaleway
# or
export OS_AUTH_URL=... OS_APPLICATION_CREDENTIAL_ID=... OS_APPLICATION_CREDENTIAL_SECRET=... # OpenStack
# or
export CIVO_TOKEN=... # Civo
# or
export UPCLOUD_USERNAME=... UPCLOUD_PASSWORD=... # UpCloud
# or
export OVH_APPLICATION_KEY=... OVH_APPLICATION_SECRET=... OVH_CONSUMER_KEY=... # OVHcloud
export OVH_ENDPOINT=ovh-eu # optional; ovh-us and ovh-ca are also supported
# or
export OCI_TENANCY_OCID=... OCI_USER_OCID=... OCI_FINGERPRINT=... # OCI
export OCI_PRIVATE_KEY_FILE=~/.oci/oci_api_key.pem # or OCI_PRIVATE_KEY with PEM contents
# or
export EXOSCALE_API_KEY=... EXOSCALE_API_SECRET=... # Exoscale
# or
export CLOUDSCALE_API_TOKEN=... # cloudscale.ch
# or
export LATITUDE_API_TOKEN=... # Latitude.sh; LATITUDESH_BEARER is also supported
# or
export KAMATERA_CLIENT_ID=... KAMATERA_SECRET=... KAMATERA_SERVER_PASSWORD=... # Kamatera
# or
export PROXMOX_API_TOKEN='root@pam!ship=...' # Proxmox VE; PVE_API_TOKEN is also supported
# or use provider.terraform with terraform/tofu output; no cloud token is read by Ship.
# or use provider.pulumi with Pulumi stack output; no cloud token is read by Ship.
# or use provider.ansible with static or dynamic Ansible inventory; no cloud token is read by Ship.
# or use provider.ssh_config with ~/.ssh/config aliases; no cloud token required.
# or use provider.manual with existing SSH hosts; no cloud token required.
ship provision apply production --yes
ship agent install production
ship secrets verify production
ship secrets list production
ship deploy production
ship status production
ship version production --json
ship agent upgrade production --json
ship ps production
ship health production
ship maintenance enable production --message "Back soon"
ship maintenance disable production
ship logs production web --lines 100
```

`ship scale` previews deterministic placement for a new service count. To make the change durable in V1, update `ship.yml` and deploy.

```bash
ship scale production web=10
ship --dry-run deploy production
ship deploy production
```

## Sample App

`testdata/sample-app/` contains a tiny Dockerized Go app used by the acceptance tests:

- `Dockerfile`
- HTTP service command `/app/sample-app server` with `/up`
- worker command `/app/sample-app worker`
- healthcheck command `/app/sample-app healthcheck`
- optional `DATABASE_URL` accessory dependency signal

The normal test suite references this fixture without requiring Docker.

## Config

Ship reads `ship.yml` from the current directory by default. The generated starter config includes:

- staging and production in one file through `environments.<env>` overrides
- `environments.production.provider.hetzner`
- `environments.production.provider.vultr`, `environments.production.provider.digitalocean`, `environments.production.provider.linode`, `environments.production.provider.aws`, `environments.production.provider.lightsail`, `environments.production.provider.gcp`, `environments.production.provider.azure`, `environments.production.provider.scaleway`, `environments.production.provider.openstack`, `environments.production.provider.civo`, `environments.production.provider.upcloud`, `environments.production.provider.ovhcloud`, `environments.production.provider.oci`, `environments.production.provider.exoscale`, `environments.production.provider.cloudscale`, `environments.production.provider.latitude`, `environments.production.provider.kamatera`, `environments.production.provider.proxmox`, `environments.production.provider.terraform`, `environments.production.provider.pulumi`, `environments.production.provider.ansible`, `environments.production.provider.ssh_config`, and `environments.production.provider.manual` are also supported
- host pools for `web`, `worker`, and `ingress`
- stateless `web` and `worker` services
- a single-primary `postgres` accessory
- shared secrets under root `secrets` and scoped service/accessory `secrets`
- managed Caddy ingress container settings under root `ingress.caddy`

Useful commands while editing config:

```bash
ship config production
ship config production --json
ship hosts production
ship hosts production --json
ship provision plan production
ship provision plan production --json
ship plan production
ship plan production --json
ship plan production --observed
ship plan production --observed --json
ship secrets verify production
ship secrets render production --dry-run
ship secrets diff production
```

`ship config ENV` prints the resolved environment config after root-level defaults and environment overrides are merged. It shows secret names, not decrypted secret values. Both config and plan commands support `--json` for CI gates, custom dashboards, and preflight automation that wants structured data without parsing human text.

`ship plan ENV --observed` contacts every resolved host agent, compares the current release to observed Ship-managed containers, and shows the typed rollout actions Ship would use to converge from the real remote state. Use `--json` for pre-deploy gates that need drift summaries and planned pull/start/health/ingress/drain/stop actions.

`ship hosts ENV` shows the resolved host inventory and SSH contact targets Ship will use. Before provisioning it reports config-derived hosts; after `ship provision apply`, it uses saved provider host facts such as public addresses, SSH ports, jump hosts, and identity files.

Hetzner provider example:

```yaml
environments:
  production:
    provider:
      hetzner:
        location: hel1
        server_type: cx23
        image: ubuntu-24.04
        ssh_allowed_cidrs: [203.0.113.0/24]
        ssh_keys:
          - ship-key
```

Hetzner provisioning manages a private network and firewall by default. Use `ssh_firewall: external` if you manage SSH access outside Ship, or set explicit `ssh_allowed_cidrs` for Ship-managed SSH rules.

Vultr provider example:

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

DigitalOcean provider example:

```yaml
environments:
  production:
    provider:
      digitalocean:
        region: nyc3
        size: s-2vcpu-4gb
        image: ubuntu-24-04-x64
        ssh_allowed_cidrs: [203.0.113.0/24]
        ssh_keys:
          - 3b:16:bf:e4:8b:00:8b:b8:59:8c:a9:d3:f0:19:45:fa
        monitoring: true
        vpc_uuid: 00000000-0000-4000-8000-000000000000
```

DigitalOcean provisioning manages Droplets with Ship ownership tags and creates a Cloud Firewall by default. Use `ssh_firewall: external` if SSH is handled elsewhere, or set explicit `ssh_allowed_cidrs` for Ship-managed SSH rules. Optional host features include `vpc_uuid`, `monitoring`, `backups`, and `ipv6`.

Linode provider example:

```yaml
environments:
  production:
    provider:
      linode:
        region: us-east
        type: g6-standard-2
        image: linode/ubuntu24.04
        ssh_allowed_cidrs: [203.0.113.0/24]
        authorized_keys:
          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...
        private_ip: true
        backups: true
```

Linode provisioning manages Linodes with Ship ownership tags and creates an Akamai Cloud Firewall by default. Use `ssh_firewall: external` if SSH is managed elsewhere, or set explicit `ssh_allowed_cidrs` for Ship-managed SSH rules. Optional host features include `authorized_users`, `private_ip`, and `backups`.

AWS EC2 provider example:

```yaml
environments:
  production:
    provider:
      aws:
        region: us-east-1
        instance_type: t3.medium
        ami: ami-0123456789abcdef0
        key_name: ship-production
        vpc_id: vpc-0123456789abcdef0
        subnet_id: subnet-0123456789abcdef0
        associate_public_ip_address: true
        ssh_allowed_cidrs: [203.0.113.0/24]
        root_volume:
          size_gb: 40
          type: gp3
        monitoring: true
```

AWS provisioning manages EC2 instances with Ship ownership tags and creates an EC2 security group by default. Use `security_group.id` with `security_group.managed: false` if networking is managed elsewhere, or set explicit `ssh_allowed_cidrs` for Ship-managed SSH rules. Optional host features include `associate_public_ip_address`, `iam_instance_profile`, detailed `monitoring`, and root EBS volume size/type.

Amazon Lightsail provider example:

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

Google Compute Engine provider example:

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

Azure Virtual Machines provider example:

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

Scaleway Instances provider example:

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

OpenStack provider example:

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

Civo provider example:

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

UpCloud provider example:

```yaml
environments:
  production:
    provider:
      upcloud:
        zone: fi-hel1
        plan: 1xCPU-1GB
        template: 01000000-0000-4000-8000-000030240200
        ssh_keys:
          - ssh-ed25519 AAAA...
        ssh_allowed_cidrs: [203.0.113.0/24]
        metadata: true
        ipv6: true
```

UpCloud provisioning manages cloud servers with native Ship ownership labels, clones from an OS template, enables SSH-key login, and creates per-server firewall rules by default. The managed rules open HTTP/HTTPS publicly and SSH only from `ssh_allowed_cidrs`. Use `ssh_firewall: external` if SSH access is handled outside Ship, or `firewall.managed: false` to leave per-server rules alone. Optional host features include `storage_size_gb`, `storage_tier`, `username`, `metadata`, `ipv6`, `utility_network`, `private_network_id`, `simple_backup`, `server_group`, and `timezone`.

OVHcloud provider example:

```yaml
environments:
  production:
    provider:
      ovhcloud:
        service_name: 00000000000000000000000000000000
        endpoint: ovh-eu
        region: GRA11
        flavor_id: 11111111-1111-1111-1111-111111111111
        image_id: 22222222-2222-2222-2222-222222222222
        ssh_key_id: 33333333-3333-3333-3333-333333333333
        monthly_billing: false
```

OVHcloud provisioning manages Public Cloud instances through OVHcloud's signed REST API. Use `endpoint` or `OVH_ENDPOINT` for `ovh-eu`, `ovh-us`, `ovh-ca`, or a full API base URL. Because the native instance API does not round-trip arbitrary per-instance Ship labels, Ship gives OVHcloud resources deterministic display names prefixed with `ship-<project>-<environment>-` and maps them back to normal host names internally. Optional host features include `monthly_billing` and `user_data`; use the OpenStack provider instead when you need OVH-hosted Neutron security groups, floating IPs, or deeper network control.

Oracle Cloud Infrastructure provider example:

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

Exoscale provider example:

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

cloudscale.ch provider example:

```yaml
environments:
  production:
    provider:
      cloudscale:
        zone: rma1
        flavor: flex-4-2
        image: debian-13
        ssh_keys:
          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...
        volume_size_gb: 50
        user_data_handling: pass-through
        use_public_network: true
        use_ipv6: true
        server_group:
          managed: true
```

cloudscale.ch provisioning manages servers with native Ship tags, passes cloud-init `user_data` directly, supports public/private network flags or explicit interfaces, and can create a managed anti-affinity server group. Optional host features include `volume_size_gb`, `bulk_volume_size_gb`, extra `volumes`, `interfaces`, `use_private_network`, `use_ipv6`, `user_data_handling` (`pass-through` or `extend-cloud-config`), `server_groups`, `server_group.uuid`, and `anti_affinity_with`. cloudscale.ch does not expose a cloud firewall API; keep SSH restricted with OS firewalling, private networking, or external network controls.

Latitude.sh bare metal provider example:

```yaml
environments:
  production:
    provider:
      latitude:
        project: proj_lxWpD699qm6rk
        site: ASH
        plan: c2-small-x86
        operating_system: ubuntu_24_04_x64_lts
        ssh_allowed_cidrs: [203.0.113.0/24]
        ssh_keys:
          - ssh-key-id-or-public-key
        user_data: |
          #cloud-config
          package_update: true
        raid: raid-1
        billing: hourly
        disk_layout:
          - count: 2
            role: os
            raid_level: raid-1
            filesystem: ext4
            mount_point: /
```

Latitude.sh provisioning manages bare-metal servers through the JSON:API, filters by project, attaches native Latitude tags for Ship ownership after create, and keeps deterministic hostnames prefixed as `ship-<project>-<environment>-` as a fallback. Ship creates a Latitude.sh Firewall by default, configures HTTP/HTTPS public rules plus SSH rules from `ssh_allowed_cidrs`, and assigns the firewall to both new and existing Ship hosts during reconcile. Use `firewall.id` with `firewall.managed: false` for an existing firewall, or `ssh_firewall: external` if SSH access is handled outside Ship. Latitude's server create endpoint accepts a `user_data` ID, so Ship can either use `user_data_id` or create a project-scoped Latitude user-data object from `user_data`/`user_data_file` automatically. Optional host features include `raid`, `disk_layout`, `ipxe`, `billing`, and `delete_reason`.

Kamatera provider example:

```yaml
environments:
  production:
    provider:
      kamatera:
        datacenter: US-NY2
        cpu: 2B
        ram_mb: 4096
        image: US-NY2:ubuntu_server_24.04_64-bit
        disk_gb: 40
        password_env: KAMATERA_SERVER_PASSWORD
        billing: hourly
        traffic: t5000
        network: wan
        ssh_public_key: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...
        backup: true
        power: true
    hosts:
      pools:
        web:
          count: 2
        worker:
          count: 1
          location: IL
          size: 4B
          image: IL:ubuntu_server_24.04_64-bit
```

Kamatera provisioning uses the Console service API to create cloud servers, waits for the async server creation to appear in the account inventory, reads the public WAN IPv4 address from server information, and terminates by server ID during decommission. Kamatera requires a server password even when injecting `ssh_public_key`; use `password_env` so the password lives in an environment variable rather than in `ship.yml`. Because Kamatera's documented server list does not expose arbitrary labels or tags, Ship names servers with a deterministic `ship-<project>-<environment>-<host>` prefix and strips that prefix back to the normal Ship host name internally. Optional host features include `billing`, `traffic`, `network`, `network_ip`, `network_bits`, `managed`, `backup`, `power`, and create-wait tuning through `wait_timeout_seconds` and `poll_interval_seconds`.

Proxmox VE provider example:

```yaml
environments:
  production:
    provider:
      proxmox:
        api_url: https://pve.example.com:8006/api2/json
        node: pve1
        template_id: 9000
        storage: local-zfs
        full_clone: true
        pool: ship
        bridge: vmbr0
        vlan: 30
        memory_mb: 2048
        cores: 2
        ciuser: deploy
        ssh_keys:
          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...
        tags: [platform]
        agent: true
        onboot: true
    hosts:
      pools:
        web:
          count: 2
```

Proxmox provisioning clones QEMU VMs from a cloud-init-capable template, applies Ship ownership tags, configures cloud-init SSH user/keys and DHCP by default, optionally sets memory, cores, network bridge/VLAN, Proxmox pool, on-boot behavior, and starts VMs after creation. Ship waits for Proxmox asynchronous clone/config/start tasks, lists VMs by Ship tags, deletes VMs with owned disks during decommission, and uses the QEMU guest agent `network-get-interfaces` endpoint to discover a public SSH address when available. Use `insecure_skip_tls_verify: true` only for trusted Proxmox clusters with self-signed certificates.

Terraform provider example for infrastructure managed outside Ship:

```yaml
environments:
  production:
    provider:
      terraform:
        working_dir: infra
        workspace: production
        binary: tofu
        output: ship_hosts
        user: deploy
    hosts:
      pools:
        web:
          labels:
            tier: edge
        worker:
          user: worker
```

The Terraform provider never creates or deletes servers; it runs `terraform output -json` (or `tofu output -json` when `binary: tofu` is set), reads the configured output, and treats those hosts as existing Ship targets. This makes any Terraform-supported cloud, hypervisor, bare-metal provider, or home lab inventory usable with Ship's agent install, secrets, deployment, logs, and status workflows. The output can be a pool map such as `{"web":["203.0.113.10"],"worker":[{"name":"worker-1","address":"203.0.113.20","port":2222}]}` or a list of objects with `name`, `address`, `pool`, optional `id`, optional `user`, and optional `port` fields.

Pulumi provider example for infrastructure managed outside Ship:

```yaml
environments:
  production:
    provider:
      pulumi:
        working_dir: infra
        stack: production
        output: ship_hosts
        user: deploy
    hosts:
      pools:
        web:
          labels:
            tier: edge
        worker:
          user: worker
```

The Pulumi provider never creates or deletes servers; it runs `pulumi stack output --json`, reads the configured stack output, and treats those hosts as existing Ship targets. Set `binary` for an alternate Pulumi wrapper, `stack` to avoid depending on the selected local stack, `working_dir` to run from a Pulumi project directory, and `show_secrets: true` only when host addresses are intentionally exported as Pulumi secrets. The `ship_hosts` output accepts the same pool map or object-list shape as the Terraform provider, including per-host `user` and `port`.

Ansible provider example for inventory managed outside Ship:

```yaml
environments:
  production:
    provider:
      ansible:
        inventory_file: ops/inventory.yml
        user: deploy
    hosts:
      pools:
        web:
          labels:
            tier: edge
        worker:
          user: worker
```

The Ansible provider never creates or deletes servers; it reads a static YAML/JSON inventory or runs a dynamic inventory command and treats those hosts as existing Ship targets. Pool names map to Ansible inventory groups; when an environment has a single pool and that group is absent, Ship falls back to the Ansible `all` group. Ship honors `ansible_host`/`ansible_ssh_host` for the SSH address, `ansible_user`/`ansible_ssh_user` for the login user, and `ansible_port`/`ansible_ssh_port` for the SSH port. Use `command: ["ansible-inventory", "-i", "ops/inventory.yml", "--list"]` to consume Ansible's normalized dynamic inventory JSON.

OpenSSH config provider example for existing SSH aliases:

```yaml
environments:
  production:
    provider:
      ssh_config:
        path: ~/.ssh/config
        user: deploy
    hosts:
      pools:
        web:
          hosts:
            - web-prod-1
            - web-prod-2
        worker:
          hosts:
            - worker-prod-1
```

The OpenSSH config provider never creates or deletes servers. Pool hosts are SSH aliases from `~/.ssh/config` or a configured `path`; Ship reads matching `Host` stanzas, wildcard defaults, negated patterns, and `Include` files, then records `HostName`, `User`, `Port`, `IdentityFile`, `UserKnownHostsFile`, `ProxyJump`, and common extra options such as `ForwardAgent`, `IdentitiesOnly`, `ProxyCommand`, and `ServerAliveInterval` as local host facts. This makes existing bastion-heavy fleets deployable without duplicating SSH connection metadata in `ship.yml`.

Manual provider example for existing SSH hosts:

```yaml
environments:
  production:
    provider:
      manual: {}
    hosts:
      pools:
        web:
          user: deploy
          hosts:
            - web-1.example.com
            - web-2.example.com
        worker:
          user: deploy
          hosts:
            - worker-1.example.com
```

The manual provider never creates or deletes servers. `ship provision apply production --yes` records the configured hosts as local host facts, waits for SSH, installs Docker prerequisites, uploads the Ship binary, and enables the Ship agent. This makes existing bare metal, colocated servers, home lab machines, and unsupported clouds usable without waiting for a provider integration.

SSH connection settings:

```yaml
ssh:
  identity_file: ~/.ssh/ship
  known_hosts_file: .ship/known_hosts
  options:
    ControlMaster: auto
    ControlPersist: 60s

environments:
  production:
    ssh:
      jump_host: deploy@bastion.example.com
    hosts:
      pools:
        web:
          count: 2
        worker:
          count: 2
          ssh:
            port: 2222
            identity_file: ~/.ssh/ship-worker
            options:
              ServerAliveInterval: "30"
```

Root `ssh` settings apply to every environment; environment settings override root values; pool settings override both. Provider-discovered host facts, such as Ansible `ansible_port`, Terraform/Pulumi host object `port`, or OpenSSH config `Port`/`IdentityFile`/`ProxyJump`, override or extend the configured settings for that host. Supported fields are `port`, `identity_file`, `known_hosts_file`, `jump_host`, and `options`. `jump_host` maps to OpenSSH `-J`, and `options` are passed as sorted `-o Key=Value` flags. These settings apply to provisioning bootstrap, `ship agent install`, deploys, status, logs, rollback, recovery, and accessory operations.

Pool-level host shape overrides:

```yaml
environments:
  production:
    hosts:
      pools:
        web:
          count: 3
          size: cpx31
        worker:
          count: 2
          location: fsn1
          size: cpx41
          image: ubuntu-24.04
```

`hosts.pools.<pool>.location`, `size`, and `image` override the environment provider defaults for that pool. Provider mappings are: Hetzner `location`/`server_type`/`image`; Vultr `region`/`plan`/source such as `os_id:2284`, `image_id:<id>`, `snapshot_id:<id>`, or `app_id:<id>`; DigitalOcean `region`/`size`/`image`; Linode `region`/`type`/`image`; AWS `instance_type`/`ami` through `size`/`image`; Lightsail `availability_zone`/`bundle_id`/`blueprint_id` through `location`/`size`/`image`; GCP `machine_type`/image through `size`/`image`; Azure `vm_size`/image through `size`/`image`; Scaleway `commercial_type`/image through `size`/`image`; OpenStack `flavor`/image through `size`/`image`; Civo `size`/template through `size`/`image`; UpCloud `plan`/template through `size`/`image`; OVHcloud `flavor_id`/`image_id` through `size`/`image`; OCI `shape`/`image_id` through `size`/`image`; Exoscale `instance_type`/`template` through `size`/`image`; cloudscale.ch `flavor`/`image` through `size`/`image`; Latitude.sh `plan`/`operating_system` through `size`/`image`; Kamatera `datacenter`/`cpu`/disk image through `location`/`size`/`image`; Proxmox `node`/template VM through `location`/`image` in plans. Terraform, Pulumi, Ansible, OpenSSH config, and manual providers treat pools as inventory and placement metadata for existing hosts. AWS pool `location` overrides are intentionally rejected; use separate environments for multi-region EC2 so security groups, subnets, and decommissioning stay explicit. Lightsail pool `location` overrides must stay in the configured region. GCP pool `location` overrides are also rejected; use separate environments for multi-zone Compute Engine. Azure pool `location` overrides are rejected; use separate environments for multi-region Azure. Scaleway pool `location` overrides are rejected; use separate environments for multi-zone Scaleway. OpenStack pool `location` overrides are rejected; use separate environments for multi-region OpenStack. Civo pool `location` overrides are rejected; use separate environments for multi-region Civo. UpCloud pool `location` overrides are rejected; use separate environments for multi-zone UpCloud. OVHcloud pool `location` overrides are rejected; use separate environments for multi-region OVHcloud. OCI pool `location` overrides are rejected; use separate environments for multi-AD OCI. Exoscale pool `location` overrides are rejected; use separate environments for multi-zone Exoscale. cloudscale.ch pool `location` overrides are rejected; use separate environments for multi-zone cloudscale.ch. Latitude.sh pool `location` overrides are rejected; use separate environments for multi-site bare metal. Proxmox pool `location` overrides are rejected; use separate environments for multi-node Proxmox placement.

Cloud-init and user data:

```yaml
environments:
  production:
    provider:
      hetzner:
        location: hel1
        server_type: cx23
        image: ubuntu-24.04
        user_data_file: ops/cloud-init/base.yml
        ssh_allowed_cidrs: [203.0.113.0/24]
    hosts:
      pools:
        worker:
          count: 2
          user_data: |
            #cloud-config
            packages:
              - imagemagick
```

Cloud providers that expose boot metadata support `user_data` and `user_data_file` on the provider block, and pools can override it with `hosts.pools.<pool>.user_data` or `user_data_file`. Files are read relative to `ship.yml`. Ship passes plaintext to Hetzner, DigitalOcean, Lightsail launch scripts, GCP metadata, Scaleway's `cloud-init` user-data key, Civo's instance script field, UpCloud `user_data`, OVHcloud `userData`, and cloudscale.ch `user_data`; automatically base64-encodes for Vultr, Linode metadata, AWS EC2, Azure custom data, OpenStack Nova user data, OCI metadata `user_data`, and Exoscale `user-data`; and creates Latitude.sh user-data objects from plaintext so servers can reference the resulting Latitude `user_data` ID. Kamatera's documented server create API does not expose cloud-init/user-data, so Ship rejects Kamatera pool `user_data` rather than silently ignoring it.

Custom host labels and tags:

```yaml
environments:
  production:
    hosts:
      labels:
        owner: platform
        cost-center: shared-infra
      pools:
        worker:
          count: 2
          labels:
            cost-center: batch
            workload: jobs
```

`hosts.labels` are applied to all provider-created hosts, and `hosts.pools.<pool>.labels` override or extend them for one pool. Ship preserves its own ownership labels (`managed-by`, `project`, `environment`, and `pool`) for reconciliation and rejects configs that try to override them. Cloud providers map these to native labels or tags: Hetzner labels, Vultr tags, DigitalOcean tags, Linode tags, AWS EC2 tags, Lightsail tags, GCP labels, Azure tags, Scaleway tags, Civo tags, UpCloud labels, OpenStack server metadata, OCI free-form tags, Exoscale labels, cloudscale.ch tags, Latitude.sh tags, and Proxmox tags. Terraform, Pulumi, Ansible, OpenSSH config, and manual providers apply labels to Ship's local host facts only because those providers do not mutate remote infrastructure. OVHcloud Public Cloud instances and Kamatera cloud servers are tracked by Ship-prefixed display name because arbitrary per-instance tags are not exposed by their documented native server APIs.

Set `HCLOUD_TOKEN` for Hetzner environments, `VULTR_API_KEY` for Vultr environments, `DIGITALOCEAN_TOKEN` for DigitalOcean environments, `LINODE_TOKEN` for Linode environments, `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` for AWS EC2 and Lightsail environments, `GOOGLE_APPLICATION_CREDENTIALS` or `GCP_ACCESS_TOKEN` for GCP environments, `AZURE_TENANT_ID`/`AZURE_CLIENT_ID`/`AZURE_CLIENT_SECRET` or `AZURE_ACCESS_TOKEN` for Azure environments, `SCW_SECRET_KEY` for Scaleway environments, OpenStack `OS_*` credentials for OpenStack environments, `CIVO_TOKEN` for Civo environments, `UPCLOUD_USERNAME`/`UPCLOUD_PASSWORD` for UpCloud environments, `OVH_APPLICATION_KEY`/`OVH_APPLICATION_SECRET`/`OVH_CONSUMER_KEY` for OVHcloud environments, `OCI_TENANCY_OCID`/`OCI_USER_OCID`/`OCI_FINGERPRINT` plus `OCI_PRIVATE_KEY` or `OCI_PRIVATE_KEY_FILE` for OCI environments, `EXOSCALE_API_KEY`/`EXOSCALE_API_SECRET` for Exoscale environments, `CLOUDSCALE_API_TOKEN` for cloudscale.ch environments, `LATITUDE_API_TOKEN` or `LATITUDESH_BEARER` for Latitude.sh environments, `KAMATERA_CLIENT_ID` plus `KAMATERA_SECRET` or `KAMATERA_API_SECRET` plus the configured server password env var for Kamatera environments, and `PROXMOX_API_TOKEN` or `PVE_API_TOKEN` for Proxmox environments. Terraform environments require the configured `terraform`/`tofu` binary and whatever credentials Terraform itself needs; Pulumi environments require the configured `pulumi` binary and whatever login/backend/cloud credentials Pulumi itself needs; Ansible inventory-file environments require no cloud credentials, while Ansible command environments require the configured command binary and whatever credentials the inventory plugin or script needs; OpenSSH config environments require no cloud credentials. Ship does not read IaC or inventory cloud credentials directly. `OVH_ENDPOINT` is optional and defaults to `ovh-eu`. `AWS_SESSION_TOKEN` is supported for temporary AWS credentials.

See [docs/providers.md](docs/providers.md) for the provider implementation contract.

## Deploy And Operate

Provisioning reconciles provider servers by Ship ownership metadata, writes local host facts, waits for SSH, installs Docker prerequisites, uploads the local `ship` binary, and enables the Ship agent service. It does not delete extra servers during apply; cleanup is an explicit decommission command.

```bash
ship provision apply production --yes
ship --dry-run agent install production
ship agent install production
ship version production
ship --dry-run agent upgrade production
ship agent upgrade production
```

To remove Ship-managed provider servers for an environment:

```bash
ship --dry-run provision decommission production
ship provision decommission production --yes
```

Deploy builds or resolves service images, writes release state, rolls services through the SSH-framed agent, runs health checks, and promotes only healthy releases.

```bash
ship --dry-run deploy production
ship deploy production
ship promote staging production
ship status production
ship hosts production
ship version production
ship agent upgrade production --json
ship ps production
ship health production
ship maintenance status production
ship inspect production
ship support production --json
ship events production
ship releases production
ship restart production web --replica 1
ship logs production web --replica 1 --lines 200
ship accessory logs production postgres --lines 200
ship exec production web --replica 1 -- bin/rails db:migrate
ship accessory exec production postgres -- psql -c 'select 1'
ship prune production
ship lock production --message "database maintenance"
ship unlock production
```

Tune service rollout behavior with `services.<service>.rolling`. Ship supports surge and unavailable limits, health retry pacing, drain waits, and canary gates. `canary_pause_seconds` pauses the rollout after the first new replica passes health checks; set `canary_replicas` to require a larger first batch before the pause. Ingress promotion still happens only after the full rollout succeeds.

```yaml
services:
  web:
    rolling:
      max_surge: 1
      max_unavailable: 0
      canary_replicas: 1
      canary_pause_seconds: 60
      health_retries: 5
      health_interval_seconds: 3
      drain_timeout_seconds: 10
```

Each release stores a hash of the resolved `ship.yml` environment config. `ship status` and `ship inspect --json` report config drift when the current config differs from the config that produced the deployed release, so operators can spot undeployed changes before chasing container drift.

`ship ps ENV` lists observed Ship-managed service, ingress, and accessory containers for the desired placement. Use `--all` to include extra old or wrong-release containers, `--service NAME` to narrow the view, and `--json` for automation.

`ship hosts ENV` lists logical hosts, pools, SSH users, contact addresses, and SSH overrides from the same resolved inventory deploys use. Use `--json` for inventory exports or automation that needs to know where Ship will connect.

`ship version` prints the local Ship binary version and supported agent protocol range. `ship version ENV` also asks every resolved host agent for its version, protocol, Docker readiness, state directory, and supported RPC methods; use `--json` for fleet audits and upgrade checks.

`ship agent upgrade ENV` uploads the currently running local Ship binary to every resolved host through the SSH-framed agent RPC, verifies the SHA-256 checksum, and records upgrade events per host. Use `--dry-run` to preview the target path and digest, and `--json` for upgrade reports.

`ship health ENV [SERVICE]` runs the configured command or HTTP health checks against the current release without deploying. Use `--replica N` to check one placed service replica and `--json` for monitors or incident tooling.

`ship support ENV` collects a redacted incident bundle with resolved config, host inventory, doctor checks, observed status, recent releases, accessories, and recent events. Use `--json` for the complete artifact, and tune noisy sections with `--events-limit` and `--releases-limit`.

`ship maintenance enable ENV` replaces ingress routing with a generated Caddy 503 page for all configured ingress domains; `ship maintenance disable ENV` restores normal ingress from the current config. Deploys preserve an enabled maintenance page, so traffic returns only through an explicit disable. Use `--message` to customize the body and `ship maintenance status ENV --json` for automation.

`ship logs ENV SERVICE` reads logs from the current release by default. Use `--release RELEASE_ID` to inspect a specific release container, or `--failed` to fetch logs for the newest failed release that included the service. `ship accessory logs ENV NAME` reads logs from the persisted accessory container after checking saved placement and observed topology; use `--lines`, `--follow`, and `--json` the same way as service logs.

Managed Caddy ingress publishes TCP 80, TCP 443, and UDP 443. UDP 443 enables HTTP/3/QUIC for clients that support it, while browsers fall back to HTTP/2 or HTTP/1.1 when QUIC is unavailable. Ship-managed firewalls and security groups open UDP 443 alongside HTTP/HTTPS; when you use an external firewall, open UDP 443 yourself for HTTP/3.

Managed Caddy ingress can also serve redirect-only domains. This is useful for canonical `www` to apex redirects, renamed products, or legacy domains that should keep their paths and query strings:

```yaml
services:
  web:
    ingress:
      domains: [example.com]
      redirects:
        - from: [www.example.com, old.example.com]
          to: https://example.com
          code: 308
```

Redirects preserve the original URI by default, so `/pricing?ref=old` becomes `https://example.com/pricing?ref=old`. Set `preserve_uri: false` for landing-page redirects that should always go to the exact `to` URL.

Managed Caddy ingress also protects live traffic after deploy. Ship enables passive upstream failure quarantine by default, and when a service defines `health.http`, the generated reverse proxy performs active background checks with the same path so unhealthy replicas are pulled out of load balancing until they recover. Tune or disable this per service with `services.<service>.ingress.health`:

```yaml
services:
  web:
    health:
      http: /up
    ingress:
      domains: [example.com]
      health:
        interval_seconds: 10
        timeout_seconds: 3
        fails: 2
        passes: 1
        try_duration_seconds: 5
        passive_fail_duration_seconds: 30
        passive_max_fails: 1
        unhealthy_status: [5xx]
```

Use `ingress.health.path` to check a different proxy-only endpoint, or `ingress.health.enabled: false` to omit Ship-managed proxy health directives for that service.

Set root `logging` defaults, or `services.<service>.logging` overrides, to pass Docker logging driver settings to service containers and release one-offs. This is useful for disk-safe rotation with `json-file` or for routing logs to drivers such as `local`, `journald`, `fluentd`, or `awslogs`.

```yaml
logging:
  driver: json-file
  options:
    max-size: 10m
    max-file: "3"

services:
  web:
    logging:
      options:
        tag: "{{.Name}}"
```

Use `services.<service>.image.cache_from` and `cache_to` to opt into Docker BuildKit external caches for faster repeat deploys and CI builds. Add `image.secrets` and `image.ssh` when builds need private package registries or Git dependencies without baking credentials into image layers. Set `image.sbom` or `image.provenance` to publish Buildx attestations alongside the image for supply-chain audits.

When cache, secret, or SSH fields are set, Ship uses `docker buildx build --load` and then pushes the image through the normal deploy flow. When SBOM or provenance is enabled, Ship uses `docker buildx build --push` so the registry receives the attestation metadata, then skips the separate `docker push`:

```yaml
services:
  web:
    image:
      build: .
      tags:
        - latest
        - production
      builder: ship-cloud
      platforms:
        - linux/amd64
        - linux/arm64
      pull: true
      no_cache_filter:
        - install
      cache_from:
        - type=registry,ref=ghcr.io/acme/app:build-cache
      cache_to:
        - type=registry,ref=ghcr.io/acme/app:build-cache,mode=max
      secrets:
        - id=npm_token,env=NPM_TOKEN
        - id=bundle,src=.bundle/credentials
      ssh:
        - default
      sbom: true
      provenance: mode=max
```

Use `image.tags` to publish stable service-prefixed aliases alongside the immutable release tag. For service `web`, `tags: [latest, production]` publishes `repo:web-latest` and `repo:web-production`, while deploys still resolve and roll out the release-specific digest. Use `image.platform` for a single target platform. Use `image.platforms` for a multi-platform image; Ship publishes those builds directly with Buildx so the registry receives a multi-architecture manifest before deploy digest resolution. Set `image.builder` to an existing Buildx builder name, such as a Docker Build Cloud builder or an SSH-backed builder created with `docker buildx create`, when builds should run somewhere other than the default local builder. Set `image.pull: true` to refresh base images, `image.no_cache: true` for a fully fresh rebuild, or `image.no_cache_filter` to bypass cache only for named Dockerfile stages.

Cache entries use Docker's BuildKit cache backend syntax, so registry, GitHub Actions, local, and other BuildKit-supported cache exporters can be used where your Docker builder supports them. Build secrets are available in Dockerfiles with `RUN --mount=type=secret,id=npm_token ...`, and SSH mounts with `RUN --mount=type=ssh ...`. `sbom` and `provenance` accept either booleans or Docker Buildx option strings such as `generator=...` or `mode=max`.

For apps without a Dockerfile, set `services.<service>.image.buildpack` to build with Cloud Native Buildpacks through the local `pack` CLI. Ship still tags the image with the immutable release tag, supports `image.tags` aliases through `pack --tag`, resolves the final registry digest, and deploys that digest like any Docker-built image. Set `publish: true` to have `pack build --publish` write directly to the registry and skip Ship's separate `docker push`.

```yaml
services:
  web:
    image:
      build: .
      tags: [latest]
      buildpack:
        builder: paketobuildpacks/builder-jammy-base
        buildpacks:
          - paketo-buildpacks/nodejs
        env:
          BP_NODE_RUN_SCRIPTS: build
        descriptor: project.production.toml
        pull_policy: if-not-present
        publish: true
```

Buildpack mode is intentionally separate from Dockerfile/Buildx mode. Use `image.buildpack.builder` for the Buildpacks builder image; `image.builder` remains the Docker Buildx builder selector.

Use `services.<service>.volumes` to mount Docker volumes or existing host paths into service containers, restarts, and release one-offs. Specs use Docker `source:target[:mode]` syntax, so named volumes and bind mounts stay familiar:

```yaml
services:
  web:
    volumes:
      - uploads:/app/uploads
      - /srv/myapp/config:/app/config:ro
```

Use `services.<service>.ports` or `accessories.<name>.ports` for simple same-port TCP publishing such as `3000:3000`. Use `publish` for full Docker publish specs, including loopback-only ports, remapped ports, and UDP:

```yaml
services:
  web:
    ports: [3000]
    publish:
      - 127.0.0.1:8080:80
      - 5353:5353/udp

accessories:
  postgres:
    publish:
      - 127.0.0.1:15432:5432
```

Use `services.<service>.resources` and `accessories.<name>.resources` to pass Docker CPU and memory constraints to service containers, restarts, release one-offs, and accessory containers. Supported fields are `cpus`, `memory`, `memory_reservation`, `memory_swap`, `cpu_shares`, `cpuset_cpus`, and `pids_limit`.

```yaml
services:
  worker:
    resources:
      cpus: "0.5"
      memory: 512m
      memory_reservation: 256m
      pids_limit: 256

accessories:
  postgres:
    resources:
      cpus: "2"
      memory: 2g
      memory_reservation: 1g
```

Use root `runtime`, `environments.<env>.runtime`, `services.<service>.runtime`, and `accessories.<name>.runtime` for Docker runtime security and kernel knobs. Root and environment runtime settings are applied as a shared baseline for every service and accessory; service or accessory settings can add list values, override scalar values, and override map keys. Boolean `true` values in the baseline enable that flag for every resolved container. Ship applies service runtime settings to long-running service containers, restarts, and release one-offs; accessory settings apply to accessory deploy and failover containers:

```yaml
runtime:
  read_only: true
  init: true
  security_opt:
    - no-new-privileges:true
  cap_drop: [NET_RAW]

environments:
  production:
    runtime:
      dns:
        - 1.1.1.1
      tmpfs:
        - /tmp:rw,size=64m

services:
  web:
    runtime:
      user: "1000:1000"
      workdir: /app
      stop_signal: SIGTERM
      stop_timeout_seconds: 30
      health_cmd: curl -fsS http://127.0.0.1:3000/up || exit 1
      health_interval: 10s
      health_timeout: 3s
      health_start_period: 20s
      health_retries: 3
      mounts:
        - type=bind,source=/srv/cache,target=/cache,readonly
      add_hosts:
        - host.docker.internal:host-gateway
      ulimits:
        - nofile=262144:262144

accessories:
  opensearch:
    runtime:
      entrypoint: /usr/local/bin/docker-entrypoint.sh
      no_healthcheck: true
      shm_size: 1g
      sysctls:
        vm.max_map_count: "262144"
      ulimits:
        - memlock=-1:-1
```

Supported runtime fields are `privileged`, `read_only`, `init`, `user`, `workdir`, `hostname`, `entrypoint`, `ipc`, `pid`, `cgroupns`, `stop_signal`, `stop_timeout_seconds`, `shm_size`, `gpus`, `no_healthcheck`, `health_cmd`, `health_interval`, `health_timeout`, `health_start_period`, `health_retries`, `cap_add`, `cap_drop`, `group_add`, `security_opt`, `sysctls`, `ulimits`, `mounts`, `add_hosts`, `dns`, `dns_search`, `dns_options`, `devices`, `device_cgroup_rules`, and `tmpfs`. Docker-native healthcheck fields make container state visible to Docker and host-level monitoring; Ship's deploy health gates still decide whether a release is promoted.

Use `services.<service>.labels` and `accessories.<name>.labels` to add Docker labels for observability, chargeback, backup agents, or external automation. Ship applies these labels to service containers, release one-offs, and accessory containers while reserving its own labels such as `project`, `environment`, `service`, `accessory`, `replica`, and `release`.

```yaml
services:
  web:
    labels:
      com.example.team: platform
      com.example.tier: frontend

accessories:
  postgres:
    labels:
      com.example.role: database
```

Ship creates and joins a per-environment Docker network on each host before starting service, release, accessory, or Caddy ingress containers. The default network name is `ship-<project>-<env>` with the Docker `bridge` driver, which gives co-located containers stable Docker DNS names without exposing accessory traffic publicly. Service containers get their service name as a network alias, and accessory containers get their accessory name as a network alias. Add `network_aliases` for compatibility names:

```yaml
docker:
  network:
    name: ship-production
    driver: bridge

services:
  web:
    network_aliases:
      - app

accessories:
  postgres:
    network_aliases:
      - database

environments:
  staging:
    docker:
      network:
        enabled: false
```

Configured network names cannot be Docker's built-in `bridge`, `host`, or `none` networks.

Long-running service, accessory, and Caddy ingress containers default to Docker `restart_policy: unless-stopped`, so they come back after host or Docker daemon restarts unless you intentionally stopped them. Override per service or accessory with `no`, `always`, `unless-stopped`, `on-failure`, or `on-failure:N`. Release commands remain one-off containers and do not inherit restart policies.

```yaml
services:
  web:
    restart_policy: unless-stopped
  worker:
    restart_policy: on-failure:5

accessories:
  postgres:
    restart_policy: always
```

Caddy ingress persists `/data` and `/config` in Docker named volumes by default so automatic HTTPS certificates, private keys, OCSP staples, and Caddy runtime state survive container replacement and host restarts. Ship derives deterministic names from the project and environment, or you can set explicit volume names:

```yaml
ingress:
  caddy:
    data_volume: ship-example-production-caddy-data
    config_volume: ship-example-production-caddy-config
```

`ship releases ENV` shows local release history newest-first, including current and rollback-target markers, release status, image digests, and config hashes. Current release pointers are tracked per environment, so staging deploys cannot disturb production rollback state. Use `--json` for automation or incident timelines. `ship releases diff ENV --from OLD --to NEW` compares two release records across config hash, service image digests, and secret digest names without exposing secret values; use `--json` for deployment review gates.

`ship promote SOURCE_ENV TARGET_ENV` creates a fresh target-environment release from the exact image digests recorded on the source environment's current release, or from `--release RELEASE_ID`. Promotion skips build and pre-build hooks, then uses the target environment's secrets, registry auth, release commands, rollout health checks, maintenance preservation, schedules, post-deploy hooks, and release-state sync. Use `--dry-run` to preview target ingress and secret readiness without touching hosts.

For automatic release-phase work such as database migrations, configure `services.<service>.release`. Ship runs the command once from the new image after secrets and registry auth are synced, but before the rollout starts; a failure marks the release failed and stops deployment.

```yaml
services:
  web:
    release:
      command: bin/rails db:migrate
      replica: 1
      timeout_seconds: 600
```

For local lifecycle gates, notifications, and smoke checks, configure `hooks.pre_deploy`, `hooks.pre_build`, `hooks.post_deploy`, and `hooks.deploy_failed` at the root or under an environment. Root hooks run before environment hooks. Hooks run from the `ship.yml` directory, record `deploy_hook` events, and receive `SHIP_PROJECT`, `SHIP_ENVIRONMENT`, `SHIP_HOOK`, `SHIP_RELEASE`, `SHIP_CONFIG`, `SHIP_CONFIG_DIR`, and, for `deploy_failed`, `SHIP_FAILURE`.

```yaml
hooks:
  pre_deploy:
    - ./scripts/check-freeze-window
  pre_build:
    - command: ./scripts/build-policy
      timeout_seconds: 30
      env:
        POLICY_MODE: strict
  post_deploy:
    - ./scripts/smoke-production
  deploy_failed:
    - ./scripts/notify-deploy-failed
```

For remote release notifications, configure `notifications.webhooks` at the root or under an environment. Root webhooks run before environment webhooks, notification failures are recorded as `notification` events without failing the deploy, and payloads include the project, environment, operation, status, release, message, image digests, and time. Use `url_env` for secret endpoints, custom `headers` for bearer tokens or routing keys, and `events` to filter delivery. Supported events are `deploy:succeeded`, `deploy:failed`, `promote:succeeded`, `promote:failed`, `rollback:succeeded`, and `rollback:failed`; use `*` or prefixes like `deploy:*` for broader subscriptions.

```yaml
notifications:
  webhooks:
    - url_env: SHIP_DEPLOY_WEBHOOK
      events: [deploy:*, promote:succeeded, rollback:failed]
      timeout_seconds: 5
      headers:
        X-Ship-Environment: production
```

`ship exec` runs the command inside the current release container over the SSH-framed agent. Use `--all` to run once per placed replica, `--replica N` for one replica, `--timeout SECONDS` for long-running migrations, and `--json` for structured output. `ship accessory exec ENV NAME -- COMMAND` runs inside the persisted accessory container after checking the saved placement and observed topology, which is useful for database shells, cache inspection, and one-off administrative commands.

`ship restart ENV [SERVICE]` recreates current-release service containers without building or promoting a new release. Use `--replica N` with a service to restart one placed replica; restart reuses deployed secret env-file paths and runs configured health checks before reporting success.

`ship lock ENV` prevents real deploys to that environment until `ship unlock ENV` clears the lock. Locked deploy attempts are recorded as blocked events. Use `ship deploy ENV --ignore-lock` only for an intentional operator override. Ship also takes a local operation lock around real deploy, rollback, and prune runs so two Ship processes cannot mutate the same environment at once.

Recurring service schedules are deploy-managed. Add `services.<service>.schedules.<name>` with a five-field cron expression and Ship syncs `/etc/cron.d/ship-<project>-<env>-*` on deploy so jobs point at the current release container:

```yaml
services:
  web:
    schedules:
      cleanup:
        cron: "17 * * * *"
        command: bin/rails cleanup
        replica: 1
        timeout_seconds: 300
```

Accessory backups can be scheduled the same way. Add `accessories.<name>.backup.schedule` and Ship syncs a cron file to the persisted accessory host on deploy. Scheduled backups require `backup.command` and a saved accessory placement, so run `ship accessory deploy ENV NAME` once before relying on the recurring job. Add `backup.export_command` to copy the completed artifact to off-host storage such as S3, R2, B2, restic, or rclone remotes; Ship runs it with `SHIP_BACKUP_ARTIFACT` set and records the first output line as the exported artifact URI.

```yaml
accessories:
  postgres:
    backup:
      command: pg_dumpall
      export_command: 'aws s3 cp "$SHIP_BACKUP_ARTIFACT" s3://ship-backups/postgres/$(basename "$SHIP_BACKUP_ARTIFACT") && printf "s3://ship-backups/postgres/%s\n" "$(basename "$SHIP_BACKUP_ARTIFACT")"'
      export_timeout_seconds: 900
      artifact_dir: /var/lib/ship/backups
      schedule:
        cron: "13 3 * * *"
        timeout_seconds: 600
```

Accessories are managed explicitly:

```bash
ship accessory deploy production postgres
ship accessory status production postgres
ship accessory backup production postgres
ship --dry-run accessory restore production postgres --artifact /var/lib/ship/backups/postgres-20260630T120000.000000000Z.backup
ship accessory restore production postgres --artifact /var/lib/ship/backups/postgres-20260630T120000.000000000Z.backup --yes
```

## Recovery

Failed provision:

```bash
ship provision plan production
ship provision apply production --yes
```

Provisioning is label-based and safe to retry. Existing matching servers are reported as `exists`; extra matching servers are reported but not deleted.

Failed deploy:

```bash
ship recover production
ship events production
ship status production
ship --dry-run deploy production
ship deploy production
```

A failed deploy records a failed release and keeps the previous healthy release as current.

Failed health check:

```bash
ship recover production
ship logs production web --failed --lines 200
ship logs production web --lines 200
ship inspect production
ship deploy production
```

Health failures stop promotion and keep the previous healthy release as current. Use `ship logs ENV SERVICE --failed` to inspect the newest failed release container when it is still present, or `--release RELEASE_ID` for a specific release from `ship releases`. Ingress reload happens only after rollout health passes. Slow-starting services can set `services.<service>.rolling.health_retries` and `health_interval_seconds` to retry the same health check before failing the rollout.

Rollback:

```bash
ship recover production
ship --dry-run rollback production --to RELEASE_ID --allow-data-rollback
ship rollback production --to RELEASE_ID --allow-data-rollback
```

Use `--allow-data-rollback` when configured accessories make data compatibility a manual decision. Real rollbacks also compare the target release's recorded secret digests with currently rendered secrets before touching hosts; if they differ, Ship blocks before mutation and records a blocked rollback event. Use `ship secrets diff ENV` to inspect drift, restore the intended secret values, or pass `--allow-secret-drift` when you intentionally want the old image to run with the current secrets.

Accessory restore:

```bash
ship accessory status production postgres
ship accessory backup production postgres
ship --dry-run accessory restore production postgres --artifact /var/lib/ship/backups/postgres.backup
ship accessory restore production postgres --artifact /var/lib/ship/backups/postgres.backup --yes
```

Restore validates the saved placement, observed topology, artifact path, and restore check before running the restore command. `ship accessory backup` records both the local artifact path and, when `backup.export_command` prints one, the exported artifact URI in accessory state for incident handoff and off-host retention audits.

## Acceptance Tests

Default CI-safe coverage:

```bash
go test ./...
go build ./cmd/ship
```

The Phase 12 acceptance tests use fake Hetzner, fake Docker, and fake agents to verify the dry-run path and provision/deploy/scale/logs/status/recover/rollback workflow.

Optional read-only live Hetzner gate:

```bash
SHIP_LIVE_HETZNER=1 HCLOUD_TOKEN=... go test ./internal/cli -run TestLiveHetznerAcceptanceGate -count=1
```

Set `SHIP_LIVE_HETZNER_PROJECT` to override the label selector project. The live gate is skipped by default and does not create or destroy servers.

Optional destructive live Hetzner full-cycle gate:

```bash
SHIP_LIVE_HETZNER_DESTRUCTIVE=1 \
HCLOUD_TOKEN=... \
SHIP_LIVE_HETZNER_SSH_KEY=... \
SHIP_LIVE_HETZNER_REGISTRY=ttl.sh/your-public-test-repo \
go test ./internal/cli -run TestDestructiveLiveHetznerFullCycle -count=1
```

The destructive gate creates two servers, bootstraps agents, deploys the sample app, scales it, rolls back, and decommissions the servers. Use a disposable project name and a registry repository that the new hosts can pull from.

Optional local integrations, with no cloud credentials:

```bash
SHIP_LOCAL_REGISTRY_INTEGRATION=1 go test ./internal/docker -run TestLocalRegistryIntegrationBuildPushResolvePull -count=1
go test ./internal/ingress -run TestGeneratedCaddyfileValidatesWithCaddyBinary -count=1
```

The registry test starts a local Docker `registry:2` container. The Caddy validation test runs when a local `caddy` binary is available.

## Safety Notes

Commands that can touch real infrastructure support the global `--dry-run` flag where mutation is possible. Provider and agent interactions are injectable in tests, so normal CI does not require Docker, SSH hosts, registry credentials, or cloud credentials.

Production use should start with `ship doctor`, `ship provision plan`, and `ship --dry-run deploy`.
