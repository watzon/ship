package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSample(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultConfigFile)
	if err := os.WriteFile(path, []byte(Sample()), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Project != "example" {
		t.Fatalf("project = %q", cfg.Project)
	}
	if cfg.Services["web"].Scale != 6 {
		t.Fatalf("web scale = %d", cfg.Services["web"].Scale)
	}
}

func TestValidateReportsMissingPool(t *testing.T) {
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"worker": {Pool: "worker", Scale: 1, Image: ImageSpec{Ref: "example"}},
		},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "missing pool") {
		t.Fatalf("expected missing pool error, got %v", err)
	}
}

func TestValidateAcceptsHetznerProvider(t *testing.T) {
	cfg := minimalValidConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderHetzner {
		t.Fatalf("provider name = %q, want %q", got, ProviderHetzner)
	}
}

func TestResolveEnvironmentMergesSSHConfig(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.SSH = SSHConfig{
		Port:           22,
		IdentityFile:   "~/.ssh/root",
		KnownHostsFile: ".ship/known_hosts",
		JumpHost:       "root-bastion.example.com",
		Options:        map[string]string{"ControlMaster": "auto", "ControlPersist": "60s"},
	}
	env := cfg.Environments["production"]
	env.SSH = SSHConfig{
		Port:     2222,
		JumpHost: "prod-bastion.example.com",
		Options:  map[string]string{"ControlPersist": "5m"},
	}
	cfg.Environments["production"] = env

	_, resolvedEnv, err := cfg.ResolveEnvironment("production")
	if err != nil {
		t.Fatal(err)
	}
	if resolvedEnv.SSH.Port != 2222 || resolvedEnv.SSH.IdentityFile != "~/.ssh/root" || resolvedEnv.SSH.KnownHostsFile != ".ship/known_hosts" || resolvedEnv.SSH.JumpHost != "prod-bastion.example.com" {
		t.Fatalf("resolved ssh config = %+v", resolvedEnv.SSH)
	}
	if resolvedEnv.SSH.Options["ControlMaster"] != "auto" || resolvedEnv.SSH.Options["ControlPersist"] != "5m" {
		t.Fatalf("resolved ssh options = %+v", resolvedEnv.SSH.Options)
	}
}

func TestValidateRejectsInvalidSSHConfig(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.SSH = SSHConfig{Port: 70000}
	env := cfg.Environments["production"]
	env.Hosts.Pools["web"] = Pool{
		Count: 1,
		SSH: SSHConfig{Options: map[string]string{
			"": "yes",
		}},
	}
	cfg.Environments["production"] = env
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected SSH validation error")
	}
	for _, want := range []string{"root ssh.port must be between 1 and 65535", "environment \"production\" pool \"web\" ssh.options cannot contain an empty option name"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q:\n%v", want, err)
		}
	}
}

func TestValidateAcceptsVultrProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Vultr: &VultrConfig{Region: "ewr", Plan: "vc2-2c-4gb", OSID: 2284, SSHAllowedCIDRs: []string{"203.0.113.0/24"}}},
		Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderVultr {
		t.Fatalf("provider name = %q, want %q", got, ProviderVultr)
	}
}

func TestValidateAcceptsDigitalOceanProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{DigitalOcean: &DigitalOceanConfig{
			Region:          "nyc3",
			Size:            "s-2vcpu-4gb",
			Image:           "ubuntu-24-04-x64",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderDigitalOcean {
		t.Fatalf("provider name = %q, want %q", got, ProviderDigitalOcean)
	}
}

func TestValidateAcceptsLinodeProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Linode: &LinodeConfig{
			Region:          "us-east",
			Type:            "g6-standard-2",
			Image:           "linode/ubuntu24.04",
			AuthorizedKeys:  []string{"ssh-ed25519 AAAA..."},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderLinode {
		t.Fatalf("provider name = %q, want %q", got, ProviderLinode)
	}
}

func TestValidateAcceptsAWSProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{AWS: &AWSConfig{
			Region:          "us-east-1",
			InstanceType:    "t3.medium",
			AMI:             "ami-0123456789abcdef0",
			SubnetID:        "subnet-0123456789abcdef0",
			VPCID:           "vpc-0123456789abcdef0",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderAWS {
		t.Fatalf("provider name = %q, want %q", got, ProviderAWS)
	}
}

func TestValidateAcceptsGCPProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{GCP: &GCPConfig{
			ProjectID:       "demo-project",
			Zone:            "us-central1-a",
			MachineType:     "e2-medium",
			Image:           "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderGCP {
		t.Fatalf("provider name = %q, want %q", got, ProviderGCP)
	}
}

func TestValidateAcceptsAzureProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Azure: &AzureConfig{
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
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderAzure {
		t.Fatalf("provider name = %q, want %q", got, ProviderAzure)
	}
}

func TestValidateAcceptsCivoProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Civo: &CivoConfig{
			Region:          "lon1",
			Size:            "g3.small",
			Image:           "ubuntu-noble",
			NetworkID:       "network-1",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderCivo {
		t.Fatalf("provider name = %q, want %q", got, ProviderCivo)
	}
}

func TestValidateAcceptsUpCloudProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{UpCloud: &UpCloudConfig{
			Zone:            "fi-hel1",
			Plan:            "1xCPU-1GB",
			Template:        "01000000-0000-4000-8000-000030240200",
			SSHKeys:         []string{"ssh-ed25519 AAAA..."},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderUpCloud {
		t.Fatalf("provider name = %q, want %q", got, ProviderUpCloud)
	}
}

func TestValidateAcceptsOVHCloudProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{OVHCloud: &OVHCloudConfig{
			ServiceName: "project-id",
			Region:      "GRA11",
			FlavorID:    "b2-7",
			ImageID:     "image-id",
			SSHKeyID:    "ssh-key-id",
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderOVHCloud {
		t.Fatalf("provider name = %q, want %q", got, ProviderOVHCloud)
	}
}

func TestValidateAcceptsOCIProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{OCI: &OCIConfig{
			Region:             "us-ashburn-1",
			CompartmentID:      "ocid1.compartment.oc1..aaaa",
			AvailabilityDomain: "Uocm:US-ASHBURN-AD-1",
			Shape:              "VM.Standard.E4.Flex",
			ImageID:            "ocid1.image.oc1..image",
			SubnetID:           "ocid1.subnet.oc1..subnet",
			SSHAuthorizedKeys:  []string{"ssh-ed25519 AAAA..."},
			NetworkSecurityGroup: OCINetworkSecurityGroup{
				VCNID: "ocid1.vcn.oc1..vcn",
			},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderOCI {
		t.Fatalf("provider name = %q, want %q", got, ProviderOCI)
	}
}

func TestValidateAcceptsExoscaleProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Exoscale: &ExoscaleConfig{
			Zone:            "ch-gva-2",
			InstanceType:    "standard.medium",
			Template:        "template-id",
			SSHKeys:         []string{"deploy"},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderExoscale {
		t.Fatalf("provider name = %q, want %q", got, ProviderExoscale)
	}
}

func TestValidateAcceptsCloudscaleProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Cloudscale: &CloudscaleConfig{
			Zone:    "rma1",
			Flavor:  "flex-4-2",
			Image:   "debian-13",
			SSHKeys: []string{"ssh-ed25519 AAAA..."},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderCloudscale {
		t.Fatalf("provider name = %q, want %q", got, ProviderCloudscale)
	}
}

func TestValidateRejectsCloudscaleInterfacesWithNetworkFlags(t *testing.T) {
	cfg := minimalValidConfig()
	usePrivate := true
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Cloudscale: &CloudscaleConfig{
			Zone:              "rma1",
			Flavor:            "flex-4-2",
			Image:             "debian-13",
			SSHKeys:           []string{"ssh-ed25519 AAAA..."},
			UsePrivateNetwork: &usePrivate,
			Interfaces:        []CloudscaleInterface{{Network: "public"}},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "provider.cloudscale.interfaces cannot be combined") {
		t.Fatalf("expected cloudscale interfaces error, got %v", err)
	}
}

func TestValidateRejectsInvalidCloudscaleUserDataHandling(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Cloudscale: &CloudscaleConfig{
			Zone:             "rma1",
			Flavor:           "flex-4-2",
			Image:            "debian-13",
			SSHKeys:          []string{"ssh-ed25519 AAAA..."},
			UserDataHandling: "merge",
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "provider.cloudscale.user_data_handling must be pass-through or extend-cloud-config") {
		t.Fatalf("expected cloudscale user_data_handling error, got %v", err)
	}
}

func TestValidateRejectsCloudscaleManagedServerGroupWithExplicitUUID(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Cloudscale: &CloudscaleConfig{
			Zone:    "rma1",
			Flavor:  "flex-4-2",
			Image:   "debian-13",
			SSHKeys: []string{"ssh-ed25519 AAAA..."},
			ServerGroup: CloudscaleServerGroupConfig{
				Managed: boolPtr(true),
				UUID:    "group-id",
			},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "provider.cloudscale.server_group.uuid requires server_group.managed: false") {
		t.Fatalf("expected cloudscale server_group error, got %v", err)
	}
}

func TestValidateAcceptsLatitudeProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Latitude: &LatitudeConfig{
			Project:         "proj-demo",
			Site:            "ASH",
			Plan:            "c2-small-x86",
			OperatingSystem: "ubuntu_24_04_x64_lts",
			SSHKeys:         []string{"ssh-key-1"},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
			RAID:            "raid-1",
			Billing:         "hourly",
			DiskLayout:      []LatitudeDiskLayout{{Count: 2, Role: "os", RAIDLevel: "raid-1", Filesystem: "ext4", MountPoint: "/"}},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderLatitude {
		t.Fatalf("provider name = %q, want %q", got, ProviderLatitude)
	}
}

func TestValidateRequiresLatitudeManagedSSHAllowedCIDRs(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Latitude: &LatitudeConfig{
			Project:         "proj-demo",
			Site:            "ASH",
			Plan:            "c2-small-x86",
			OperatingSystem: "ubuntu_24_04_x64_lts",
			SSHKeys:         []string{"ssh-key-1"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "provider.latitude.ssh_allowed_cidrs is required") {
		t.Fatalf("expected latitude ssh_allowed_cidrs error, got %v", err)
	}
}

func TestValidateRejectsLatitudeManagedFirewallWithExplicitID(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Latitude: &LatitudeConfig{
			Project:         "proj-demo",
			Site:            "ASH",
			Plan:            "c2-small-x86",
			OperatingSystem: "ubuntu_24_04_x64_lts",
			SSHKeys:         []string{"ssh-key-1"},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
			Firewall:        LatitudeFirewall{ID: "fw_1"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `provider.latitude.firewall.id requires firewall.managed: false`) {
		t.Fatalf("expected latitude firewall validation error, got %v", err)
	}
}

func TestValidateAcceptsProxmoxProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Proxmox: &ProxmoxConfig{
			APIURL:     "https://pve.example.com:8006/api2/json",
			Node:       "pve1",
			TemplateID: 9000,
			Storage:    "local-zfs",
			Bridge:     "vmbr0",
			VLAN:       30,
			MemoryMB:   2048,
			Cores:      2,
			SSHKeys:    []string{"ssh-ed25519 AAAA..."},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderProxmox {
		t.Fatalf("provider name = %q, want %q", got, ProviderProxmox)
	}
}

func TestValidateRejectsInvalidProxmoxProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Proxmox: &ProxmoxConfig{
			TemplateID: -1,
			MemoryMB:   -1,
			Cores:      -1,
			VLAN:       -1,
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected proxmox validation error")
	}
	for _, want := range []string{
		"provider.proxmox.api_url is required",
		"provider.proxmox.node is required",
		"provider.proxmox.template_id cannot be negative",
		"provider.proxmox.memory_mb cannot be negative",
		"provider.proxmox.cores cannot be negative",
		"provider.proxmox.vlan cannot be negative",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q:\n%v", want, err)
		}
	}
}

func TestValidateAcceptsKamateraProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Kamatera: &KamateraConfig{
			Datacenter:   "US-NY2",
			CPU:          "2B",
			RAMMB:        4096,
			Image:        "US-NY2:ubuntu_server_24.04_64-bit",
			DiskGB:       40,
			PasswordEnv:  "SHIP_KAMATERA_PASSWORD",
			Billing:      "hourly",
			Traffic:      "t5000",
			Network:      "wan",
			SSHPublicKey: "ssh-ed25519 AAAA...",
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1, Location: "IL", Size: "4B"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderKamatera {
		t.Fatalf("provider name = %q, want %q", got, ProviderKamatera)
	}
}

func TestValidateRejectsInvalidKamateraProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Kamatera: &KamateraConfig{
			RAMMB:               -1,
			DiskGB:              -1,
			NetworkBits:         -1,
			WaitTimeoutSeconds:  -1,
			PollIntervalSeconds: -1,
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected kamatera validation error")
	}
	for _, want := range []string{
		"provider.kamatera.datacenter is required",
		"provider.kamatera.cpu is required",
		"provider.kamatera.ram_mb must be positive",
		"provider.kamatera.image is required",
		"provider.kamatera.disk_gb must be positive",
		"provider.kamatera.network_bits cannot be negative",
		"provider.kamatera.wait_timeout_seconds cannot be negative",
		"provider.kamatera.poll_interval_seconds cannot be negative",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q:\n%v", want, err)
		}
	}
}

func TestValidateAcceptsLightsailProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Lightsail: &LightsailConfig{
			Region:           "us-east-1",
			AvailabilityZone: "us-east-1a",
			BundleID:         "nano_3_0",
			BlueprintID:      "ubuntu_24_04",
			KeyPairName:      "ship-key",
			IPAddressType:    "dualstack",
			AddOns: []LightsailAddOn{{
				Type:              "AutoSnapshot",
				SnapshotTimeOfDay: "06:00",
			}},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1, Location: "us-east-1b", Size: "small_3_0"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderLightsail {
		t.Fatalf("provider name = %q, want %q", got, ProviderLightsail)
	}
}

func TestValidateRejectsInvalidLightsailProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Lightsail: &LightsailConfig{
			IPAddressType:       "public",
			AddOns:              []LightsailAddOn{{Type: "Unknown"}},
			WaitTimeoutSeconds:  -1,
			PollIntervalSeconds: -1,
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1, Location: "us-west-2a"}}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected lightsail validation error")
	}
	for _, want := range []string{
		"provider.lightsail.region is required",
		"provider.lightsail.availability_zone is required",
		"provider.lightsail.bundle_id is required",
		"provider.lightsail.blueprint_id is required",
		"provider.lightsail.ip_address_type must be dualstack, ipv4, or ipv6",
		"provider.lightsail.ssh_allowed_cidrs is required when managed firewall SSH is enabled",
		"provider.lightsail.add_ons[0].type must be AutoSnapshot or StopInstanceOnIdle",
		"provider.lightsail.wait_timeout_seconds cannot be negative",
		"provider.lightsail.poll_interval_seconds cannot be negative",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q:\n%v", want, err)
		}
	}
}

func TestValidateRejectsKamateraPoolUserData(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Kamatera: &KamateraConfig{
			Datacenter: "US-NY2",
			CPU:        "2B",
			RAMMB:      4096,
			Image:      "US-NY2:ubuntu_server_24.04_64-bit",
			DiskGB:     40,
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1, UserData: "#cloud-config\n"}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "provider.kamatera pool \"web\" user_data is not supported") {
		t.Fatalf("expected kamatera user_data validation error, got %v", err)
	}
}

func TestValidateAcceptsSSHConfigProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{SSHConfig: &SSHConfigInventory{Path: "~/.ssh/config", User: "deploy"}},
		Hosts: HostsConfig{Pools: map[string]Pool{
			"web": {Hosts: []string{"web-prod"}},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderSSHConfig {
		t.Fatalf("provider name = %q, want %q", got, ProviderSSHConfig)
	}
}

func TestValidateRequiresSSHConfigHosts(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{SSHConfig: &SSHConfigInventory{}},
		Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "ssh_config provider requires pool \"web\" to define hosts") {
		t.Fatalf("expected ssh_config hosts validation error, got %v", err)
	}
}

func TestValidateAcceptsTerraformProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Terraform: &TerraformConfig{
			WorkingDir: "infra",
			Workspace:  "production",
			Binary:     "tofu",
			Output:     "ship_hosts",
			User:       "deploy",
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderTerraform {
		t.Fatalf("provider name = %q, want %q", got, ProviderTerraform)
	}
}

func TestValidateRequiresTerraformOutput(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Terraform: &TerraformConfig{}},
		Hosts:    HostsConfig{Pools: map[string]Pool{"web": {}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "provider.terraform.output is required") {
		t.Fatalf("expected terraform output validation error, got %v", err)
	}
}

func TestValidateAcceptsPulumiProvider(t *testing.T) {
	cfg := minimalValidConfig()
	showSecrets := true
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Pulumi: &PulumiConfig{
			WorkingDir:  "infra",
			Stack:       "production",
			Binary:      "pulumi",
			Output:      "ship_hosts",
			User:        "deploy",
			ShowSecrets: &showSecrets,
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderPulumi {
		t.Fatalf("provider name = %q, want %q", got, ProviderPulumi)
	}
}

func TestValidateRequiresPulumiOutput(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Pulumi: &PulumiConfig{}},
		Hosts:    HostsConfig{Pools: map[string]Pool{"web": {}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "provider.pulumi.output is required") {
		t.Fatalf("expected pulumi output validation error, got %v", err)
	}
}

func TestValidateAcceptsAnsibleProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Ansible: &AnsibleConfig{
			InventoryFile: "inventory.yml",
			User:          "deploy",
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderAnsible {
		t.Fatalf("provider name = %q, want %q", got, ProviderAnsible)
	}
}

func TestValidateRequiresExactlyOneAnsibleSource(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Ansible: &AnsibleConfig{}},
		Hosts:    HostsConfig{Pools: map[string]Pool{"web": {}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "provider.ansible must define exactly one inventory source") {
		t.Fatalf("expected ansible source validation error, got %v", err)
	}

	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Ansible: &AnsibleConfig{
			InventoryFile: "inventory.yml",
			Command:       []string{"ansible-inventory", "--list"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {}}},
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "provider.ansible must define exactly one inventory source") {
		t.Fatalf("expected ansible source validation error, got %v", err)
	}
}

func TestValidateRejectsInvalidLatitudeBilling(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Latitude: &LatitudeConfig{
			Project:         "proj-demo",
			Site:            "ASH",
			Plan:            "c2-small-x86",
			OperatingSystem: "ubuntu_24_04_x64_lts",
			SSHKeys:         []string{"ssh-key-1"},
			Billing:         "weekly",
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "provider.latitude.billing must be hourly, monthly, or yearly") {
		t.Fatalf("expected latitude billing error, got %v", err)
	}
}

func TestValidateRequiresLatitudeIPXEScript(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Latitude: &LatitudeConfig{
			Project:         "proj-demo",
			Site:            "ASH",
			Plan:            "c2-small-x86",
			OperatingSystem: "ipxe",
			SSHKeys:         []string{"ssh-key-1"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "provider.latitude.ipxe is required when operating_system is ipxe") {
		t.Fatalf("expected latitude ipxe error, got %v", err)
	}
}

func TestValidateRequiresExoscaleManagedSSHAllowedCIDRs(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Exoscale: &ExoscaleConfig{
			Zone:         "ch-gva-2",
			InstanceType: "standard.medium",
			Template:     "template-id",
			SSHKeys:      []string{"deploy"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "provider.exoscale.ssh_allowed_cidrs is required") {
		t.Fatalf("expected exoscale ssh_allowed_cidrs error, got %v", err)
	}
}

func TestValidateRejectsInvalidExoscalePublicIPAssignment(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Exoscale: &ExoscaleConfig{
			Zone:               "ch-gva-2",
			InstanceType:       "standard.medium",
			Template:           "template-id",
			SSHKeys:            []string{"deploy"},
			SSHAllowedCIDRs:    []string{"203.0.113.0/24"},
			PublicIPAssignment: "ipv4",
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "provider.exoscale.public_ip_assignment must be inet4, dual, or none") {
		t.Fatalf("expected public_ip_assignment error, got %v", err)
	}
}

func TestValidateAcceptsManualProviderWithExplicitHosts(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Manual: &ManualConfig{}},
		Hosts: HostsConfig{Pools: map[string]Pool{
			"web": {User: "deploy", Hosts: []string{"web-1.example.com"}},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderManual {
		t.Fatalf("provider name = %q, want %q", got, ProviderManual)
	}
}

func TestLoadResolvesUserDataFilesRelativeToConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cloud-init.yml"), []byte("#cloud-config\npackages: [htop]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgText := `project: x
registry: ghcr.io/acme/x

environments:
  production:
    provider:
      digitalocean:
        region: nyc3
        size: s-1vcpu-1gb
        image: ubuntu-24-04-x64
        ssh_allowed_cidrs: [203.0.113.0/24]
        user_data_file: cloud-init.yml
    hosts:
      pools:
        web:
          count: 1
          user_data_file: cloud-init.yml
  staging:
    provider:
      exoscale:
        zone: ch-gva-2
        instance_type: standard.medium
        template: template-id
        ssh_keys: [deploy]
        ssh_allowed_cidrs: [203.0.113.0/24]
        user_data_file: cloud-init.yml
    hosts:
      pools:
        web:
          count: 1
  test:
    provider:
      cloudscale:
        zone: rma1
        flavor: flex-4-2
        image: debian-13
        ssh_keys: [ssh-ed25519 AAAA...]
        user_data_file: cloud-init.yml
    hosts:
      pools:
        web:
          count: 1
  baremetal:
    provider:
      latitude:
        project: proj-demo
        site: ASH
        plan: c2-small-x86
        operating_system: ubuntu_24_04_x64_lts
        ssh_keys: [ssh-key-1]
        ssh_allowed_cidrs: [203.0.113.0/24]
        user_data_file: cloud-init.yml
    hosts:
      pools:
        web:
          count: 1

services:
  web:
    image:
      ref: example/web
    pool: web
    scale: 1
`
	path := filepath.Join(dir, DefaultConfigFile)
	if err := os.WriteFile(path, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	env := cfg.Environments["production"]
	want := "#cloud-config\npackages: [htop]\n"
	if env.Provider.DigitalOcean.UserData != want || env.Provider.DigitalOcean.UserDataFile != "" {
		t.Fatalf("provider user data = %q file=%q", env.Provider.DigitalOcean.UserData, env.Provider.DigitalOcean.UserDataFile)
	}
	if env.Hosts.Pools["web"].UserData != want || env.Hosts.Pools["web"].UserDataFile != "" {
		t.Fatalf("pool user data = %+v", env.Hosts.Pools["web"])
	}
	staging := cfg.Environments["staging"]
	if staging.Provider.Exoscale.UserData != want || staging.Provider.Exoscale.UserDataFile != "" {
		t.Fatalf("exoscale provider user data = %q file=%q", staging.Provider.Exoscale.UserData, staging.Provider.Exoscale.UserDataFile)
	}
	testEnv := cfg.Environments["test"]
	if testEnv.Provider.Cloudscale.UserData != want || testEnv.Provider.Cloudscale.UserDataFile != "" {
		t.Fatalf("cloudscale provider user data = %q file=%q", testEnv.Provider.Cloudscale.UserData, testEnv.Provider.Cloudscale.UserDataFile)
	}
	baremetal := cfg.Environments["baremetal"]
	if baremetal.Provider.Latitude.UserData != want || baremetal.Provider.Latitude.UserDataFile != "" {
		t.Fatalf("latitude provider user data = %q file=%q", baremetal.Provider.Latitude.UserData, baremetal.Provider.Latitude.UserDataFile)
	}
}

func TestValidateRequiresProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `environment "production" must define exactly one provider`) {
		t.Fatalf("expected missing provider error, got %v", err)
	}
}

func TestValidateRequiresVultrFields(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Vultr: &VultrConfig{}},
		Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, needle := range []string{
		`provider.vultr.region is required`,
		`provider.vultr.plan is required`,
		`provider.vultr must define exactly one source`,
		`provider.vultr.ssh_allowed_cidrs is required`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateRejectsInvalidVultrUserScheme(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Vultr: &VultrConfig{Region: "ewr", Plan: "vc2-2c-4gb", OSID: 2284, UserScheme: "admin", SSHAllowedCIDRs: []string{"203.0.113.0/24"}}},
		Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `provider.vultr.user_scheme must be root or limited`) {
		t.Fatalf("expected user_scheme validation error, got %v", err)
	}
}

func TestValidateRequiresDigitalOceanFields(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{DigitalOcean: &DigitalOceanConfig{}},
		Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, needle := range []string{
		`provider.digitalocean.region is required`,
		`provider.digitalocean.size is required`,
		`provider.digitalocean.image is required`,
		`provider.digitalocean.ssh_allowed_cidrs is required`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateRequiresLinodeFields(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Linode: &LinodeConfig{}},
		Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, needle := range []string{
		`provider.linode.region is required`,
		`provider.linode.type is required`,
		`provider.linode.image is required`,
		`provider.linode.authorized_keys or provider.linode.authorized_users is required`,
		`provider.linode.ssh_allowed_cidrs is required`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateRejectsBothUserDataAndUserDataFile(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Hetzner: &HetznerConfig{
			Location:        "ash",
			ServerType:      "cpx31",
			Image:           "ubuntu-24.04",
			SSHAllowedCIDRs: []string{"0.0.0.0/0"},
			UserData:        "#cloud-config\n",
			UserDataFile:    "cloud-init.yml",
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{
			"web": {Count: 1, UserData: "#cloud-config\n", UserDataFile: "cloud-init.yml"},
		}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, needle := range []string{
		`provider.hetzner cannot define both user_data and user_data_file`,
		`pool "web" cannot define both user_data and user_data_file`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateRejectsReservedCustomHostLabels(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Manual: &ManualConfig{}},
		Hosts: HostsConfig{
			Labels: map[string]string{"project": "override"},
			Pools: map[string]Pool{
				"web": {
					Hosts:  []string{"web-1.example.com"},
					Labels: map[string]string{"pool": "override"},
				},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, needle := range []string{
		`hosts.labels cannot override reserved Ship label "project"`,
		`pool "web" labels cannot override reserved Ship label "pool"`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateRequiresAWSFields(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{AWS: &AWSConfig{}},
		Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, needle := range []string{
		`provider.aws.region is required`,
		`provider.aws.instance_type is required`,
		`provider.aws.ami is required`,
		`provider.aws.subnet_id is required`,
		`provider.aws.ssh_allowed_cidrs is required`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateRequiresGCPFields(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{GCP: &GCPConfig{}},
		Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, needle := range []string{
		`provider.gcp.project_id is required`,
		`provider.gcp.zone is required`,
		`provider.gcp.machine_type is required`,
		`provider.gcp.image is required`,
		`provider.gcp.ssh_allowed_cidrs is required`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateRequiresAzureFields(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Azure: &AzureConfig{}},
		Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, needle := range []string{
		`provider.azure.subscription_id is required`,
		`provider.azure.resource_group is required`,
		`provider.azure.location is required`,
		`provider.azure.vm_size is required`,
		`provider.azure.image is required`,
		`provider.azure.admin_username is required`,
		`provider.azure.ssh_public_key is required`,
		`provider.azure.subnet_id or virtual_network/subnet is required`,
		`provider.azure.ssh_allowed_cidrs is required`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateRequiresScalewayFields(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Scaleway: &ScalewayConfig{}},
		Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, needle := range []string{
		`provider.scaleway.project_id is required`,
		`provider.scaleway.zone is required`,
		`provider.scaleway.commercial_type is required`,
		`provider.scaleway.image is required`,
		`provider.scaleway.ssh_allowed_cidrs is required`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateRequiresOpenStackFields(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{OpenStack: &OpenStackConfig{}},
		Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, needle := range []string{
		`provider.openstack.flavor is required`,
		`provider.openstack.image is required`,
		`provider.openstack.region is required when compute_url is not set`,
		`provider.openstack.ssh_allowed_cidrs is required`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateRequiresCivoFields(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Civo: &CivoConfig{}},
		Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, needle := range []string{
		`provider.civo.region is required`,
		`provider.civo.size is required`,
		`provider.civo.image is required`,
		`provider.civo.network_id is required`,
		`provider.civo.ssh_allowed_cidrs is required`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateRequiresUpCloudFields(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{UpCloud: &UpCloudConfig{}},
		Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, needle := range []string{
		`provider.upcloud.zone is required`,
		`provider.upcloud.plan is required`,
		`provider.upcloud.template is required`,
		`provider.upcloud.ssh_keys is required`,
		`provider.upcloud.ssh_allowed_cidrs is required`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateRequiresOVHCloudFields(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{OVHCloud: &OVHCloudConfig{}},
		Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, needle := range []string{
		`provider.ovhcloud.service_name is required`,
		`provider.ovhcloud.region is required`,
		`provider.ovhcloud.flavor_id is required`,
		`provider.ovhcloud.image_id is required`,
		`provider.ovhcloud.ssh_key_id is required`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateRequiresOCIFields(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{OCI: &OCIConfig{}},
		Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, needle := range []string{
		`provider.oci.region is required`,
		`provider.oci.compartment_id is required`,
		`provider.oci.availability_domain is required`,
		`provider.oci.shape is required`,
		`provider.oci.image_id is required`,
		`provider.oci.subnet_id is required`,
		`provider.oci.ssh_authorized_keys is required`,
		`provider.oci.network_security_group.vcn_id is required`,
		`provider.oci.ssh_allowed_cidrs is required`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateRejectsInvalidOpenStackInterface(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{OpenStack: &OpenStackConfig{
			Region:          "GRA11",
			Flavor:          "b2-7",
			Image:           "ubuntu-24.04",
			Interface:       "partner",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `provider.openstack.interface must be public, internal, or admin`) {
		t.Fatalf("expected interface validation error, got %v", err)
	}
}

func TestValidateRejectsAWSManagedSecurityGroupWithExplicitID(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{AWS: &AWSConfig{
			Region:          "us-east-1",
			InstanceType:    "t3.medium",
			AMI:             "ami-0123456789abcdef0",
			SubnetID:        "subnet-0123456789abcdef0",
			SecurityGroup:   AWSSecurityGroupConfig{ID: "sg-0123456789abcdef0"},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `security_group.id requires security_group.managed: false`) {
		t.Fatalf("expected security group validation error, got %v", err)
	}
}

func TestValidateRejectsAWSLocationPoolOverride(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{AWS: &AWSConfig{
			Region:          "us-east-1",
			InstanceType:    "t3.medium",
			AMI:             "ami-0123456789abcdef0",
			SubnetID:        "subnet-0123456789abcdef0",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1, Location: "us-west-2"}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `pool "web" location override is not supported`) {
		t.Fatalf("expected location override validation error, got %v", err)
	}
}

func TestValidateRejectsGCPLocationPoolOverride(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{GCP: &GCPConfig{
			ProjectID:       "demo-project",
			Zone:            "us-central1-a",
			MachineType:     "e2-medium",
			Image:           "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1, Location: "us-east1-b"}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `provider.gcp pool "web" location override is not supported`) {
		t.Fatalf("expected gcp location override validation error, got %v", err)
	}
}

func TestValidateRejectsAzureLocationPoolOverride(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Azure: &AzureConfig{
			SubscriptionID:  "sub-123",
			ResourceGroup:   "rg-ship",
			Location:        "eastus",
			VMSize:          "Standard_B2s",
			Image:           "Canonical:ubuntu-24_04-lts:server:latest",
			AdminUsername:   "deploy",
			SSHPublicKey:    "ssh-ed25519 AAAA...",
			SubnetID:        "/subscriptions/sub-123/resourceGroups/rg-ship/providers/Microsoft.Network/virtualNetworks/ship/subnets/default",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1, Location: "westus"}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `provider.azure pool "web" location override is not supported`) {
		t.Fatalf("expected azure location override validation error, got %v", err)
	}
}

func TestValidateRejectsScalewayLocationPoolOverride(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Scaleway: &ScalewayConfig{
			ProjectID:       "project-id",
			Zone:            "fr-par-1",
			CommercialType:  "DEV1-S",
			Image:           "ubuntu_noble",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1, Location: "nl-ams-1"}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `provider.scaleway pool "web" location override is not supported`) {
		t.Fatalf("expected scaleway location override validation error, got %v", err)
	}
}

func TestValidateRejectsOpenStackLocationPoolOverride(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{OpenStack: &OpenStackConfig{
			Region:          "GRA11",
			Flavor:          "b2-7",
			Image:           "ubuntu-24.04",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1, Location: "BHS5"}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `provider.openstack pool "web" location override is not supported`) {
		t.Fatalf("expected openstack location override validation error, got %v", err)
	}
}

func TestValidateRejectsCivoLocationPoolOverride(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Civo: &CivoConfig{
			Region:          "lon1",
			Size:            "g3.small",
			Image:           "ubuntu-noble",
			NetworkID:       "network-1",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1, Location: "nyc1"}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `provider.civo pool "web" location override is not supported`) {
		t.Fatalf("expected civo location override validation error, got %v", err)
	}
}

func TestValidateRejectsUpCloudLocationPoolOverride(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{UpCloud: &UpCloudConfig{
			Zone:            "fi-hel1",
			Plan:            "1xCPU-1GB",
			Template:        "01000000-0000-4000-8000-000030240200",
			SSHKeys:         []string{"ssh-ed25519 AAAA..."},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1, Location: "de-fra1"}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `provider.upcloud pool "web" location override is not supported`) {
		t.Fatalf("expected upcloud location override validation error, got %v", err)
	}
}

func TestValidateRejectsOVHCloudLocationPoolOverride(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{OVHCloud: &OVHCloudConfig{
			ServiceName: "project-id",
			Region:      "GRA11",
			FlavorID:    "b2-7",
			ImageID:     "image-id",
			SSHKeyID:    "ssh-key-id",
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1, Location: "BHS5"}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `provider.ovhcloud pool "web" location override is not supported`) {
		t.Fatalf("expected ovhcloud location override validation error, got %v", err)
	}
}

func TestValidateRejectsOCILocationPoolOverride(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{OCI: &OCIConfig{
			Region:             "us-ashburn-1",
			CompartmentID:      "ocid1.compartment.oc1..aaaa",
			AvailabilityDomain: "Uocm:US-ASHBURN-AD-1",
			Shape:              "VM.Standard.E4.Flex",
			ImageID:            "ocid1.image.oc1..image",
			SubnetID:           "ocid1.subnet.oc1..subnet",
			SSHAuthorizedKeys:  []string{"ssh-ed25519 AAAA..."},
			NetworkSecurityGroup: OCINetworkSecurityGroup{
				VCNID: "ocid1.vcn.oc1..vcn",
			},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1, Location: "Uocm:US-ASHBURN-AD-2"}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `provider.oci pool "web" location override is not supported`) {
		t.Fatalf("expected oci location override validation error, got %v", err)
	}
}

func TestValidateRejectsExoscaleLocationPoolOverride(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Exoscale: &ExoscaleConfig{
			Zone:            "ch-gva-2",
			InstanceType:    "standard.medium",
			Template:        "template-id",
			SSHKeys:         []string{"deploy"},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1, Location: "de-fra-1"}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `provider.exoscale pool "web" location override is not supported`) {
		t.Fatalf("expected exoscale location override validation error, got %v", err)
	}
}

func TestValidateRejectsCloudscaleLocationPoolOverride(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Cloudscale: &CloudscaleConfig{
			Zone:    "rma1",
			Flavor:  "flex-4-2",
			Image:   "debian-13",
			SSHKeys: []string{"ssh-ed25519 AAAA..."},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1, Location: "lpg1"}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `provider.cloudscale pool "web" location override is not supported`) {
		t.Fatalf("expected cloudscale location override validation error, got %v", err)
	}
}

func TestValidateRejectsLatitudeLocationPoolOverride(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Latitude: &LatitudeConfig{
			Project:         "proj-demo",
			Site:            "ASH",
			Plan:            "c2-small-x86",
			OperatingSystem: "ubuntu_24_04_x64_lts",
			SSHKeys:         []string{"ssh-key-1"},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1, Location: "DAL"}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `provider.latitude pool "web" location override is not supported`) {
		t.Fatalf("expected latitude location override validation error, got %v", err)
	}
}

func TestValidateRejectsOpenStackManagedSecurityGroupWithExplicitID(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{OpenStack: &OpenStackConfig{
			Region:          "GRA11",
			Flavor:          "b2-7",
			Image:           "ubuntu-24.04",
			SecurityGroup:   OpenStackSecurityGroupConfig{ID: "sg-id"},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `provider.openstack.security_group.id requires security_group.managed: false`) {
		t.Fatalf("expected security group validation error, got %v", err)
	}
}

func TestValidateRejectsCivoManagedFirewallWithExplicitID(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Civo: &CivoConfig{
			Region:          "lon1",
			Size:            "g3.small",
			Image:           "ubuntu-noble",
			NetworkID:       "network-1",
			Firewall:        CivoFirewallConfig{ID: "firewall-1"},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `provider.civo.firewall.id requires firewall.managed: false`) {
		t.Fatalf("expected firewall validation error, got %v", err)
	}
}

func TestValidateRejectsOpenStackFloatingIPWithoutNetworkOrAddress(t *testing.T) {
	cfg := minimalValidConfig()
	enabled := true
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{OpenStack: &OpenStackConfig{
			Region:          "GRA11",
			Flavor:          "b2-7",
			Image:           "ubuntu-24.04",
			FloatingIP:      OpenStackFloatingIPConfig{Enabled: &enabled},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `provider.openstack.floating_ip.network_id or address is required`) {
		t.Fatalf("expected floating IP validation error, got %v", err)
	}
}

func TestValidateRejectsScalewayManagedSecurityGroupWithExplicitID(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Scaleway: &ScalewayConfig{
			ProjectID:       "project-id",
			Zone:            "fr-par-1",
			CommercialType:  "DEV1-S",
			Image:           "ubuntu_noble",
			SecurityGroup:   ScalewaySecurityGroup{ID: "sg-id"},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `provider.scaleway.security_group.id requires security_group.managed: false`) {
		t.Fatalf("expected security group validation error, got %v", err)
	}
}

func TestValidateRequiresManualProviderHosts(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Manual: &ManualConfig{}},
		Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `manual provider requires pool "web" to define hosts`) {
		t.Fatalf("expected manual host validation error, got %v", err)
	}
}

func TestValidateRequiresExactlyOneVultrSource(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Provider: ProviderConfig{Vultr: &VultrConfig{Region: "ewr", Plan: "vc2-2c-4gb", OSID: 2284, ImageID: "image-abc", SSHAllowedCIDRs: []string{"203.0.113.0/24"}}},
		Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `provider.vultr must define exactly one source`) {
		t.Fatalf("expected source validation error, got %v", err)
	}
}

func TestLoadReportsUnsupportedProvider(t *testing.T) {
	cfgText := strings.Replace(minimalConfigYAML(), "hetzner:", "fly:", 1)
	_, err := loadConfigText(t, cfgText)
	if err == nil || !strings.Contains(err.Error(), `unsupported provider(s): fly`) {
		t.Fatalf("expected unsupported provider error, got %v", err)
	}
}

func TestLoadReportsMultipleProviderBlocks(t *testing.T) {
	cfgText := strings.Replace(minimalConfigYAML(), "      hetzner:", "      digitalocean:\n        region: nyc1\n      hetzner:", 1)
	_, err := loadConfigText(t, cfgText)
	if err == nil || !strings.Contains(err.Error(), `must define exactly one provider`) {
		t.Fatalf("expected multiple provider error, got %v", err)
	}
}

func TestValidateBuildOptionsRequireBuild(t *testing.T) {
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {
				Pool:  "web",
				Scale: 1,
				Image: ImageSpec{
					Ref:       "ghcr.io/acme/x:web",
					Tags:      []string{"latest", ""},
					BuildArgs: map[string]string{"VERSION": "x"},
					Target:    "runtime",
					Builder:   "ship\ncloud",
					Platform:  "linux/amd64",
					Platforms: []string{"linux/amd64", ""},
					Pull:      true,
					NoCache:   true,
					NoCacheFilter: []string{
						"install",
						"",
					},
					CacheFrom: []string{"type=registry,ref=ghcr.io/acme/x:cache"},
					CacheTo:   []string{""},
					Secrets:   []string{"id=npm_token,env=NPM_TOKEN"},
					SSH:       []string{""},
					SBOM:      BuildxFlag("true"),
					Provenance: BuildxFlag(
						"mode=max",
					),
				},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, needle := range []string{
		"image.build_args requires image.build",
		"image.tags requires image.build",
		"image.tags[1] is required",
		"image.target requires image.build",
		"image.builder requires image.build",
		"image.platform requires image.build",
		"image.platforms requires image.build",
		"image.pull requires image.build",
		"image.no_cache requires image.build",
		"image.no_cache_filter requires image.build",
		"image.builder cannot contain newlines",
		"image.platform cannot be combined with image.platforms",
		"image.platforms[1] is required",
		"image.no_cache_filter[1] is required",
		"image.cache_from requires image.build",
		"image.cache_to requires image.build",
		"image.secrets requires image.build",
		"image.ssh requires image.build",
		"image.sbom requires image.build",
		"image.provenance requires image.build",
		"image.cache_to[0] is required",
		"image.ssh[0] is required",
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateBuildpackImageOptions(t *testing.T) {
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}, "worker": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {
				Pool:  "web",
				Scale: 1,
				Image: ImageSpec{
					Build:      ".",
					Dockerfile: "Dockerfile.prod",
					BuildArgs:  map[string]string{"VERSION": "x"},
					Target:     "runtime",
					Builder:    "ship-cloud",
					Platform:   "linux/amd64",
					Platforms:  []string{"linux/arm64"},
					Pull:       true,
					NoCache:    true,
					NoCacheFilter: []string{
						"install",
					},
					CacheFrom:  []string{"type=registry,ref=ghcr.io/acme/x:cache"},
					CacheTo:    []string{"type=registry,ref=ghcr.io/acme/x:cache,mode=max"},
					Secrets:    []string{"id=npm_token,env=NPM_TOKEN"},
					SSH:        []string{"default"},
					SBOM:       BuildxFlag("true"),
					Provenance: BuildxFlag("mode=max"),
					Buildpack: BuildpackConfig{
						Builder:    "paketo\nbad",
						Buildpacks: []string{"paketo-buildpacks/nodejs", ""},
						Env: map[string]string{
							"BAD KEY": "x",
							"GOOD":    "line\nbad",
						},
						Descriptor: "project\nbad.toml",
						PullPolicy: "sometimes",
					},
				},
			},
			"worker": {
				Pool:  "worker",
				Scale: 1,
				Image: ImageSpec{
					Buildpack: BuildpackConfig{Publish: true},
				},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, needle := range []string{
		`service "web" image.buildpack builder cannot contain newlines`,
		`service "web" image.buildpack descriptor cannot contain newlines`,
		`service "web" image.buildpack pull_policy must be one of always, never, or if-not-present`,
		`service "web" image.buildpack buildpacks[1] is required`,
		`service "web" image.buildpack env["BAD KEY"] key cannot contain whitespace, newlines, or '='`,
		`service "web" image.buildpack env["GOOD"] value cannot contain newlines`,
		`service "web" image.dockerfile cannot be combined with image.buildpack`,
		`service "web" image.build_args cannot be combined with image.buildpack; use image.buildpack.env`,
		`service "web" image.target cannot be combined with image.buildpack`,
		`service "web" image.builder cannot be combined with image.buildpack; use image.buildpack.builder`,
		`service "web" image.platform cannot be combined with image.buildpack`,
		`service "web" image.platforms cannot be combined with image.buildpack`,
		`service "web" image.pull cannot be combined with image.buildpack; use image.buildpack.pull_policy`,
		`service "web" image.no_cache cannot be combined with image.buildpack`,
		`service "web" image.no_cache_filter cannot be combined with image.buildpack`,
		`service "web" image.cache_from cannot be combined with image.buildpack`,
		`service "web" image.cache_to cannot be combined with image.buildpack`,
		`service "web" image.secrets cannot be combined with image.buildpack`,
		`service "web" image.ssh cannot be combined with image.buildpack`,
		`service "web" image.sbom cannot be combined with image.buildpack`,
		`service "web" image.provenance cannot be combined with image.buildpack`,
		`service "worker" image.buildpack requires image.build`,
		`service "worker" image.build or image.ref is required`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateServiceSchedules(t *testing.T) {
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {
				Pool:  "web",
				Scale: 1,
				Image: ImageSpec{Ref: "ghcr.io/acme/x:web"},
				Schedules: map[string]Schedule{
					"bad/name": {
						Cron:           "* * *",
						Command:        "bin/task\nrm -rf /",
						Replica:        2,
						TimeoutSeconds: -1,
					},
				},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected schedule validation errors")
	}
	for _, needle := range []string{
		`service "web" schedule "bad/name" name must contain only letters`,
		`service "web" schedule "bad/name" cron must have exactly five fields`,
		`service "web" schedule "bad/name" command cannot contain newlines`,
		`service "web" schedule "bad/name" replica 2 exceeds service scale 1`,
		`service "web" schedule "bad/name" timeout_seconds cannot be negative`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateServiceReleaseCommand(t *testing.T) {
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {
				Pool:    "web",
				Scale:   1,
				Image:   ImageSpec{Ref: "ghcr.io/acme/x:web"},
				Release: ReleaseCommand{Command: "bin/migrate\nbad", Replica: 2, TimeoutSeconds: -1},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected release validation errors")
	}
	for _, needle := range []string{
		`service "web" release.command cannot contain newlines`,
		`service "web" release.replica 2 exceeds service scale 1`,
		`service "web" release.timeout_seconds cannot be negative`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateContainerLabels(t *testing.T) {
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}, "data": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {
				Pool:  "web",
				Scale: 1,
				Image: ImageSpec{Ref: "ghcr.io/acme/x:web"},
				Labels: map[string]string{
					"com.example.team": "platform",
					"service":          "override",
					"bad key":          "value",
					"line":             "one\ntwo",
				},
				NetworkAliases: []string{"web", "bad alias"},
			},
		},
		Accessories: map[string]Accessory{
			"postgres": {
				Image: "postgres:17",
				Pool:  "data",
				Labels: map[string]string{
					"environment": "override",
					"":            "empty",
				},
				NetworkAliases: []string{""},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected container label validation errors")
	}
	for _, needle := range []string{
		`service "web" labels cannot override reserved Ship label "service"`,
		`service "web" labels["bad key"] key cannot contain whitespace, newlines, or '='`,
		`service "web" labels["line"] value cannot contain newlines`,
		`service "web" network_aliases[1] must contain only letters`,
		`accessory "postgres" labels cannot override reserved Ship label "environment"`,
		`accessory "postgres" labels[""] key is required`,
		`accessory "postgres" network_aliases[0] is required`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateServiceRollingHealthRetrySettings(t *testing.T) {
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {
				Pool:  "web",
				Scale: 1,
				Image: ImageSpec{Ref: "ghcr.io/acme/x:web"},
				Rolling: Rolling{
					CanaryReplicas:        2,
					CanaryPauseSeconds:    -1,
					HealthRetries:         -1,
					HealthIntervalSeconds: -2,
				},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected rolling health retry validation errors")
	}
	for _, needle := range []string{
		`service "web" rolling.canary_pause_seconds cannot be negative`,
		`service "web" rolling.canary_replicas 2 exceeds service scale 1`,
		`service "web" rolling.health_retries cannot be negative`,
		`service "web" rolling.health_interval_seconds cannot be negative`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateServiceIngressHealthSettings(t *testing.T) {
	enabled := true
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {
				Pool:  "web",
				Scale: 1,
				Image: ImageSpec{Ref: "ghcr.io/acme/x:web"},
				Ingress: &Ingress{
					Domains: []string{"example.com"},
					Health: IngressHealth{
						Enabled:                    &enabled,
						Path:                       "ready",
						IntervalSeconds:            -1,
						TimeoutSeconds:             -1,
						Passes:                     -1,
						Fails:                      -1,
						TryDurationSeconds:         -1,
						PassiveFailDurationSeconds: -1,
						PassiveMaxFails:            -1,
						UnhealthyStatus:            []string{"5xx", "bad status"},
					},
				},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected ingress health validation errors")
	}
	for _, needle := range []string{
		`service "web" ingress.health.interval_seconds cannot be negative`,
		`service "web" ingress.health.timeout_seconds cannot be negative`,
		`service "web" ingress.health.passes cannot be negative`,
		`service "web" ingress.health.fails cannot be negative`,
		`service "web" ingress.health.try_duration_seconds cannot be negative`,
		`service "web" ingress.health.passive_fail_duration_seconds cannot be negative`,
		`service "web" ingress.health.passive_max_fails cannot be negative`,
		`service "web" ingress.health.path must start with /`,
		`service "web" ingress.health.unhealthy_status[1] cannot contain whitespace`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateRejectsIngressHealthPathWithWhitespace(t *testing.T) {
	cfg := minimalValidConfig()
	svc := cfg.Services["web"]
	svc.Ingress = &Ingress{
		Domains: []string{"example.com"},
		Health:  IngressHealth{Path: "/up\nevil directive"},
	}
	cfg.Services["web"] = svc

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `service "web" ingress.health.path cannot contain whitespace`) {
		t.Fatalf("expected ingress health path whitespace error, got %v", err)
	}
}

func TestValidateRejectsServiceHealthHTTPWithWhitespace(t *testing.T) {
	cfg := minimalValidConfig()
	svc := cfg.Services["web"]
	svc.Health.HTTP = "/up\nevil directive"
	cfg.Services["web"] = svc

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `service "web" health.http cannot contain whitespace`) {
		t.Fatalf("expected service health.http whitespace error, got %v", err)
	}
}

func TestValidateServiceIngressRedirects(t *testing.T) {
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {
				Pool:  "web",
				Scale: 1,
				Image: ImageSpec{Ref: "ghcr.io/acme/x:web"},
				Ingress: &Ingress{
					Domains: []string{"example.com", "bad domain"},
					Redirects: []IngressRedirect{
						{To: "https://example.com"},
						{From: []string{"", "bad domain"}, To: "example.com", Code: 309},
						{From: []string{"www.example.com"}, To: "https://example.com/bad target"},
						{From: []string{"example.com"}, To: "https://www.example.com"},
					},
				},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected ingress redirect validation errors")
	}
	for _, needle := range []string{
		`service "web" ingress.domains[1] cannot contain whitespace`,
		`service "web" ingress.redirects[0].from is required`,
		`service "web" ingress.redirects[1].from[0] is required`,
		`service "web" ingress.redirects[1].from[1] cannot contain whitespace`,
		`service "web" ingress.redirects[1].to must start with http:// or https://`,
		`service "web" ingress.redirects[1].code must be one of 301, 302, 303, 307, or 308`,
		`service "web" ingress.redirects[2].to cannot contain whitespace`,
		`service "web" ingress.redirects[3].from[0] conflicts with proxied domain "example.com"`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateExplicitIngressHealthRequiresPath(t *testing.T) {
	enabled := true
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {
				Pool:    "web",
				Scale:   1,
				Image:   ImageSpec{Ref: "ghcr.io/acme/x:web"},
				Ingress: &Ingress{Domains: []string{"example.com"}, Health: IngressHealth{Enabled: &enabled}},
			},
		},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `service "web" ingress.health.path or health.http is required when ingress.health.enabled is true`) {
		t.Fatalf("expected explicit ingress health path error, got %v", err)
	}
}

func TestResolveEnvironmentAppliesRootLoggingDefaults(t *testing.T) {
	cfg, err := loadConfigText(t, `project: x
registry: ghcr.io/acme/x

logging:
  driver: json-file
  options:
    max-size: 10m
    max-file: "3"

environments:
  production:
    provider:
      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    hosts:
      pools:
        web:
          count: 1

services:
  web:
    image:
      ref: ghcr.io/acme/x:web
    pool: web
    scale: 1
    logging:
      options:
        max-file: "5"
        tag: "{{.Name}}"
`)
	if err != nil {
		t.Fatal(err)
	}
	resolved, _, err := cfg.ResolveEnvironment("production")
	if err != nil {
		t.Fatal(err)
	}
	logging := resolved.Services["web"].Logging
	if logging.Driver != "json-file" || logging.Options["max-size"] != "10m" || logging.Options["max-file"] != "5" || logging.Options["tag"] != "{{.Name}}" {
		t.Fatalf("logging = %+v", logging)
	}
}

func TestResolveEnvironmentAppliesDockerNetworkOverride(t *testing.T) {
	disabled := false
	cfg, err := loadConfigText(t, `project: x
registry: ghcr.io/acme/x

docker:
  network:
    name: root-net
    driver: bridge

environments:
  production:
    provider:
      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    docker:
      network:
        name: production-net
        driver: overlay
    hosts:
      pools:
        web:
          count: 1
  staging:
    provider:
      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    hosts:
      pools:
        web:
          count: 1

services:
  web:
    image:
      ref: ghcr.io/acme/x:web
    pool: web
    scale: 1
`)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Environments["staging"] = Environment{
		Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
		Docker:   DockerConfig{Network: DockerNetworkConfig{Enabled: &disabled}},
		Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	production, env, err := cfg.ResolveEnvironment("production")
	if err != nil {
		t.Fatal(err)
	}
	if production.Docker.Network.Name != "production-net" || production.Docker.Network.Driver != "overlay" || env.Docker.Network.Name != "production-net" {
		t.Fatalf("production docker = root %+v env %+v", production.Docker, env.Docker)
	}
	staging, _, err := cfg.ResolveEnvironment("staging")
	if err != nil {
		t.Fatal(err)
	}
	if staging.Docker.Network.Enabled == nil || *staging.Docker.Network.Enabled {
		t.Fatalf("staging docker = %+v", staging.Docker)
	}
}

func TestValidateLoggingConfig(t *testing.T) {
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Logging: LoggingConfig{
			Driver:  "json file",
			Options: map[string]string{"bad key": "value", "tag": "line\nbreak"},
		},
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {
				Pool:    "web",
				Scale:   1,
				Image:   ImageSpec{Ref: "ghcr.io/acme/x:web"},
				Logging: LoggingConfig{Options: map[string]string{"": "value"}},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected logging validation errors")
	}
	for _, needle := range []string{
		`root logging.driver cannot contain whitespace`,
		`root logging.options["bad key"] key cannot contain whitespace or '='`,
		`root logging.options["tag"] value cannot contain newlines`,
		`service "web" logging.options[""] key is required`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateDockerNetworkConfig(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Docker.Network.Name = "bridge"
	cfg.Docker.Network.Driver = "bad/driver"
	env := cfg.Environments["production"]
	env.Docker.Network.Name = "bad\nnetwork"
	cfg.Environments["production"] = env
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected docker network validation errors")
	}
	for _, needle := range []string{
		"root docker.network.name cannot be bridge, host, or none",
		"root docker.network.driver must contain only letters",
		`environment "production" docker.network.name must contain only letters`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateCaddyVolumes(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Ingress.Caddy.DataVolume = "ship-caddy-data"
	cfg.Ingress.Caddy.ConfigVolume = "ship-caddy-config"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	cfg.Ingress.Caddy.DataVolume = "bad volume"
	cfg.Ingress.Caddy.ConfigVolume = "bad:volume"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected caddy volume validation errors")
	}
	for _, needle := range []string{
		"root ingress.caddy.data_volume cannot contain whitespace, newlines, or ':'",
		"root ingress.caddy.config_volume cannot contain whitespace, newlines, or ':'",
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateRestartPolicies(t *testing.T) {
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {
				Pool:          "web",
				Scale:         1,
				Image:         ImageSpec{Ref: "ghcr.io/acme/x:web"},
				RestartPolicy: "on-failure:3",
			},
		},
		Accessories: map[string]Accessory{
			"cache": {
				Image:         "redis:7",
				Pool:          "web",
				RestartPolicy: "always",
			},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestValidateRejectsInvalidRestartPolicies(t *testing.T) {
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {
				Pool:          "web",
				Scale:         1,
				Image:         ImageSpec{Ref: "ghcr.io/acme/x:web"},
				RestartPolicy: "unless stopped",
			},
			"worker": {
				Pool:          "web",
				Scale:         1,
				Image:         ImageSpec{Ref: "ghcr.io/acme/x:worker"},
				RestartPolicy: "on-failure:0",
			},
		},
		Accessories: map[string]Accessory{
			"cache": {
				Image:         "redis:7",
				Pool:          "web",
				RestartPolicy: "sometimes",
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected restart policy validation errors")
	}
	for _, needle := range []string{
		`service "web" restart_policy cannot contain whitespace`,
		`service "worker" restart_policy on-failure retry count must be a positive integer`,
		`accessory "cache" restart_policy must be one of no, always, unless-stopped, on-failure, or on-failure:N`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestLoadBuildAttestationFlags(t *testing.T) {
	cfg, err := loadConfigText(t, `project: x
registry: ghcr.io/acme/x

environments:
  production:
    provider:
      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    hosts:
      pools:
        web:
          count: 1

services:
  web:
    image:
      build: .
      sbom: true
      provenance: mode=max
    pool: web
    scale: 1
`)
	if err != nil {
		t.Fatal(err)
	}
	image := cfg.Services["web"].Image
	if image.SBOM.Value() != "true" || image.Provenance.Value() != "mode=max" {
		t.Fatalf("attestation flags = sbom:%q provenance:%q", image.SBOM.Value(), image.Provenance.Value())
	}
}

func TestValidateServiceVolumes(t *testing.T) {
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {
				Pool:    "web",
				Scale:   1,
				Image:   ImageSpec{Ref: "ghcr.io/acme/x:web"},
				Volumes: []string{"", "uploads", "data:/app/data\nbad"},
				Publish: []string{"", "127.0.0.1:8080:80\nbad"},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected volume validation errors")
	}
	for _, needle := range []string{
		`service "web" volumes[0] is required`,
		`service "web" volumes[1] must use source:target syntax`,
		`service "web" volumes[2] cannot contain newlines`,
		`service "web" publish[0] is required`,
		`service "web" publish[1] cannot contain newlines`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateServiceResources(t *testing.T) {
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {
				Pool:  "web",
				Scale: 1,
				Image: ImageSpec{Ref: "ghcr.io/acme/x:web"},
				Resources: ResourceConfig{
					CPUs:       "1\nbad",
					Memory:     "512m\nbad",
					CPUShares:  -1,
					PIDsLimit:  -2,
					CPUSetCPUs: "0,1\nbad",
				},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected resource validation errors")
	}
	for _, needle := range []string{
		`service "web" resources.cpus cannot contain newlines`,
		`service "web" resources.memory cannot contain newlines`,
		`service "web" resources.cpuset_cpus cannot contain newlines`,
		`service "web" resources.cpu_shares cannot be negative`,
		`service "web" resources.pids_limit cannot be negative`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateRuntimeConfig(t *testing.T) {
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {
				Pool:  "web",
				Scale: 1,
				Image: ImageSpec{Ref: "ghcr.io/acme/x:web"},
				Runtime: RuntimeConfig{
					CapAdd:             []string{""},
					CapDrop:            []string{"NET_RAW\nbad"},
					SecurityOpt:        []string{"no-new-privileges:true"},
					User:               "1000\nbad",
					Workdir:            "/app\nbad",
					Hostname:           "web\nbad",
					Entrypoint:         "/entry\nbad",
					IPC:                "host\nbad",
					PID:                "host\nbad",
					CgroupNS:           "host\nbad",
					StopSignal:         "SIGTERM\nbad",
					StopTimeoutSeconds: -1,
					ShmSize:            "1g\nbad",
					GPUs:               "all\nbad",
					NoHealthcheck:      true,
					HealthCMD:          "curl\nbad",
					HealthInterval:     "10s\nbad",
					HealthTimeout:      "3s\nbad",
					HealthStartPeriod:  "20s\nbad",
					HealthRetries:      -1,
					GroupAdd:           []string{""},
					Sysctls:            map[string]string{"bad key": "1", "vm.max_map_count": ""},
					Ulimits:            []string{"nofile=1024:2048"},
					Mounts:             []string{"type=bind\nbad"},
					AddHosts:           []string{""},
					DNS:                []string{"1.1.1.1\nbad"},
					Devices:            []string{""},
					Tmpfs:              []string{"/tmp\nbad"},
				},
			},
		},
		Accessories: map[string]Accessory{
			"postgres": {
				Image: "postgres:17",
				Pool:  "web",
				Resources: ResourceConfig{
					CPUs:      "1\nbad",
					CPUShares: -1,
					PIDsLimit: -1,
				},
				Publish: []string{""},
				Runtime: RuntimeConfig{
					Ulimits: []string{""},
				},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected runtime validation errors")
	}
	for _, needle := range []string{
		`service "web" runtime cap_add[0] is required`,
		`service "web" runtime cap_drop[0] cannot contain newlines`,
		`service "web" runtime user cannot contain newlines`,
		`service "web" runtime workdir cannot contain newlines`,
		`service "web" runtime hostname cannot contain newlines`,
		`service "web" runtime entrypoint cannot contain newlines`,
		`service "web" runtime ipc cannot contain newlines`,
		`service "web" runtime pid cannot contain newlines`,
		`service "web" runtime cgroupns cannot contain newlines`,
		`service "web" runtime stop_signal cannot contain newlines`,
		`service "web" runtime stop_timeout_seconds cannot be negative`,
		`service "web" runtime shm_size cannot contain newlines`,
		`service "web" runtime gpus cannot contain newlines`,
		`service "web" runtime health_cmd cannot contain newlines`,
		`service "web" runtime health_interval cannot contain newlines`,
		`service "web" runtime health_timeout cannot contain newlines`,
		`service "web" runtime health_start_period cannot contain newlines`,
		`service "web" runtime health_retries cannot be negative`,
		`service "web" runtime no_healthcheck cannot be combined with explicit healthcheck settings`,
		`service "web" runtime group_add[0] is required`,
		`service "web" runtime mounts[0] cannot contain newlines`,
		`service "web" runtime add_hosts[0] is required`,
		`service "web" runtime dns[0] cannot contain newlines`,
		`service "web" runtime devices[0] is required`,
		`service "web" runtime tmpfs[0] cannot contain newlines`,
		`service "web" runtime sysctls["bad key"] key cannot contain whitespace, newlines, or '='`,
		`service "web" runtime sysctls["vm.max_map_count"] value is required`,
		`accessory "postgres" publish[0] is required`,
		`accessory "postgres" resources.cpus cannot contain newlines`,
		`accessory "postgres" resources.cpu_shares cannot be negative`,
		`accessory "postgres" resources.pids_limit cannot be negative`,
		`accessory "postgres" runtime ulimits[0] is required`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestResolveEnvironmentMergesHooks(t *testing.T) {
	cfg, err := loadConfigText(t, `project: x
registry: ghcr.io/acme/x

hooks:
  pre_deploy:
    - ./scripts/root-pre
  deploy_failed:
    - command: ./scripts/root-failed
      timeout_seconds: 5
      env:
        ROOT_HOOK: "1"
notifications:
  webhooks:
    - url: https://hooks.example/root
      events: [deploy:succeeded]

environments:
  production:
    provider:
      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    hosts:
      pools:
        web:
          count: 1
    hooks:
      pre_deploy:
        - ./scripts/env-pre
      post_deploy:
        - command: ./scripts/env-post
          env:
            ENV_HOOK: "1"
    notifications:
      webhooks:
        - url_env: SHIP_NOTIFY_WEBHOOK
          events: [deploy:failed, rollback:*]
          headers:
            X-Env: production

services:
  web:
    image:
      ref: ghcr.io/acme/x:web
    pool: web
    scale: 1
`)
	if err != nil {
		t.Fatal(err)
	}
	resolved, env, err := cfg.ResolveEnvironment("production")
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Hooks.PreDeploy) != 2 || resolved.Hooks.PreDeploy[0].Command != "./scripts/root-pre" || resolved.Hooks.PreDeploy[1].Command != "./scripts/env-pre" {
		t.Fatalf("pre deploy hooks = %+v", resolved.Hooks.PreDeploy)
	}
	if len(resolved.Hooks.DeployFailed) != 1 || resolved.Hooks.DeployFailed[0].TimeoutSeconds != 5 || resolved.Hooks.DeployFailed[0].Env["ROOT_HOOK"] != "1" {
		t.Fatalf("deploy failed hooks = %+v", resolved.Hooks.DeployFailed)
	}
	if len(env.Hooks.PostDeploy) != 1 || env.Hooks.PostDeploy[0].Env["ENV_HOOK"] != "1" {
		t.Fatalf("environment hooks = %+v", env.Hooks)
	}
	if len(resolved.Notifications.Webhooks) != 2 {
		t.Fatalf("notifications = %+v", resolved.Notifications.Webhooks)
	}
	if resolved.Notifications.Webhooks[0].URL != "https://hooks.example/root" || resolved.Notifications.Webhooks[1].URLEnv != "SHIP_NOTIFY_WEBHOOK" {
		t.Fatalf("notification merge order = %+v", resolved.Notifications.Webhooks)
	}
	if len(env.Notifications.Webhooks) != 2 || env.Notifications.Webhooks[1].Headers["X-Env"] != "production" {
		t.Fatalf("environment notifications = %+v", env.Notifications.Webhooks)
	}
}

func TestValidateHooks(t *testing.T) {
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Hooks: Hooks{
			PreDeploy: []HookCommand{{
				Command:        "echo ok\nbad",
				TimeoutSeconds: -1,
				Env:            map[string]string{"1BAD": "x"},
			}},
			PostDeploy: []HookCommand{{}},
		},
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {Pool: "web", Scale: 1, Image: ImageSpec{Ref: "ghcr.io/acme/x:web"}},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected hook validation errors")
	}
	for _, needle := range []string{
		`root hooks.pre_deploy[0].command cannot contain newlines`,
		`root hooks.pre_deploy[0].timeout_seconds cannot be negative`,
		`root hooks.pre_deploy[0].env "1BAD" must be a valid environment variable name`,
		`root hooks.post_deploy[0].command is required`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateNotifications(t *testing.T) {
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Notifications: Notifications{
			Webhooks: []WebhookNotification{
				{
					URL:            "https://example.test/hook\nbad",
					URLEnv:         "1BAD",
					Events:         []string{""},
					Headers:        map[string]string{"": "value", "X-Bad": "bad\nvalue"},
					TimeoutSeconds: -1,
				},
				{URL: "ftp://example.test/hook"},
				{URL: "not-a-url"},
				{},
			},
		},
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {Pool: "web", Scale: 1, Image: ImageSpec{Ref: "ghcr.io/acme/x:web"}},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected notification validation errors")
	}
	for _, needle := range []string{
		`root notifications.webhooks[0] requires exactly one of url or url_env`,
		`root notifications.webhooks[0].url cannot contain newlines`,
		`root notifications.webhooks[0].url_env "1BAD" must be a valid environment variable name`,
		`root notifications.webhooks[0].timeout_seconds cannot be negative`,
		`root notifications.webhooks[0].headers key is required`,
		`root notifications.webhooks[0].headers "X-Bad" cannot contain newlines`,
		`root notifications.webhooks[0].events[0] is required`,
		`root notifications.webhooks[1].url scheme must be http or https`,
		`root notifications.webhooks[2].url must be an absolute http or https URL`,
		`root notifications.webhooks[3] requires exactly one of url or url_env`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateAccessoryBackupRequiredRequiresCommand(t *testing.T) {
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"data": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {Pool: "data", Scale: 0, Image: ImageSpec{Ref: "example/web"}},
		},
		Accessories: map[string]Accessory{
			"redis": {
				Image:  "redis:7",
				Pool:   "data",
				Backup: BackupSpec{Required: true},
			},
		},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `accessory "redis" requires backup.command`) {
		t.Fatalf("expected accessory backup command validation error, got %v", err)
	}
}

func TestValidateAccessoryBackupSchedule(t *testing.T) {
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"data": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {Pool: "data", Scale: 0, Image: ImageSpec{Ref: "example/web"}},
		},
		Accessories: map[string]Accessory{
			"postgres": {
				Image: "postgres:17",
				Pool:  "data",
				Backup: BackupSpec{
					Command:              "pg_dumpall\nrm -rf /",
					ExportCommand:        "rclone copy\nbad",
					ExportTimeoutSeconds: -1,
					Schedule: BackupSchedule{
						Cron:           "* * *",
						TimeoutSeconds: -1,
					},
				},
			},
			"redis": {
				Image:  "redis:7",
				Pool:   "data",
				Backup: BackupSpec{ExportCommand: "aws s3 cp \"$SHIP_BACKUP_ARTIFACT\" s3://bucket/redis.backup"},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected accessory backup schedule validation errors")
	}
	for _, needle := range []string{
		`accessory "postgres" backup.schedule.cron must have exactly five fields`,
		`accessory "postgres" backup.export_command cannot contain newlines`,
		`accessory "postgres" backup.export_timeout_seconds cannot be negative`,
		`accessory "postgres" backup.command cannot contain newlines when backup.schedule is set`,
		`accessory "postgres" backup.schedule.timeout_seconds cannot be negative`,
		`accessory "redis" backup.export_command requires backup.command`,
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestResolveEnvironmentAppliesServiceAccessoryAndSecretOverrides(t *testing.T) {
	cfg, err := loadConfigText(t, `project: x
registry: ghcr.io/acme/x

environments:
  staging:
    provider:
      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    hosts:
      pools:
        web:
          count: 1
        data:
          count: 1
    secrets: [STAGING_SHARED]
    services:
      web:
        image:
          ref: example/web:staging
        pool: web
        scale: 1
        secrets: [STAGING_WEB]
    accessories:
      redis:
        image: redis:7
        pool: data
        secrets: [STAGING_REDIS]

services:
  web:
    image:
      ref: example/web
    pool: web
    scale: 3
    secrets: [WEB_SECRET]

accessories:
  redis:
    image: redis:7
    pool: data

secrets: [GLOBAL_SECRET]
`)
	if err != nil {
		t.Fatal(err)
	}
	resolved, env, err := cfg.ResolveEnvironment("staging")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Services["web"].Scale != 1 || resolved.Services["web"].Image.Ref != "example/web:staging" {
		t.Fatalf("resolved web = %+v", resolved.Services["web"])
	}
	if strings.Join(resolved.Services["web"].Secrets, ",") != "STAGING_WEB" {
		t.Fatalf("service secrets = %+v", resolved.Services["web"].Secrets)
	}
	if strings.Join(resolved.Secrets, ",") != "GLOBAL_SECRET,STAGING_SHARED" {
		t.Fatalf("shared secrets = %+v", resolved.Secrets)
	}
	if env.Services != nil || env.Accessories != nil {
		t.Fatalf("resolved env retained override maps: %+v", env)
	}
}

func TestResolveEnvironmentDeepMergesPartialServiceOverrides(t *testing.T) {
	cfg, err := loadConfigText(t, `project: x
registry: ghcr.io/acme/x

environments:
  staging:
    provider:
      manual: {}
    hosts:
      pools:
        web:
          hosts: [10.0.0.1]
    services:
      web:
        ingress:
          domains: [staging.example.com]
        scale: 1

services:
  web:
    image:
      ref: example/web
    pool: web
    scale: 3
    ports: [3000]
`)
	if err != nil {
		t.Fatal(err)
	}
	resolved, _, err := cfg.ResolveEnvironment("staging")
	if err != nil {
		t.Fatal(err)
	}
	web := resolved.Services["web"]
	if web.Image.Ref != "example/web" || web.Pool != "web" || web.Scale != 1 {
		t.Fatalf("merged web = %+v", web)
	}
	if web.Ingress == nil || web.Ingress.Domains[0] != "staging.example.com" {
		t.Fatalf("merged ingress = %+v", web.Ingress)
	}
}

func TestResolveEnvironmentMergesServiceAndAccessoryEnvOverrides(t *testing.T) {
	cfg, err := loadConfigText(t, `project: x
registry: ghcr.io/acme/x

environments:
  staging:
    provider:
      manual: {}
    hosts:
      pools:
        web:
          hosts: [10.0.0.1]
        data:
          hosts: [10.0.0.2]
    services:
      web:
        env:
          - SHARED=staging
          - STAGING_ONLY=1
    accessories:
      redis:
        env:
          - REDIS_SHARED=staging
          - REDIS_STAGING_ONLY=1

services:
  web:
    image:
      ref: example/web
    pool: web
    scale: 1
    env:
      - ROOT_ONLY=1
      - SHARED=root
      - HOST_PASSTHROUGH

accessories:
  redis:
    image: redis:7
    pool: data
    env:
      - REDIS_ROOT_ONLY=1
      - REDIS_SHARED=root
`)
	if err != nil {
		t.Fatal(err)
	}
	resolved, _, err := cfg.ResolveEnvironment("staging")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(resolved.Services["web"].Env, ","); got != "ROOT_ONLY=1,SHARED=staging,HOST_PASSTHROUGH,STAGING_ONLY=1" {
		t.Fatalf("service env = %q", got)
	}
	if got := strings.Join(resolved.Accessories["redis"].Env, ","); got != "REDIS_ROOT_ONLY=1,REDIS_SHARED=staging,REDIS_STAGING_ONLY=1" {
		t.Fatalf("accessory env = %q", got)
	}
}

func TestResolveEnvironmentAppliesRuntimeDefaults(t *testing.T) {
	cfg, err := loadConfigText(t, `project: x
registry: ghcr.io/acme/x

runtime:
  read_only: true
  init: true
  user: "1000:1000"
  stop_timeout_seconds: 20
  security_opt:
    - no-new-privileges:true
  sysctls:
    net.core.somaxconn: "1024"
  tmpfs:
    - /tmp:rw,size=64m

environments:
  production:
    runtime:
      user: "2000:2000"
      dns:
        - 1.1.1.1
      sysctls:
        net.core.somaxconn: "2048"
    provider:
      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    hosts:
      pools:
        web:
          count: 1
        data:
          count: 1

services:
  web:
    image:
      ref: example/web
    pool: web
    scale: 2
    runtime:
      user: "3000:3000"
      cap_drop: [NET_RAW]
      tmpfs:
        - /run:rw,size=32m

accessories:
  redis:
    image: redis:7
    pool: data
    runtime:
      stop_timeout_seconds: 45
      ulimits:
        - nofile=262144:262144
`)
	if err != nil {
		t.Fatal(err)
	}
	resolved, _, err := cfg.ResolveEnvironment("production")
	if err != nil {
		t.Fatal(err)
	}
	webRuntime := resolved.Services["web"].Runtime
	if !webRuntime.ReadOnly || !webRuntime.Init {
		t.Fatalf("service runtime did not inherit boolean defaults: %+v", webRuntime)
	}
	if webRuntime.User != "3000:3000" {
		t.Fatalf("service runtime user = %q", webRuntime.User)
	}
	if webRuntime.StopTimeoutSeconds != 20 {
		t.Fatalf("service runtime stop timeout = %d", webRuntime.StopTimeoutSeconds)
	}
	if strings.Join(webRuntime.SecurityOpt, ",") != "no-new-privileges:true" {
		t.Fatalf("service runtime security_opt = %+v", webRuntime.SecurityOpt)
	}
	if strings.Join(webRuntime.DNS, ",") != "1.1.1.1" {
		t.Fatalf("service runtime dns = %+v", webRuntime.DNS)
	}
	if strings.Join(webRuntime.CapDrop, ",") != "NET_RAW" {
		t.Fatalf("service runtime cap_drop = %+v", webRuntime.CapDrop)
	}
	if strings.Join(webRuntime.Tmpfs, ",") != "/tmp:rw,size=64m,/run:rw,size=32m" {
		t.Fatalf("service runtime tmpfs = %+v", webRuntime.Tmpfs)
	}
	if webRuntime.Sysctls["net.core.somaxconn"] != "2048" {
		t.Fatalf("service runtime sysctls = %+v", webRuntime.Sysctls)
	}

	redisRuntime := resolved.Accessories["redis"].Runtime
	if !redisRuntime.ReadOnly || !redisRuntime.Init {
		t.Fatalf("accessory runtime did not inherit boolean defaults: %+v", redisRuntime)
	}
	if redisRuntime.User != "2000:2000" {
		t.Fatalf("accessory runtime user = %q", redisRuntime.User)
	}
	if redisRuntime.StopTimeoutSeconds != 45 {
		t.Fatalf("accessory runtime stop timeout = %d", redisRuntime.StopTimeoutSeconds)
	}
	if strings.Join(redisRuntime.Ulimits, ",") != "nofile=262144:262144" {
		t.Fatalf("accessory runtime ulimits = %+v", redisRuntime.Ulimits)
	}
	if redisRuntime.Sysctls["net.core.somaxconn"] != "2048" {
		t.Fatalf("accessory runtime sysctls = %+v", redisRuntime.Sysctls)
	}
}

func minimalValidConfig() *Config {
	return &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {Pool: "web", Scale: 1, Image: ImageSpec{Ref: "example/web"}},
		},
	}
}

func loadConfigText(t *testing.T, text string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultConfigFile)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func boolPtr(value bool) *bool {
	return &value
}

func minimalConfigYAML() string {
	return `project: x
registry: ghcr.io/acme/x

environments:
  production:
    provider:
      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    hosts:
      pools:
        web:
          count: 1

services:
  web:
    image:
      ref: example/web
    pool: web
    scale: 1
`
}
