# Kamatera

Configure `environments.<env>.provider.kamatera`.

## Example

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


## Credentials

Set `KAMATERA_CLIENT_ID`, `KAMATERA_SECRET` or `KAMATERA_API_SECRET`, and the configured server password env var (for example `KAMATERA_SERVER_PASSWORD`).

[← All providers](README.md) · [Configuration overview](../README.md)
