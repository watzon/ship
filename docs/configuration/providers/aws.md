# AWS EC2

Configure `environments.<env>.provider.aws`.

## Example

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


## Credentials

Set `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`. `AWS_SESSION_TOKEN` is supported for temporary credentials.

[← All providers](README.md) · [Configuration overview](../README.md)
