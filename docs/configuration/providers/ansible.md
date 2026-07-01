# Ansible inventory

Configure `environments.<env>.provider.ansible`.

## Example

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


## Credentials

Static inventory files need no cloud credentials. Dynamic inventory commands need the configured binary and whatever credentials the inventory plugin or script requires.

[← All providers](README.md) · [Configuration overview](../README.md)
