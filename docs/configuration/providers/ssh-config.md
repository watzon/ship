# OpenSSH config

Configure `environments.<env>.provider.ssh_config`.

## Example

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


## Credentials

No cloud credentials required. Pool hosts are SSH aliases from your config file.

[← All providers](README.md) · [Configuration overview](../README.md)
