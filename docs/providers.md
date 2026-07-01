# Adding a Provider

Ship providers create, list, and delete ordinary Linux hosts for an environment. The CLI stays provider-neutral by talking only to `internal/provider.Provider`.

Currently supported providers are Hetzner Cloud, Vultr, DigitalOcean, Linode, AWS EC2, Amazon Lightsail, Google Compute Engine, Azure Virtual Machines, Scaleway Instances, OpenStack-compatible clouds, Civo, UpCloud, OVHcloud Public Cloud, Oracle Cloud Infrastructure Compute, Exoscale Compute, cloudscale.ch servers, Latitude.sh bare metal, Kamatera cloud servers, Proxmox VE VMs, Terraform/OpenTofu-managed hosts, Pulumi-managed hosts, Ansible inventory hosts, OpenSSH config inventory, and manual SSH hosts.

## Config

Add a provider constant and config block in `internal/config/config.go`.

- Add `Provider<Name>` beside the existing provider constants.
- Add a pointer field to `ProviderConfig` with the provider YAML name.
- Teach `UnmarshalYAML`, `Name`, `Validate`, and `blocks` about the provider.
- Validate all fields needed before the first API call. Prefer one clear provider block and concrete field errors.

Provider config should describe the host shape for all pools in the environment. Pool counts and explicit host names stay under `hosts.pools`.

## Factory

Register the provider in `internal/provider/providers/providers.go`.

`ForEnvironment` must return a provider initialized from environment variables and the global dry-run flag. The CLI and doctor use this factory, so provider-specific code should not be added to command handlers.

## Provider Contract

Implement `provider.Provider`.

- `Name` returns the config provider constant.
- `PlanHosts` converts Ship host pools into `provider.HostPlan` values without making API calls.
- `Reconcile` validates project/environment, honors dry-run by returning desired plans only, and delegates matching/create behavior to `provider.ReconcileHosts`.
- `List` returns only Ship-managed hosts for the requested project and environment.
- `Delete` deletes the given provider host by provider ID.
- `CredentialChecks` reports required environment variables for `ship doctor`.

Use the standard library HTTP client unless there is a strong reason to add a dependency.

## Ownership Metadata

Every provider must mark created hosts with Ship ownership metadata:

- `managed-by=ship`
- `project=<project>`
- `environment=<environment>`
- `pool=<pool>`

Use native labels when the provider supports key/value labels. Use stable tags when it does not. If a provider does not expose arbitrary per-host metadata through its list API, use a deterministic Ship-owned cloud resource name prefix and strip it back to the normal Ship host name internally. `List` must filter to the requested project and environment before returning hosts, and `Delete` should only be reached for hosts returned by that filtered list.

## Tests

Provider tests should use `httptest` fake APIs, not live credentials.

Cover:

- host planning from pools
- ownership metadata on create
- list filtering and pagination
- create missing, keep existing, and report extra hosts through reconcile
- delete by provider ID
- dry-run behavior
- missing credential checks
- factory routing from config to provider

Update `README.md` with user-facing config and credential notes whenever a provider becomes supported.
