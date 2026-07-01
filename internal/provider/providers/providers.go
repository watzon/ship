package providers

import (
	"fmt"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
	"github.com/watzon/ship/internal/provider/ansible"
	"github.com/watzon/ship/internal/provider/aws"
	"github.com/watzon/ship/internal/provider/azure"
	"github.com/watzon/ship/internal/provider/civo"
	"github.com/watzon/ship/internal/provider/cloudscale"
	"github.com/watzon/ship/internal/provider/digitalocean"
	"github.com/watzon/ship/internal/provider/exoscale"
	"github.com/watzon/ship/internal/provider/gcp"
	"github.com/watzon/ship/internal/provider/hetzner"
	"github.com/watzon/ship/internal/provider/kamatera"
	"github.com/watzon/ship/internal/provider/latitude"
	"github.com/watzon/ship/internal/provider/lightsail"
	"github.com/watzon/ship/internal/provider/linode"
	"github.com/watzon/ship/internal/provider/manual"
	"github.com/watzon/ship/internal/provider/oci"
	"github.com/watzon/ship/internal/provider/openstack"
	"github.com/watzon/ship/internal/provider/ovhcloud"
	"github.com/watzon/ship/internal/provider/proxmox"
	pulumiprovider "github.com/watzon/ship/internal/provider/pulumi"
	"github.com/watzon/ship/internal/provider/scaleway"
	"github.com/watzon/ship/internal/provider/sshconfig"
	terraformprovider "github.com/watzon/ship/internal/provider/terraform"
	"github.com/watzon/ship/internal/provider/upcloud"
	"github.com/watzon/ship/internal/provider/vultr"
)

func ForEnvironment(env config.Environment, dryRun bool) (provider.Provider, error) {
	switch env.Provider.Name() {
	case config.ProviderHetzner:
		return hetzner.NewFromEnv(dryRun), nil
	case config.ProviderVultr:
		return vultr.NewFromEnv(dryRun), nil
	case config.ProviderDigitalOcean:
		return digitalocean.NewFromEnv(dryRun), nil
	case config.ProviderLinode:
		return linode.NewFromEnv(dryRun), nil
	case config.ProviderAWS:
		region := ""
		if env.Provider.AWS != nil {
			region = env.Provider.AWS.Region
		}
		return aws.NewFromEnv(dryRun, region), nil
	case config.ProviderLightsail:
		var cfg config.LightsailConfig
		if env.Provider.Lightsail != nil {
			cfg = *env.Provider.Lightsail
		}
		return lightsail.NewFromEnv(dryRun, cfg), nil
	case config.ProviderGCP:
		var cfg config.GCPConfig
		if env.Provider.GCP != nil {
			cfg = *env.Provider.GCP
		}
		return gcp.NewFromEnv(dryRun, cfg), nil
	case config.ProviderAzure:
		var cfg config.AzureConfig
		if env.Provider.Azure != nil {
			cfg = *env.Provider.Azure
		}
		return azure.NewFromEnv(dryRun, cfg), nil
	case config.ProviderScaleway:
		var cfg config.ScalewayConfig
		if env.Provider.Scaleway != nil {
			cfg = *env.Provider.Scaleway
		}
		return scaleway.NewFromEnv(dryRun, cfg), nil
	case config.ProviderOpenStack:
		var cfg config.OpenStackConfig
		if env.Provider.OpenStack != nil {
			cfg = *env.Provider.OpenStack
		}
		return openstack.NewFromEnv(dryRun, cfg), nil
	case config.ProviderCivo:
		var cfg config.CivoConfig
		if env.Provider.Civo != nil {
			cfg = *env.Provider.Civo
		}
		return civo.NewFromEnv(dryRun, cfg), nil
	case config.ProviderUpCloud:
		var cfg config.UpCloudConfig
		if env.Provider.UpCloud != nil {
			cfg = *env.Provider.UpCloud
		}
		return upcloud.NewFromEnv(dryRun, cfg), nil
	case config.ProviderOVHCloud:
		var cfg config.OVHCloudConfig
		if env.Provider.OVHCloud != nil {
			cfg = *env.Provider.OVHCloud
		}
		return ovhcloud.NewFromEnv(dryRun, cfg), nil
	case config.ProviderOCI:
		var cfg config.OCIConfig
		if env.Provider.OCI != nil {
			cfg = *env.Provider.OCI
		}
		return oci.NewFromEnv(dryRun, cfg), nil
	case config.ProviderExoscale:
		var cfg config.ExoscaleConfig
		if env.Provider.Exoscale != nil {
			cfg = *env.Provider.Exoscale
		}
		return exoscale.NewFromEnv(dryRun, cfg), nil
	case config.ProviderCloudscale:
		var cfg config.CloudscaleConfig
		if env.Provider.Cloudscale != nil {
			cfg = *env.Provider.Cloudscale
		}
		return cloudscale.NewFromEnv(dryRun, cfg), nil
	case config.ProviderLatitude:
		var cfg config.LatitudeConfig
		if env.Provider.Latitude != nil {
			cfg = *env.Provider.Latitude
		}
		return latitude.NewFromEnv(dryRun, cfg), nil
	case config.ProviderKamatera:
		var cfg config.KamateraConfig
		if env.Provider.Kamatera != nil {
			cfg = *env.Provider.Kamatera
		}
		return kamatera.NewFromEnv(dryRun, cfg), nil
	case config.ProviderProxmox:
		var cfg config.ProxmoxConfig
		if env.Provider.Proxmox != nil {
			cfg = *env.Provider.Proxmox
		}
		return proxmox.NewFromEnv(dryRun, cfg), nil
	case config.ProviderSSHConfig:
		return sshconfig.New(dryRun, env), nil
	case config.ProviderTerraform:
		return terraformprovider.New(dryRun, env), nil
	case config.ProviderPulumi:
		return pulumiprovider.New(dryRun, env), nil
	case config.ProviderAnsible:
		return ansible.New(dryRun, env), nil
	case config.ProviderManual:
		return manual.New(dryRun, env), nil
	default:
		return nil, fmt.Errorf("unsupported provider %q", env.Provider.Name())
	}
}
