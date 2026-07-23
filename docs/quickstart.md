# Quick start


Build or install the CLI:

```bash
go install ./cmd/ship
ship --help
```

In application CI, pin Ship to an explicit released version instead of floating on `@latest`:

```bash
go install github.com/watzon/ship/cmd/ship@v0.5.3
ship version
```

Use a version that publishes release assets for your host platforms before relying on `ship agent upgrade` from CI — verify with `ship release check v0.5.3` before pinning. For runners or hosts without GitHub access, see [docs/airgap.md](airgap.md).

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

`ship secrets set ENV NAME` reads the value from environment variable `NAME` unless `--value` is provided. `ship secrets export ENV` prints plaintext dotenv output for local inspection; prefer `--redacted` in logs or shared terminal output. Environment-scoped `verify`, `render`, and `diff` read the encrypted store by default, so unrelated variables exported in the current shell cannot change their result. Pass `--with-process-env` only when you intentionally want to inspect the values a CI or deploy environment would overlay.

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
