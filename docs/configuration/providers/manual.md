# Manual hosts

Configure `environments.<env>.provider.manual`.

## Example

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


## Credentials

No cloud credentials required. Hosts are listed explicitly in `ship.yml`.

[← All providers](README.md) · [Configuration overview](../README.md)
