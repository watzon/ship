package providers

import (
	"context"
	"testing"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider/ansible"
	"github.com/watzon/ship/internal/provider/aws"
	"github.com/watzon/ship/internal/provider/azure"
	"github.com/watzon/ship/internal/provider/civo"
	"github.com/watzon/ship/internal/provider/cloudscale"
	"github.com/watzon/ship/internal/provider/digitalocean"
	"github.com/watzon/ship/internal/provider/exoscale"
	"github.com/watzon/ship/internal/provider/gcp"
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

func TestForEnvironmentReturnsVultrProvider(t *testing.T) {
	prov, err := ForEnvironment(config.Environment{
		Provider: config.ProviderConfig{Vultr: &config.VultrConfig{Region: "ewr", Plan: "vc2-2c-4gb", OSID: 2284, SSHAllowedCIDRs: []string{"203.0.113.0/24"}}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := prov.(vultr.Client)
	if !ok {
		t.Fatalf("provider = %T, want vultr.Client", prov)
	}
	if !client.DryRun {
		t.Fatal("expected dry-run client")
	}
}

func TestForEnvironmentReturnsLinodeProvider(t *testing.T) {
	prov, err := ForEnvironment(config.Environment{
		Provider: config.ProviderConfig{Linode: &config.LinodeConfig{
			Region:          "us-east",
			Type:            "g6-standard-2",
			Image:           "linode/ubuntu24.04",
			AuthorizedKeys:  []string{"ssh-ed25519 AAAA..."},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := prov.(linode.Client)
	if !ok {
		t.Fatalf("provider = %T, want linode.Client", prov)
	}
	if !client.DryRun {
		t.Fatal("expected dry-run client")
	}
}

func TestForEnvironmentReturnsAWSProvider(t *testing.T) {
	prov, err := ForEnvironment(config.Environment{
		Provider: config.ProviderConfig{AWS: &config.AWSConfig{
			Region:          "us-east-1",
			InstanceType:    "t3.medium",
			AMI:             "ami-0123456789abcdef0",
			SubnetID:        "subnet-0123456789abcdef0",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := prov.(aws.Client)
	if !ok {
		t.Fatalf("provider = %T, want aws.Client", prov)
	}
	if !client.DryRun {
		t.Fatal("expected dry-run client")
	}
	if client.Region != "us-east-1" {
		t.Fatalf("region = %q", client.Region)
	}
}

func TestForEnvironmentReturnsGCPProvider(t *testing.T) {
	prov, err := ForEnvironment(config.Environment{
		Provider: config.ProviderConfig{GCP: &config.GCPConfig{
			ProjectID:       "demo-project",
			Zone:            "us-central1-a",
			MachineType:     "e2-medium",
			Image:           "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := prov.(gcp.Client)
	if !ok {
		t.Fatalf("provider = %T, want gcp.Client", prov)
	}
	if !client.DryRun {
		t.Fatal("expected dry-run client")
	}
	if client.ProjectID != "demo-project" || client.Zone != "us-central1-a" {
		t.Fatalf("client location = %s/%s", client.ProjectID, client.Zone)
	}
}

func TestForEnvironmentReturnsAzureProvider(t *testing.T) {
	prov, err := ForEnvironment(config.Environment{
		Provider: config.ProviderConfig{Azure: &config.AzureConfig{
			SubscriptionID:  "sub-123",
			ResourceGroup:   "rg-ship",
			Location:        "eastus",
			VMSize:          "Standard_B2s",
			Image:           "Canonical:ubuntu-24_04-lts:server:latest",
			AdminUsername:   "deploy",
			SSHPublicKey:    "ssh-ed25519 AAAA...",
			VirtualNetwork:  "ship-vnet",
			Subnet:          "default",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := prov.(azure.Client)
	if !ok {
		t.Fatalf("provider = %T, want azure.Client", prov)
	}
	if !client.DryRun {
		t.Fatal("expected dry-run client")
	}
	if client.SubscriptionID != "sub-123" || client.ResourceGroup != "rg-ship" {
		t.Fatalf("client scope = %s/%s", client.SubscriptionID, client.ResourceGroup)
	}
}

func TestForEnvironmentReturnsScalewayProvider(t *testing.T) {
	prov, err := ForEnvironment(config.Environment{
		Provider: config.ProviderConfig{Scaleway: &config.ScalewayConfig{
			ProjectID:       "project-id",
			Zone:            "fr-par-1",
			CommercialType:  "DEV1-S",
			Image:           "ubuntu_noble",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := prov.(scaleway.Client)
	if !ok {
		t.Fatalf("provider = %T, want scaleway.Client", prov)
	}
	if !client.DryRun {
		t.Fatal("expected dry-run client")
	}
	if client.ProjectID != "project-id" || client.Zone != "fr-par-1" {
		t.Fatalf("client scope = %s/%s", client.ProjectID, client.Zone)
	}
}

func TestForEnvironmentReturnsOpenStackProvider(t *testing.T) {
	prov, err := ForEnvironment(config.Environment{
		Provider: config.ProviderConfig{OpenStack: &config.OpenStackConfig{
			Region: "GRA11",
			Flavor: "b2-7",
			Image:  "ubuntu-24.04",
		}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := prov.(openstack.Client)
	if !ok {
		t.Fatalf("provider = %T, want openstack.Client", prov)
	}
	if !client.DryRun {
		t.Fatal("expected dry-run client")
	}
	if client.Region != "GRA11" {
		t.Fatalf("client region = %s", client.Region)
	}
}

func TestForEnvironmentReturnsCivoProvider(t *testing.T) {
	prov, err := ForEnvironment(config.Environment{
		Provider: config.ProviderConfig{Civo: &config.CivoConfig{
			Region:          "lon1",
			Size:            "g3.small",
			Image:           "ubuntu-noble",
			NetworkID:       "network-1",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := prov.(civo.Client)
	if !ok {
		t.Fatalf("provider = %T, want civo.Client", prov)
	}
	if !client.DryRun {
		t.Fatal("expected dry-run client")
	}
	if client.Region != "lon1" {
		t.Fatalf("client region = %s", client.Region)
	}
}

func TestForEnvironmentReturnsUpCloudProvider(t *testing.T) {
	prov, err := ForEnvironment(config.Environment{
		Provider: config.ProviderConfig{UpCloud: &config.UpCloudConfig{
			Zone:            "fi-hel1",
			Plan:            "1xCPU-1GB",
			Template:        "01000000-0000-4000-8000-000030240200",
			SSHKeys:         []string{"ssh-ed25519 AAAA..."},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := prov.(upcloud.Client)
	if !ok {
		t.Fatalf("provider = %T, want upcloud.Client", prov)
	}
	if !client.DryRun {
		t.Fatal("expected dry-run client")
	}
	if client.Zone != "fi-hel1" {
		t.Fatalf("client zone = %s", client.Zone)
	}
}

func TestForEnvironmentReturnsOVHCloudProvider(t *testing.T) {
	prov, err := ForEnvironment(config.Environment{
		Provider: config.ProviderConfig{OVHCloud: &config.OVHCloudConfig{
			ServiceName: "project-id",
			Region:      "GRA11",
			FlavorID:    "b2-7",
			ImageID:     "image-id",
			SSHKeyID:    "ssh-key-id",
		}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := prov.(ovhcloud.Client)
	if !ok {
		t.Fatalf("provider = %T, want ovhcloud.Client", prov)
	}
	if !client.DryRun {
		t.Fatal("expected dry-run client")
	}
	if client.ServiceName != "project-id" || client.Region != "GRA11" {
		t.Fatalf("client scope = %s/%s", client.ServiceName, client.Region)
	}
}

func TestForEnvironmentReturnsOCIProvider(t *testing.T) {
	prov, err := ForEnvironment(config.Environment{
		Provider: config.ProviderConfig{OCI: &config.OCIConfig{
			Region:             "us-ashburn-1",
			CompartmentID:      "ocid1.compartment.oc1..aaaa",
			AvailabilityDomain: "Uocm:US-ASHBURN-AD-1",
			Shape:              "VM.Standard.E4.Flex",
			ImageID:            "ocid1.image.oc1..image",
			SubnetID:           "ocid1.subnet.oc1..subnet",
			SSHAuthorizedKeys:  []string{"ssh-ed25519 AAAA..."},
			NetworkSecurityGroup: config.OCINetworkSecurityGroup{
				VCNID: "ocid1.vcn.oc1..vcn",
			},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := prov.(oci.Client)
	if !ok {
		t.Fatalf("provider = %T, want oci.Client", prov)
	}
	if !client.DryRun {
		t.Fatal("expected dry-run client")
	}
	if client.Region != "us-ashburn-1" || client.CompartmentID != "ocid1.compartment.oc1..aaaa" {
		t.Fatalf("client scope = %s/%s", client.Region, client.CompartmentID)
	}
}

func TestForEnvironmentReturnsExoscaleProvider(t *testing.T) {
	prov, err := ForEnvironment(config.Environment{
		Provider: config.ProviderConfig{Exoscale: &config.ExoscaleConfig{
			Zone:            "ch-gva-2",
			InstanceType:    "standard.medium",
			Template:        "template-id",
			SSHKeys:         []string{"deploy"},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := prov.(exoscale.Client)
	if !ok {
		t.Fatalf("provider = %T, want exoscale.Client", prov)
	}
	if !client.DryRun {
		t.Fatal("expected dry-run client")
	}
	if client.Zone != "ch-gva-2" {
		t.Fatalf("client zone = %s", client.Zone)
	}
}

func TestForEnvironmentReturnsCloudscaleProvider(t *testing.T) {
	prov, err := ForEnvironment(config.Environment{
		Provider: config.ProviderConfig{Cloudscale: &config.CloudscaleConfig{
			Zone:    "rma1",
			Flavor:  "flex-4-2",
			Image:   "debian-13",
			SSHKeys: []string{"ssh-ed25519 AAAA..."},
		}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := prov.(cloudscale.Client)
	if !ok {
		t.Fatalf("provider = %T, want cloudscale.Client", prov)
	}
	if !client.DryRun {
		t.Fatal("expected dry-run client")
	}
}

func TestForEnvironmentReturnsLatitudeProvider(t *testing.T) {
	prov, err := ForEnvironment(config.Environment{
		Provider: config.ProviderConfig{Latitude: &config.LatitudeConfig{
			Project:         "proj-demo",
			Site:            "ASH",
			Plan:            "c2-small-x86",
			OperatingSystem: "ubuntu_24_04_x64_lts",
			SSHKeys:         []string{"ssh-key-1"},
			DeleteReason:    "ship decommission",
		}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := prov.(latitude.Client)
	if !ok {
		t.Fatalf("provider = %T, want latitude.Client", prov)
	}
	if !client.DryRun {
		t.Fatal("expected dry-run client")
	}
	if client.DeleteReason != "ship decommission" {
		t.Fatalf("delete reason = %q", client.DeleteReason)
	}
}

func TestForEnvironmentReturnsProxmoxProvider(t *testing.T) {
	prov, err := ForEnvironment(config.Environment{
		Provider: config.ProviderConfig{Proxmox: &config.ProxmoxConfig{
			APIURL:     "https://pve.example.com:8006/api2/json",
			Node:       "pve1",
			TemplateID: 9000,
			MemoryMB:   2048,
			Cores:      2,
		}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := prov.(proxmox.Client)
	if !ok {
		t.Fatalf("provider = %T, want proxmox.Client", prov)
	}
	if !client.DryRun {
		t.Fatal("expected dry-run client")
	}
	if client.Config.Node != "pve1" || client.Config.TemplateID != 9000 {
		t.Fatalf("config = %+v", client.Config)
	}
}

func TestForEnvironmentReturnsKamateraProvider(t *testing.T) {
	prov, err := ForEnvironment(config.Environment{
		Provider: config.ProviderConfig{Kamatera: &config.KamateraConfig{
			Datacenter:  "US-NY2",
			CPU:         "2B",
			RAMMB:       4096,
			Image:       "US-NY2:ubuntu_server_24.04_64-bit",
			DiskGB:      40,
			PasswordEnv: "SHIP_KAMATERA_PASSWORD",
		}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := prov.(kamatera.Client)
	if !ok {
		t.Fatalf("provider = %T, want kamatera.Client", prov)
	}
	if !client.DryRun {
		t.Fatal("expected dry-run client")
	}
	if client.PasswordEnv != "SHIP_KAMATERA_PASSWORD" {
		t.Fatalf("password env = %q", client.PasswordEnv)
	}
}

func TestForEnvironmentReturnsLightsailProvider(t *testing.T) {
	force := false
	prov, err := ForEnvironment(config.Environment{
		Provider: config.ProviderConfig{Lightsail: &config.LightsailConfig{
			Region:            "us-east-1",
			AvailabilityZone:  "us-east-1a",
			BundleID:          "nano_3_0",
			BlueprintID:       "ubuntu_24_04",
			ForceDeleteAddOns: &force,
		}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := prov.(lightsail.Client)
	if !ok {
		t.Fatalf("provider = %T, want lightsail.Client", prov)
	}
	if !client.DryRun {
		t.Fatal("expected dry-run client")
	}
	if client.Region != "us-east-1" || client.ForceDeleteAddOns == nil || *client.ForceDeleteAddOns {
		t.Fatalf("client = %+v", client)
	}
}

func TestForEnvironmentReturnsSSHConfigProvider(t *testing.T) {
	env := config.Environment{
		Provider: config.ProviderConfig{SSHConfig: &config.SSHConfigInventory{Path: "~/.ssh/config"}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {Hosts: []string{"web-prod"}},
		}},
	}
	prov, err := ForEnvironment(env, true)
	if err != nil {
		t.Fatal(err)
	}
	sshProvider, ok := prov.(sshconfig.Provider)
	if !ok {
		t.Fatalf("provider = %T, want sshconfig.Provider", prov)
	}
	if !sshProvider.DryRun {
		t.Fatal("expected dry-run provider")
	}
}

func TestForEnvironmentReturnsManualProvider(t *testing.T) {
	env := config.Environment{
		Provider: config.ProviderConfig{Manual: &config.ManualConfig{}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {Hosts: []string{"web-1.example.com"}},
		}},
	}
	prov, err := ForEnvironment(env, true)
	if err != nil {
		t.Fatal(err)
	}
	manualProvider, ok := prov.(manual.Provider)
	if !ok {
		t.Fatalf("provider = %T, want manual.Provider", prov)
	}
	if !manualProvider.DryRun {
		t.Fatal("expected dry-run provider")
	}
	hosts, err := manualProvider.List(context.Background(), "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].Name != "web-1.example.com" {
		t.Fatalf("hosts = %+v", hosts)
	}
}

func TestForEnvironmentReturnsTerraformProvider(t *testing.T) {
	env := config.Environment{
		Provider: config.ProviderConfig{Terraform: &config.TerraformConfig{
			Binary: "tofu",
			Output: "ship_hosts",
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {},
		}},
	}
	prov, err := ForEnvironment(env, true)
	if err != nil {
		t.Fatal(err)
	}
	terraformProvider, ok := prov.(terraformprovider.Provider)
	if !ok {
		t.Fatalf("provider = %T, want terraform.Provider", prov)
	}
	if !terraformProvider.DryRun {
		t.Fatal("expected dry-run provider")
	}
}

func TestForEnvironmentReturnsPulumiProvider(t *testing.T) {
	env := config.Environment{
		Provider: config.ProviderConfig{Pulumi: &config.PulumiConfig{
			Output: "ship_hosts",
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {},
		}},
	}
	prov, err := ForEnvironment(env, true)
	if err != nil {
		t.Fatal(err)
	}
	pulumiProvider, ok := prov.(pulumiprovider.Provider)
	if !ok {
		t.Fatalf("provider = %T, want pulumi.Provider", prov)
	}
	if !pulumiProvider.DryRun {
		t.Fatal("expected dry-run provider")
	}
}

func TestForEnvironmentReturnsAnsibleProvider(t *testing.T) {
	env := config.Environment{
		Provider: config.ProviderConfig{Ansible: &config.AnsibleConfig{
			InventoryFile: "inventory.yml",
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {},
		}},
	}
	prov, err := ForEnvironment(env, true)
	if err != nil {
		t.Fatal(err)
	}
	ansibleProvider, ok := prov.(ansible.Provider)
	if !ok {
		t.Fatalf("provider = %T, want ansible.Provider", prov)
	}
	if !ansibleProvider.DryRun {
		t.Fatal("expected dry-run provider")
	}
}

func TestForEnvironmentReturnsDigitalOceanProvider(t *testing.T) {
	prov, err := ForEnvironment(config.Environment{
		Provider: config.ProviderConfig{DigitalOcean: &config.DigitalOceanConfig{
			Region:          "nyc3",
			Size:            "s-2vcpu-4gb",
			Image:           "ubuntu-24-04-x64",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := prov.(digitalocean.Client)
	if !ok {
		t.Fatalf("provider = %T, want digitalocean.Client", prov)
	}
	if !client.DryRun {
		t.Fatal("expected dry-run client")
	}
}
