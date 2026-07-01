# Configuration


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

## Providers

Pick one provider per environment. See the [provider guides](providers/README.md) for YAML examples, firewall behavior, optional fields, and required credentials.

Supported integrations: Hetzner, Vultr, DigitalOcean, Linode, AWS EC2, Lightsail, GCP, Azure, Scaleway, OpenStack, Civo, UpCloud, OVHcloud, OCI, Exoscale, cloudscale.ch, Latitude.sh, Kamatera, Proxmox, Terraform/OpenTofu, Pulumi, Ansible, OpenSSH config, and manual host lists.

## SSH connection settings

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

## Pool-level host shape overrides

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

## Cloud-init and user data

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

## Custom host labels and tags

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

Credential requirements for each provider are on the [provider guides](providers/README.md).

To implement a new provider in Ship, see [Adding a provider](../providers.md).
