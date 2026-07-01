# OVHcloud Public Cloud

Configure `environments.<env>.provider.ovhcloud`.

## Example

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


## Credentials

Set `OVH_APPLICATION_KEY`, `OVH_APPLICATION_SECRET`, and `OVH_CONSUMER_KEY`. `OVH_ENDPOINT` is optional and defaults to `ovh-eu`.

[← All providers](README.md) · [Configuration overview](../README.md)
