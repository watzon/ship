package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	DefaultConfigFile    = "ship.yml"
	LocalStateDir        = ".ship"
	RemoteStateDir       = "/var/lib/ship"
	RemoteBinaryPath     = "/usr/local/bin/ship"
	DefaultCaddyImage    = "caddy:2"
	DefaultRestartPolicy = "unless-stopped"

	ProviderHetzner      = "hetzner"
	ProviderVultr        = "vultr"
	ProviderDigitalOcean = "digitalocean"
	ProviderLinode       = "linode"
	ProviderAWS          = "aws"
	ProviderLightsail    = "lightsail"
	ProviderGCP          = "gcp"
	ProviderAzure        = "azure"
	ProviderScaleway     = "scaleway"
	ProviderOpenStack    = "openstack"
	ProviderCivo         = "civo"
	ProviderUpCloud      = "upcloud"
	ProviderOVHCloud     = "ovhcloud"
	ProviderOCI          = "oci"
	ProviderExoscale     = "exoscale"
	ProviderCloudscale   = "cloudscale"
	ProviderLatitude     = "latitude"
	ProviderKamatera     = "kamatera"
	ProviderProxmox      = "proxmox"
	ProviderSSHConfig    = "ssh_config"
	ProviderTerraform    = "terraform"
	ProviderPulumi       = "pulumi"
	ProviderAnsible      = "ansible"
	ProviderManual       = "manual"
)

const (
	SSHFirewallManaged  = "managed"
	SSHFirewallExternal = "external"
	SSHFirewallDisabled = "disabled"
)

type Config struct {
	Project       string                 `yaml:"project"`
	Registry      string                 `yaml:"registry"`
	SSH           SSHConfig              `yaml:"ssh"`
	Docker        DockerConfig           `yaml:"docker"`
	Ingress       IngressConfig          `yaml:"ingress"`
	Hooks         Hooks                  `yaml:"hooks"`
	Notifications Notifications          `yaml:"notifications"`
	Logging       LoggingConfig          `yaml:"logging"`
	Runtime       RuntimeConfig          `yaml:"runtime"`
	Environments  map[string]Environment `yaml:"environments"`
	Services      map[string]Service     `yaml:"services"`
	Accessories   map[string]Accessory   `yaml:"accessories"`
	Secrets       []string               `yaml:"secrets"`
}

type Environment struct {
	Provider      ProviderConfig       `yaml:"provider"`
	Hosts         HostsConfig          `yaml:"hosts"`
	SSH           SSHConfig            `yaml:"ssh"`
	Docker        DockerConfig         `yaml:"docker"`
	Ingress       IngressConfig        `yaml:"ingress"`
	Hooks         Hooks                `yaml:"hooks"`
	Notifications Notifications        `yaml:"notifications"`
	Runtime       RuntimeConfig        `yaml:"runtime"`
	Services      map[string]Service   `yaml:"services"`
	Accessories   map[string]Accessory `yaml:"accessories"`
	Secrets       []string             `yaml:"secrets"`
}

type ProviderConfig struct {
	Hetzner      *HetznerConfig      `yaml:"hetzner"`
	Vultr        *VultrConfig        `yaml:"vultr"`
	DigitalOcean *DigitalOceanConfig `yaml:"digitalocean"`
	Linode       *LinodeConfig       `yaml:"linode"`
	AWS          *AWSConfig          `yaml:"aws"`
	Lightsail    *LightsailConfig    `yaml:"lightsail"`
	GCP          *GCPConfig          `yaml:"gcp"`
	Azure        *AzureConfig        `yaml:"azure"`
	Scaleway     *ScalewayConfig     `yaml:"scaleway"`
	OpenStack    *OpenStackConfig    `yaml:"openstack"`
	Civo         *CivoConfig         `yaml:"civo"`
	UpCloud      *UpCloudConfig      `yaml:"upcloud"`
	OVHCloud     *OVHCloudConfig     `yaml:"ovhcloud"`
	OCI          *OCIConfig          `yaml:"oci"`
	Exoscale     *ExoscaleConfig     `yaml:"exoscale"`
	Cloudscale   *CloudscaleConfig   `yaml:"cloudscale"`
	Latitude     *LatitudeConfig     `yaml:"latitude"`
	Kamatera     *KamateraConfig     `yaml:"kamatera"`
	Proxmox      *ProxmoxConfig      `yaml:"proxmox"`
	SSHConfig    *SSHConfigInventory `yaml:"ssh_config"`
	Terraform    *TerraformConfig    `yaml:"terraform"`
	Pulumi       *PulumiConfig       `yaml:"pulumi"`
	Ansible      *AnsibleConfig      `yaml:"ansible"`
	Manual       *ManualConfig       `yaml:"manual"`
	Unknown      []string            `yaml:"-"`
}

type HetznerConfig struct {
	Location        string                `yaml:"location"`
	ServerType      string                `yaml:"server_type"`
	Image           string                `yaml:"image"`
	UserData        string                `yaml:"user_data"`
	UserDataFile    string                `yaml:"user_data_file"`
	SSHKeys         []string              `yaml:"ssh_keys"`
	Network         HetznerNetworkConfig  `yaml:"network"`
	Firewall        HetznerFirewallConfig `yaml:"firewall"`
	SSHAllowedCIDRs []string              `yaml:"ssh_allowed_cidrs"`
	SSHFirewall     string                `yaml:"ssh_firewall"`
}

type HetznerNetworkConfig struct {
	Enabled *bool  `yaml:"enabled"`
	Name    string `yaml:"name"`
	IPRange string `yaml:"ip_range"`
}

type HetznerFirewallConfig struct {
	Enabled *bool  `yaml:"enabled"`
	Name    string `yaml:"name"`
}

type VultrConfig struct {
	Region            string              `yaml:"region"`
	Plan              string              `yaml:"plan"`
	OSID              int                 `yaml:"os_id"`
	ImageID           string              `yaml:"image_id"`
	SnapshotID        string              `yaml:"snapshot_id"`
	AppID             int                 `yaml:"app_id"`
	Hostname          string              `yaml:"hostname"`
	UserData          string              `yaml:"user_data"`
	UserDataFile      string              `yaml:"user_data_file"`
	SSHKeyIDs         []string            `yaml:"ssh_key_ids"`
	FirewallGroupID   string              `yaml:"firewall_group_id"`
	Firewall          VultrFirewallConfig `yaml:"firewall"`
	SSHAllowedCIDRs   []string            `yaml:"ssh_allowed_cidrs"`
	SSHFirewall       string              `yaml:"ssh_firewall"`
	Backups           *bool               `yaml:"backups"`
	IPv6              *bool               `yaml:"ipv6"`
	DDoSProtection    *bool               `yaml:"ddos_protection"`
	ActivationEmail   *bool               `yaml:"activation_email"`
	EnableVPC         *bool               `yaml:"enable_vpc"`
	VPCIDs            []string            `yaml:"vpc_ids"`
	VPCOnly           *bool               `yaml:"vpc_only"`
	DisablePublicIPv4 *bool               `yaml:"disable_public_ipv4"`
	ReservedIPv4      string              `yaml:"reserved_ipv4"`
	UserScheme        string              `yaml:"user_scheme"`
	ScriptID          string              `yaml:"script_id"`
	AppVariables      map[string]string   `yaml:"app_variables"`
}

type VultrFirewallConfig struct {
	Enabled     *bool  `yaml:"enabled"`
	Description string `yaml:"description"`
}

type DigitalOceanConfig struct {
	Region          string                     `yaml:"region"`
	Size            string                     `yaml:"size"`
	Image           string                     `yaml:"image"`
	UserData        string                     `yaml:"user_data"`
	UserDataFile    string                     `yaml:"user_data_file"`
	SSHKeys         []string                   `yaml:"ssh_keys"`
	VPCUUID         string                     `yaml:"vpc_uuid"`
	Monitoring      *bool                      `yaml:"monitoring"`
	Backups         *bool                      `yaml:"backups"`
	IPv6            *bool                      `yaml:"ipv6"`
	Firewall        DigitalOceanFirewallConfig `yaml:"firewall"`
	SSHAllowedCIDRs []string                   `yaml:"ssh_allowed_cidrs"`
	SSHFirewall     string                     `yaml:"ssh_firewall"`
}

type DigitalOceanFirewallConfig struct {
	Enabled *bool  `yaml:"enabled"`
	Name    string `yaml:"name"`
}

type LinodeConfig struct {
	Region          string               `yaml:"region"`
	Type            string               `yaml:"type"`
	Image           string               `yaml:"image"`
	UserData        string               `yaml:"user_data"`
	UserDataFile    string               `yaml:"user_data_file"`
	AuthorizedKeys  []string             `yaml:"authorized_keys"`
	AuthorizedUsers []string             `yaml:"authorized_users"`
	PrivateIP       *bool                `yaml:"private_ip"`
	Backups         *bool                `yaml:"backups"`
	Firewall        LinodeFirewallConfig `yaml:"firewall"`
	SSHAllowedCIDRs []string             `yaml:"ssh_allowed_cidrs"`
	SSHFirewall     string               `yaml:"ssh_firewall"`
}

type LinodeFirewallConfig struct {
	Enabled *bool  `yaml:"enabled"`
	Label   string `yaml:"label"`
}

type AWSConfig struct {
	Region                   string                 `yaml:"region"`
	InstanceType             string                 `yaml:"instance_type"`
	AMI                      string                 `yaml:"ami"`
	UserData                 string                 `yaml:"user_data"`
	UserDataFile             string                 `yaml:"user_data_file"`
	KeyName                  string                 `yaml:"key_name"`
	SubnetID                 string                 `yaml:"subnet_id"`
	VPCID                    string                 `yaml:"vpc_id"`
	AssociatePublicIPAddress *bool                  `yaml:"associate_public_ip_address"`
	IAMInstanceProfile       string                 `yaml:"iam_instance_profile"`
	Monitoring               *bool                  `yaml:"monitoring"`
	RootVolume               AWSRootVolumeConfig    `yaml:"root_volume"`
	SecurityGroup            AWSSecurityGroupConfig `yaml:"security_group"`
	SSHAllowedCIDRs          []string               `yaml:"ssh_allowed_cidrs"`
	SSHFirewall              string                 `yaml:"ssh_firewall"`
}

type AWSRootVolumeConfig struct {
	DeviceName string `yaml:"device_name"`
	SizeGB     int    `yaml:"size_gb"`
	Type       string `yaml:"type"`
}

type AWSSecurityGroupConfig struct {
	Managed *bool  `yaml:"managed"`
	Name    string `yaml:"name"`
	ID      string `yaml:"id"`
}

type LightsailConfig struct {
	Region              string                  `yaml:"region"`
	AvailabilityZone    string                  `yaml:"availability_zone"`
	BundleID            string                  `yaml:"bundle_id"`
	BlueprintID         string                  `yaml:"blueprint_id"`
	KeyPairName         string                  `yaml:"key_pair_name"`
	UserData            string                  `yaml:"user_data"`
	UserDataFile        string                  `yaml:"user_data_file"`
	IPAddressType       string                  `yaml:"ip_address_type"`
	AddOns              []LightsailAddOn        `yaml:"add_ons"`
	Firewall            LightsailFirewallConfig `yaml:"firewall"`
	SSHAllowedCIDRs     []string                `yaml:"ssh_allowed_cidrs"`
	SSHFirewall         string                  `yaml:"ssh_firewall"`
	ForceDeleteAddOns   *bool                   `yaml:"force_delete_add_ons"`
	WaitTimeoutSeconds  int                     `yaml:"wait_timeout_seconds"`
	PollIntervalSeconds int                     `yaml:"poll_interval_seconds"`
}

type LightsailAddOn struct {
	Type              string `yaml:"type"`
	SnapshotTimeOfDay string `yaml:"snapshot_time_of_day"`
	StopDuration      string `yaml:"stop_duration"`
	StopThreshold     string `yaml:"stop_threshold"`
}

type LightsailFirewallConfig struct {
	Managed *bool `yaml:"managed"`
}

type GCPConfig struct {
	ProjectID       string            `yaml:"project_id"`
	Zone            string            `yaml:"zone"`
	MachineType     string            `yaml:"machine_type"`
	Image           string            `yaml:"image"`
	ImageProject    string            `yaml:"image_project"`
	UserData        string            `yaml:"user_data"`
	UserDataFile    string            `yaml:"user_data_file"`
	Network         string            `yaml:"network"`
	Subnetwork      string            `yaml:"subnetwork"`
	NetworkTags     []string          `yaml:"network_tags"`
	Metadata        map[string]string `yaml:"metadata"`
	ServiceAccount  string            `yaml:"service_account"`
	Scopes          []string          `yaml:"scopes"`
	ExternalIP      *bool             `yaml:"external_ip"`
	BootDisk        GCPBootDiskConfig `yaml:"boot_disk"`
	ShieldedVM      GCPShieldedConfig `yaml:"shielded_vm"`
	Firewall        GCPFirewallConfig `yaml:"firewall"`
	SSHAllowedCIDRs []string          `yaml:"ssh_allowed_cidrs"`
	SSHFirewall     string            `yaml:"ssh_firewall"`
}

type GCPBootDiskConfig struct {
	SizeGB int    `yaml:"size_gb"`
	Type   string `yaml:"type"`
}

type GCPShieldedConfig struct {
	SecureBoot          *bool `yaml:"secure_boot"`
	VTPM                *bool `yaml:"vtpm"`
	IntegrityMonitoring *bool `yaml:"integrity_monitoring"`
}

type GCPFirewallConfig struct {
	Managed *bool  `yaml:"managed"`
	Name    string `yaml:"name"`
}

type AzureConfig struct {
	SubscriptionID       string                   `yaml:"subscription_id"`
	ResourceGroup        string                   `yaml:"resource_group"`
	Location             string                   `yaml:"location"`
	VMSize               string                   `yaml:"vm_size"`
	Image                string                   `yaml:"image"`
	AdminUsername        string                   `yaml:"admin_username"`
	SSHPublicKey         string                   `yaml:"ssh_public_key"`
	UserData             string                   `yaml:"user_data"`
	UserDataFile         string                   `yaml:"user_data_file"`
	VirtualNetwork       string                   `yaml:"virtual_network"`
	Subnet               string                   `yaml:"subnet"`
	SubnetID             string                   `yaml:"subnet_id"`
	PublicIP             *bool                    `yaml:"public_ip"`
	OSDisk               AzureOSDiskConfig        `yaml:"os_disk"`
	SecurityGroup        AzureSecurityGroupConfig `yaml:"security_group"`
	SSHAllowedCIDRs      []string                 `yaml:"ssh_allowed_cidrs"`
	SSHFirewall          string                   `yaml:"ssh_firewall"`
	DisablePasswordLogin *bool                    `yaml:"disable_password_login"`
}

type AzureOSDiskConfig struct {
	SizeGB int    `yaml:"size_gb"`
	Type   string `yaml:"type"`
}

type AzureSecurityGroupConfig struct {
	Managed *bool  `yaml:"managed"`
	Name    string `yaml:"name"`
	ID      string `yaml:"id"`
}

type ScalewayConfig struct {
	ProjectID         string                `yaml:"project_id"`
	Zone              string                `yaml:"zone"`
	CommercialType    string                `yaml:"commercial_type"`
	Image             string                `yaml:"image"`
	UserData          string                `yaml:"user_data"`
	UserDataFile      string                `yaml:"user_data_file"`
	EnableIPv6        *bool                 `yaml:"enable_ipv6"`
	DynamicIPRequired *bool                 `yaml:"dynamic_ip_required"`
	RoutedIPEnabled   *bool                 `yaml:"routed_ip_enabled"`
	BootAfterCreate   *bool                 `yaml:"boot_after_create"`
	Volumes           map[string]any        `yaml:"volumes"`
	SecurityGroup     ScalewaySecurityGroup `yaml:"security_group"`
	SSHAllowedCIDRs   []string              `yaml:"ssh_allowed_cidrs"`
	SSHFirewall       string                `yaml:"ssh_firewall"`
}

type ScalewaySecurityGroup struct {
	Managed     *bool  `yaml:"managed"`
	Name        string `yaml:"name"`
	ID          string `yaml:"id"`
	Description string `yaml:"description"`
}

type OpenStackConfig struct {
	AuthURL                     string                       `yaml:"auth_url"`
	ComputeURL                  string                       `yaml:"compute_url"`
	NetworkURL                  string                       `yaml:"network_url"`
	Region                      string                       `yaml:"region"`
	Interface                   string                       `yaml:"interface"`
	ProjectID                   string                       `yaml:"project_id"`
	ProjectName                 string                       `yaml:"project_name"`
	ProjectDomainID             string                       `yaml:"project_domain_id"`
	ProjectDomainName           string                       `yaml:"project_domain_name"`
	UserID                      string                       `yaml:"user_id"`
	Username                    string                       `yaml:"username"`
	UserDomainID                string                       `yaml:"user_domain_id"`
	UserDomainName              string                       `yaml:"user_domain_name"`
	ApplicationCredentialID     string                       `yaml:"application_credential_id"`
	ApplicationCredentialName   string                       `yaml:"application_credential_name"`
	ApplicationCredentialSecret string                       `yaml:"application_credential_secret"`
	Flavor                      string                       `yaml:"flavor"`
	Image                       string                       `yaml:"image"`
	Network                     string                       `yaml:"network"`
	KeyName                     string                       `yaml:"key_name"`
	SecurityGroups              []string                     `yaml:"security_groups"`
	SecurityGroup               OpenStackSecurityGroupConfig `yaml:"security_group"`
	FloatingIP                  OpenStackFloatingIPConfig    `yaml:"floating_ip"`
	SSHAllowedCIDRs             []string                     `yaml:"ssh_allowed_cidrs"`
	SSHFirewall                 string                       `yaml:"ssh_firewall"`
	AvailabilityZone            string                       `yaml:"availability_zone"`
	ConfigDrive                 *bool                        `yaml:"config_drive"`
	Metadata                    map[string]string            `yaml:"metadata"`
	Tags                        []string                     `yaml:"tags"`
	SchedulerHints              map[string]any               `yaml:"scheduler_hints"`
	UserData                    string                       `yaml:"user_data"`
	UserDataFile                string                       `yaml:"user_data_file"`
}

type OpenStackSecurityGroupConfig struct {
	Managed     *bool  `yaml:"managed"`
	Name        string `yaml:"name"`
	ID          string `yaml:"id"`
	Description string `yaml:"description"`
}

type OpenStackFloatingIPConfig struct {
	Enabled        *bool  `yaml:"enabled"`
	NetworkID      string `yaml:"network_id"`
	SubnetID       string `yaml:"subnet_id"`
	Address        string `yaml:"address"`
	FixedIPAddress string `yaml:"fixed_ip_address"`
	Description    string `yaml:"description"`
	DNSName        string `yaml:"dns_name"`
	DNSDomain      string `yaml:"dns_domain"`
	QOSPolicyID    string `yaml:"qos_policy_id"`
	Distributed    *bool  `yaml:"distributed"`
}

type CivoConfig struct {
	Region                string             `yaml:"region"`
	Size                  string             `yaml:"size"`
	Image                 string             `yaml:"image"`
	NetworkID             string             `yaml:"network_id"`
	Hostname              string             `yaml:"hostname"`
	UserData              string             `yaml:"user_data"`
	UserDataFile          string             `yaml:"user_data_file"`
	SSHKeyID              string             `yaml:"ssh_key_id"`
	InitialUser           string             `yaml:"initial_user"`
	PublicIP              *bool              `yaml:"public_ip"`
	ReverseDNS            string             `yaml:"reverse_dns"`
	PrivateIPv4           string             `yaml:"private_ipv4"`
	AllowedIPs            []string           `yaml:"allowed_ips"`
	NetworkBandwidthLimit int                `yaml:"network_bandwidth_limit"`
	Firewall              CivoFirewallConfig `yaml:"firewall"`
	SSHAllowedCIDRs       []string           `yaml:"ssh_allowed_cidrs"`
	SSHFirewall           string             `yaml:"ssh_firewall"`
}

type CivoFirewallConfig struct {
	Managed *bool  `yaml:"managed"`
	Name    string `yaml:"name"`
	ID      string `yaml:"id"`
}

type UpCloudConfig struct {
	Zone             string                `yaml:"zone"`
	Plan             string                `yaml:"plan"`
	Template         string                `yaml:"template"`
	StorageSizeGB    int                   `yaml:"storage_size_gb"`
	StorageTier      string                `yaml:"storage_tier"`
	Hostname         string                `yaml:"hostname"`
	UserData         string                `yaml:"user_data"`
	UserDataFile     string                `yaml:"user_data_file"`
	SSHKeys          []string              `yaml:"ssh_keys"`
	Username         string                `yaml:"username"`
	Metadata         *bool                 `yaml:"metadata"`
	IPv6             *bool                 `yaml:"ipv6"`
	UtilityNetwork   *bool                 `yaml:"utility_network"`
	PrivateNetworkID string                `yaml:"private_network_id"`
	SimpleBackup     string                `yaml:"simple_backup"`
	ServerGroup      string                `yaml:"server_group"`
	Timezone         string                `yaml:"timezone"`
	Firewall         UpCloudFirewallConfig `yaml:"firewall"`
	SSHAllowedCIDRs  []string              `yaml:"ssh_allowed_cidrs"`
	SSHFirewall      string                `yaml:"ssh_firewall"`
}

type UpCloudFirewallConfig struct {
	Managed *bool `yaml:"managed"`
}

type OVHCloudConfig struct {
	ServiceName    string `yaml:"service_name"`
	Endpoint       string `yaml:"endpoint"`
	Region         string `yaml:"region"`
	FlavorID       string `yaml:"flavor_id"`
	ImageID        string `yaml:"image_id"`
	SSHKeyID       string `yaml:"ssh_key_id"`
	UserData       string `yaml:"user_data"`
	UserDataFile   string `yaml:"user_data_file"`
	MonthlyBilling *bool  `yaml:"monthly_billing"`
}

type OCIConfig struct {
	Region               string                  `yaml:"region"`
	CompartmentID        string                  `yaml:"compartment_id"`
	AvailabilityDomain   string                  `yaml:"availability_domain"`
	Shape                string                  `yaml:"shape"`
	ImageID              string                  `yaml:"image_id"`
	SubnetID             string                  `yaml:"subnet_id"`
	PrivateKeyFile       string                  `yaml:"private_key_file"`
	SSHAuthorizedKeys    []string                `yaml:"ssh_authorized_keys"`
	AssignPublicIP       *bool                   `yaml:"assign_public_ip"`
	NSGIDs               []string                `yaml:"nsg_ids"`
	NetworkSecurityGroup OCINetworkSecurityGroup `yaml:"network_security_group"`
	SSHAllowedCIDRs      []string                `yaml:"ssh_allowed_cidrs"`
	SSHFirewall          string                  `yaml:"ssh_firewall"`
	BootVolumeSizeGB     int                     `yaml:"boot_volume_size_gb"`
	ShapeConfig          OCIShapeConfig          `yaml:"shape_config"`
	Metadata             map[string]string       `yaml:"metadata"`
	FreeformTags         map[string]string       `yaml:"freeform_tags"`
	UserData             string                  `yaml:"user_data"`
	UserDataFile         string                  `yaml:"user_data_file"`
	PreserveBootVolume   *bool                   `yaml:"preserve_boot_volume"`
}

type OCINetworkSecurityGroup struct {
	Managed *bool  `yaml:"managed"`
	Name    string `yaml:"name"`
	ID      string `yaml:"id"`
	VCNID   string `yaml:"vcn_id"`
}

type OCIShapeConfig struct {
	OCPUs    float64 `yaml:"ocpus"`
	MemoryGB float64 `yaml:"memory_gb"`
}

type ExoscaleConfig struct {
	Zone                          string                      `yaml:"zone"`
	InstanceType                  string                      `yaml:"instance_type"`
	Template                      string                      `yaml:"template"`
	DiskSizeGB                    int                         `yaml:"disk_size_gb"`
	SSHKey                        string                      `yaml:"ssh_key"`
	SSHKeys                       []string                    `yaml:"ssh_keys"`
	UserData                      string                      `yaml:"user_data"`
	UserDataFile                  string                      `yaml:"user_data_file"`
	PublicIPAssignment            string                      `yaml:"public_ip_assignment"`
	SecurityGroups                []string                    `yaml:"security_groups"`
	SecurityGroup                 ExoscaleSecurityGroupConfig `yaml:"security_group"`
	SSHAllowedCIDRs               []string                    `yaml:"ssh_allowed_cidrs"`
	SSHFirewall                   string                      `yaml:"ssh_firewall"`
	AntiAffinityGroups            []string                    `yaml:"anti_affinity_groups"`
	DeployTarget                  string                      `yaml:"deploy_target"`
	AutoStart                     *bool                       `yaml:"auto_start"`
	SecureBoot                    *bool                       `yaml:"secure_boot"`
	TPM                           *bool                       `yaml:"tpm"`
	ApplicationConsistentSnapshot *bool                       `yaml:"application_consistent_snapshot"`
}

type ExoscaleSecurityGroupConfig struct {
	Managed     *bool  `yaml:"managed"`
	Name        string `yaml:"name"`
	ID          string `yaml:"id"`
	Description string `yaml:"description"`
}

type CloudscaleConfig struct {
	Zone              string                      `yaml:"zone"`
	Flavor            string                      `yaml:"flavor"`
	Image             string                      `yaml:"image"`
	SSHKeys           []string                    `yaml:"ssh_keys"`
	Password          string                      `yaml:"password"`
	UserData          string                      `yaml:"user_data"`
	UserDataFile      string                      `yaml:"user_data_file"`
	UserDataHandling  string                      `yaml:"user_data_handling"`
	VolumeSizeGB      int                         `yaml:"volume_size_gb"`
	BulkVolumeSizeGB  int                         `yaml:"bulk_volume_size_gb"`
	Volumes           []CloudscaleVolume          `yaml:"volumes" json:"volumes,omitempty"`
	UsePublicNetwork  *bool                       `yaml:"use_public_network"`
	UsePrivateNetwork *bool                       `yaml:"use_private_network"`
	UseIPv6           *bool                       `yaml:"use_ipv6"`
	Interfaces        []CloudscaleInterface       `yaml:"interfaces" json:"interfaces,omitempty"`
	ServerGroups      []string                    `yaml:"server_groups"`
	ServerGroup       CloudscaleServerGroupConfig `yaml:"server_group"`
	AntiAffinityWith  string                      `yaml:"anti_affinity_with"`
}

type CloudscaleVolume struct {
	SizeGB int    `yaml:"size_gb" json:"size_gb"`
	Type   string `yaml:"type" json:"type,omitempty"`
}

type CloudscaleInterface struct {
	Network   string                       `yaml:"network" json:"network"`
	Addresses []CloudscaleInterfaceAddress `yaml:"addresses" json:"addresses,omitempty"`
}

type CloudscaleInterfaceAddress struct {
	Subnet  string `yaml:"subnet" json:"subnet,omitempty"`
	Address string `yaml:"address" json:"address,omitempty"`
}

type CloudscaleServerGroupConfig struct {
	Managed *bool  `yaml:"managed"`
	Name    string `yaml:"name"`
	UUID    string `yaml:"uuid"`
}

type LatitudeConfig struct {
	Project         string               `yaml:"project"`
	Site            string               `yaml:"site"`
	Plan            string               `yaml:"plan"`
	OperatingSystem string               `yaml:"operating_system"`
	SSHKeys         []string             `yaml:"ssh_keys"`
	Firewall        LatitudeFirewall     `yaml:"firewall"`
	SSHAllowedCIDRs []string             `yaml:"ssh_allowed_cidrs"`
	SSHFirewall     string               `yaml:"ssh_firewall"`
	UserDataID      string               `yaml:"user_data_id"`
	UserData        string               `yaml:"user_data"`
	UserDataFile    string               `yaml:"user_data_file"`
	RAID            string               `yaml:"raid"`
	DiskLayout      []LatitudeDiskLayout `yaml:"disk_layout" json:"disk_layout,omitempty"`
	IPXE            string               `yaml:"ipxe"`
	Billing         string               `yaml:"billing"`
	DeleteReason    string               `yaml:"delete_reason"`
}

type LatitudeFirewall struct {
	Managed *bool  `yaml:"managed"`
	Name    string `yaml:"name"`
	ID      string `yaml:"id"`
}

type LatitudeDiskLayout struct {
	Count      int    `yaml:"count" json:"count"`
	Role       string `yaml:"role" json:"role"`
	RAIDLevel  string `yaml:"raid_level,omitempty" json:"raid_level,omitempty"`
	Filesystem string `yaml:"filesystem,omitempty" json:"filesystem,omitempty"`
	MountPoint string `yaml:"mount_point,omitempty" json:"mount_point,omitempty"`
}

type KamateraConfig struct {
	Datacenter          string `yaml:"datacenter"`
	CPU                 string `yaml:"cpu"`
	RAMMB               int    `yaml:"ram_mb"`
	Image               string `yaml:"image"`
	DiskGB              int    `yaml:"disk_gb"`
	Password            string `yaml:"password"`
	PasswordEnv         string `yaml:"password_env"`
	Billing             string `yaml:"billing"`
	Traffic             string `yaml:"traffic"`
	Network             string `yaml:"network"`
	NetworkIP           string `yaml:"network_ip"`
	NetworkBits         int    `yaml:"network_bits"`
	SSHPublicKey        string `yaml:"ssh_public_key"`
	Managed             *bool  `yaml:"managed"`
	Backup              *bool  `yaml:"backup"`
	Power               *bool  `yaml:"power"`
	WaitTimeoutSeconds  int    `yaml:"wait_timeout_seconds"`
	PollIntervalSeconds int    `yaml:"poll_interval_seconds"`
}

type ProxmoxConfig struct {
	APIURL                string   `yaml:"api_url"`
	InsecureSkipTLSVerify bool     `yaml:"insecure_skip_tls_verify"`
	Node                  string   `yaml:"node"`
	TemplateID            int      `yaml:"template_id"`
	Storage               string   `yaml:"storage"`
	FullClone             *bool    `yaml:"full_clone"`
	Pool                  string   `yaml:"pool"`
	Bridge                string   `yaml:"bridge"`
	VLAN                  int      `yaml:"vlan"`
	MemoryMB              int      `yaml:"memory_mb"`
	Cores                 int      `yaml:"cores"`
	CIUser                string   `yaml:"ciuser"`
	SSHKeys               []string `yaml:"ssh_keys"`
	IPConfig              string   `yaml:"ipconfig"`
	Nameserver            string   `yaml:"nameserver"`
	SearchDomain          string   `yaml:"searchdomain"`
	Agent                 *bool    `yaml:"agent"`
	OnBoot                *bool    `yaml:"onboot"`
	Start                 *bool    `yaml:"start"`
	Tags                  []string `yaml:"tags"`
	Description           string   `yaml:"description"`
}

type SSHConfigInventory struct {
	Path string `yaml:"path"`
	User string `yaml:"user"`
}

type TerraformConfig struct {
	WorkingDir string `yaml:"working_dir"`
	Workspace  string `yaml:"workspace"`
	Binary     string `yaml:"binary"`
	Output     string `yaml:"output"`
	User       string `yaml:"user"`
}

type PulumiConfig struct {
	WorkingDir  string `yaml:"working_dir"`
	Stack       string `yaml:"stack"`
	Binary      string `yaml:"binary"`
	Output      string `yaml:"output"`
	User        string `yaml:"user"`
	ShowSecrets *bool  `yaml:"show_secrets"`
}

type AnsibleConfig struct {
	InventoryFile string   `yaml:"inventory_file"`
	Command       []string `yaml:"command"`
	User          string   `yaml:"user"`
}

type ManualConfig struct{}

func (p *ProviderConfig) UnmarshalYAML(value *yaml.Node) error {
	type providerConfig ProviderConfig
	var decoded providerConfig
	if err := value.Decode(&decoded); err != nil {
		return err
	}
	*p = ProviderConfig(decoded)
	p.Unknown = nil
	if value.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		name := value.Content[i].Value
		switch name {
		case ProviderHetzner, ProviderVultr, ProviderDigitalOcean, ProviderLinode, ProviderAWS, ProviderLightsail, ProviderGCP, ProviderAzure, ProviderScaleway, ProviderOpenStack, ProviderCivo, ProviderUpCloud, ProviderOVHCloud, ProviderOCI, ProviderExoscale, ProviderCloudscale, ProviderLatitude, ProviderKamatera, ProviderProxmox, ProviderSSHConfig, ProviderTerraform, ProviderPulumi, ProviderAnsible, ProviderManual:
		default:
			p.Unknown = append(p.Unknown, name)
		}
	}
	sort.Strings(p.Unknown)
	return nil
}

func (p ProviderConfig) Name() string {
	if p.Hetzner != nil {
		return ProviderHetzner
	}
	if p.Vultr != nil {
		return ProviderVultr
	}
	if p.DigitalOcean != nil {
		return ProviderDigitalOcean
	}
	if p.Linode != nil {
		return ProviderLinode
	}
	if p.AWS != nil {
		return ProviderAWS
	}
	if p.Lightsail != nil {
		return ProviderLightsail
	}
	if p.GCP != nil {
		return ProviderGCP
	}
	if p.Azure != nil {
		return ProviderAzure
	}
	if p.Scaleway != nil {
		return ProviderScaleway
	}
	if p.OpenStack != nil {
		return ProviderOpenStack
	}
	if p.Civo != nil {
		return ProviderCivo
	}
	if p.UpCloud != nil {
		return ProviderUpCloud
	}
	if p.OVHCloud != nil {
		return ProviderOVHCloud
	}
	if p.OCI != nil {
		return ProviderOCI
	}
	if p.Exoscale != nil {
		return ProviderExoscale
	}
	if p.Cloudscale != nil {
		return ProviderCloudscale
	}
	if p.Latitude != nil {
		return ProviderLatitude
	}
	if p.Kamatera != nil {
		return ProviderKamatera
	}
	if p.Proxmox != nil {
		return ProviderProxmox
	}
	if p.SSHConfig != nil {
		return ProviderSSHConfig
	}
	if p.Terraform != nil {
		return ProviderTerraform
	}
	if p.Pulumi != nil {
		return ProviderPulumi
	}
	if p.Ansible != nil {
		return ProviderAnsible
	}
	if p.Manual != nil {
		return ProviderManual
	}
	return ""
}

func (p ProviderConfig) Validate(envName string) []string {
	var errs []string
	blocks := p.blocks()
	if len(blocks) == 0 {
		return []string{fmt.Sprintf("environment %q must define exactly one provider", envName)}
	}
	if len(blocks) > 1 {
		errs = append(errs, fmt.Sprintf("environment %q must define exactly one provider (found %s)", envName, strings.Join(blocks, ", ")))
	}
	if len(p.Unknown) > 0 {
		errs = append(errs, fmt.Sprintf("environment %q defines unsupported provider(s): %s", envName, strings.Join(p.Unknown, ", ")))
	}
	if p.Hetzner != nil {
		errs = append(errs, validateUserDataConfig(fmt.Sprintf("environment %q provider.hetzner", envName), p.Hetzner.UserData, p.Hetzner.UserDataFile)...)
		if p.Hetzner.Location == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.hetzner.location is required", envName))
		}
		if p.Hetzner.ServerType == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.hetzner.server_type is required", envName))
		}
		if p.Hetzner.Image == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.hetzner.image is required", envName))
		}
		sshFirewall := p.Hetzner.EffectiveSSHFirewall()
		switch sshFirewall {
		case SSHFirewallManaged:
			if p.Hetzner.Firewall.EnabledValue(true) && len(p.Hetzner.SSHAllowedCIDRs) == 0 {
				errs = append(errs, fmt.Sprintf("environment %q provider.hetzner.ssh_allowed_cidrs is required when managed firewall SSH is enabled", envName))
			}
		case SSHFirewallExternal, SSHFirewallDisabled:
		default:
			errs = append(errs, fmt.Sprintf("environment %q provider.hetzner.ssh_firewall must be managed, external, or disabled", envName))
		}
	}
	if p.Vultr != nil {
		errs = append(errs, validateUserDataConfig(fmt.Sprintf("environment %q provider.vultr", envName), p.Vultr.UserData, p.Vultr.UserDataFile)...)
		if p.Vultr.Region == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.vultr.region is required", envName))
		}
		if p.Vultr.Plan == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.vultr.plan is required", envName))
		}
		sources := p.Vultr.sourceBlocks()
		if len(sources) != 1 {
			errs = append(errs, fmt.Sprintf("environment %q provider.vultr must define exactly one source (found %s)", envName, strings.Join(sources, ", ")))
		}
		switch p.Vultr.UserScheme {
		case "", "root", "limited":
		default:
			errs = append(errs, fmt.Sprintf("environment %q provider.vultr.user_scheme must be root or limited", envName))
		}
		sshFirewall := p.Vultr.EffectiveSSHFirewall()
		switch sshFirewall {
		case SSHFirewallManaged:
			if p.Vultr.FirewallGroupID == "" && p.Vultr.Firewall.EnabledValue(true) && len(p.Vultr.SSHAllowedCIDRs) == 0 {
				errs = append(errs, fmt.Sprintf("environment %q provider.vultr.ssh_allowed_cidrs is required when managed firewall SSH is enabled", envName))
			}
		case SSHFirewallExternal, SSHFirewallDisabled:
		default:
			errs = append(errs, fmt.Sprintf("environment %q provider.vultr.ssh_firewall must be managed, external, or disabled", envName))
		}
	}
	if p.DigitalOcean != nil {
		errs = append(errs, validateUserDataConfig(fmt.Sprintf("environment %q provider.digitalocean", envName), p.DigitalOcean.UserData, p.DigitalOcean.UserDataFile)...)
		if p.DigitalOcean.Region == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.digitalocean.region is required", envName))
		}
		if p.DigitalOcean.Size == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.digitalocean.size is required", envName))
		}
		if p.DigitalOcean.Image == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.digitalocean.image is required", envName))
		}
		sshFirewall := p.DigitalOcean.EffectiveSSHFirewall()
		switch sshFirewall {
		case SSHFirewallManaged:
			if p.DigitalOcean.Firewall.EnabledValue(true) && len(p.DigitalOcean.SSHAllowedCIDRs) == 0 {
				errs = append(errs, fmt.Sprintf("environment %q provider.digitalocean.ssh_allowed_cidrs is required when managed firewall SSH is enabled", envName))
			}
		case SSHFirewallExternal, SSHFirewallDisabled:
		default:
			errs = append(errs, fmt.Sprintf("environment %q provider.digitalocean.ssh_firewall must be managed, external, or disabled", envName))
		}
	}
	if p.Linode != nil {
		errs = append(errs, validateUserDataConfig(fmt.Sprintf("environment %q provider.linode", envName), p.Linode.UserData, p.Linode.UserDataFile)...)
		if p.Linode.Region == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.linode.region is required", envName))
		}
		if p.Linode.Type == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.linode.type is required", envName))
		}
		if p.Linode.Image == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.linode.image is required", envName))
		}
		if len(p.Linode.AuthorizedKeys) == 0 && len(p.Linode.AuthorizedUsers) == 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.linode.authorized_keys or provider.linode.authorized_users is required", envName))
		}
		sshFirewall := p.Linode.EffectiveSSHFirewall()
		switch sshFirewall {
		case SSHFirewallManaged:
			if p.Linode.Firewall.EnabledValue(true) && len(p.Linode.SSHAllowedCIDRs) == 0 {
				errs = append(errs, fmt.Sprintf("environment %q provider.linode.ssh_allowed_cidrs is required when managed firewall SSH is enabled", envName))
			}
		case SSHFirewallExternal, SSHFirewallDisabled:
		default:
			errs = append(errs, fmt.Sprintf("environment %q provider.linode.ssh_firewall must be managed, external, or disabled", envName))
		}
	}
	if p.AWS != nil {
		errs = append(errs, validateUserDataConfig(fmt.Sprintf("environment %q provider.aws", envName), p.AWS.UserData, p.AWS.UserDataFile)...)
		if p.AWS.Region == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.aws.region is required", envName))
		}
		if p.AWS.InstanceType == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.aws.instance_type is required", envName))
		}
		if p.AWS.AMI == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.aws.ami is required", envName))
		}
		if p.AWS.SubnetID == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.aws.subnet_id is required", envName))
		}
		if p.AWS.SecurityGroup.ID != "" && p.AWS.SecurityGroup.ManagedValue(true) {
			errs = append(errs, fmt.Sprintf("environment %q provider.aws.security_group.id requires security_group.managed: false", envName))
		}
		if p.AWS.RootVolume.SizeGB < 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.aws.root_volume.size_gb cannot be negative", envName))
		}
		sshFirewall := p.AWS.EffectiveSSHFirewall()
		switch sshFirewall {
		case SSHFirewallManaged:
			if p.AWS.SecurityGroup.ManagedValue(true) && len(p.AWS.SSHAllowedCIDRs) == 0 {
				errs = append(errs, fmt.Sprintf("environment %q provider.aws.ssh_allowed_cidrs is required when managed security group SSH is enabled", envName))
			}
		case SSHFirewallExternal, SSHFirewallDisabled:
		default:
			errs = append(errs, fmt.Sprintf("environment %q provider.aws.ssh_firewall must be managed, external, or disabled", envName))
		}
	}
	if p.Lightsail != nil {
		errs = append(errs, validateUserDataConfig(fmt.Sprintf("environment %q provider.lightsail", envName), p.Lightsail.UserData, p.Lightsail.UserDataFile)...)
		if p.Lightsail.Region == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.lightsail.region is required", envName))
		}
		if p.Lightsail.AvailabilityZone == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.lightsail.availability_zone is required", envName))
		}
		if p.Lightsail.BundleID == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.lightsail.bundle_id is required", envName))
		}
		if p.Lightsail.BlueprintID == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.lightsail.blueprint_id is required", envName))
		}
		switch p.Lightsail.IPAddressType {
		case "", "dualstack", "ipv4", "ipv6":
		default:
			errs = append(errs, fmt.Sprintf("environment %q provider.lightsail.ip_address_type must be dualstack, ipv4, or ipv6", envName))
		}
		if p.Lightsail.Firewall.ManagedValue(true) {
			sshFirewall := p.Lightsail.EffectiveSSHFirewall()
			switch sshFirewall {
			case SSHFirewallManaged:
				if len(p.Lightsail.SSHAllowedCIDRs) == 0 {
					errs = append(errs, fmt.Sprintf("environment %q provider.lightsail.ssh_allowed_cidrs is required when managed firewall SSH is enabled", envName))
				}
			case SSHFirewallExternal, SSHFirewallDisabled:
			default:
				errs = append(errs, fmt.Sprintf("environment %q provider.lightsail.ssh_firewall must be managed, external, or disabled", envName))
			}
		}
		for i, addOn := range p.Lightsail.AddOns {
			switch addOn.Type {
			case "AutoSnapshot", "StopInstanceOnIdle":
			default:
				errs = append(errs, fmt.Sprintf("environment %q provider.lightsail.add_ons[%d].type must be AutoSnapshot or StopInstanceOnIdle", envName, i))
			}
		}
		if p.Lightsail.WaitTimeoutSeconds < 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.lightsail.wait_timeout_seconds cannot be negative", envName))
		}
		if p.Lightsail.PollIntervalSeconds < 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.lightsail.poll_interval_seconds cannot be negative", envName))
		}
	}
	if p.GCP != nil {
		errs = append(errs, validateUserDataConfig(fmt.Sprintf("environment %q provider.gcp", envName), p.GCP.UserData, p.GCP.UserDataFile)...)
		if p.GCP.ProjectID == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.gcp.project_id is required", envName))
		}
		if p.GCP.Zone == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.gcp.zone is required", envName))
		}
		if p.GCP.MachineType == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.gcp.machine_type is required", envName))
		}
		if p.GCP.Image == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.gcp.image is required", envName))
		}
		if p.GCP.BootDisk.SizeGB < 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.gcp.boot_disk.size_gb cannot be negative", envName))
		}
		sshFirewall := p.GCP.EffectiveSSHFirewall()
		switch sshFirewall {
		case SSHFirewallManaged:
			if p.GCP.Firewall.ManagedValue(true) && len(p.GCP.SSHAllowedCIDRs) == 0 {
				errs = append(errs, fmt.Sprintf("environment %q provider.gcp.ssh_allowed_cidrs is required when managed firewall SSH is enabled", envName))
			}
		case SSHFirewallExternal, SSHFirewallDisabled:
		default:
			errs = append(errs, fmt.Sprintf("environment %q provider.gcp.ssh_firewall must be managed, external, or disabled", envName))
		}
	}
	if p.Azure != nil {
		errs = append(errs, validateUserDataConfig(fmt.Sprintf("environment %q provider.azure", envName), p.Azure.UserData, p.Azure.UserDataFile)...)
		if p.Azure.SubscriptionID == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.azure.subscription_id is required", envName))
		}
		if p.Azure.ResourceGroup == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.azure.resource_group is required", envName))
		}
		if p.Azure.Location == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.azure.location is required", envName))
		}
		if p.Azure.VMSize == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.azure.vm_size is required", envName))
		}
		if p.Azure.Image == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.azure.image is required", envName))
		}
		if p.Azure.AdminUsername == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.azure.admin_username is required", envName))
		}
		if p.Azure.SSHPublicKey == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.azure.ssh_public_key is required", envName))
		}
		if p.Azure.SubnetID == "" && (p.Azure.VirtualNetwork == "" || p.Azure.Subnet == "") {
			errs = append(errs, fmt.Sprintf("environment %q provider.azure.subnet_id or virtual_network/subnet is required", envName))
		}
		if p.Azure.SecurityGroup.ID != "" && p.Azure.SecurityGroup.ManagedValue(true) {
			errs = append(errs, fmt.Sprintf("environment %q provider.azure.security_group.id requires security_group.managed: false", envName))
		}
		if p.Azure.OSDisk.SizeGB < 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.azure.os_disk.size_gb cannot be negative", envName))
		}
		sshFirewall := p.Azure.EffectiveSSHFirewall()
		switch sshFirewall {
		case SSHFirewallManaged:
			if p.Azure.SecurityGroup.ManagedValue(true) && len(p.Azure.SSHAllowedCIDRs) == 0 {
				errs = append(errs, fmt.Sprintf("environment %q provider.azure.ssh_allowed_cidrs is required when managed network security group SSH is enabled", envName))
			}
		case SSHFirewallExternal, SSHFirewallDisabled:
		default:
			errs = append(errs, fmt.Sprintf("environment %q provider.azure.ssh_firewall must be managed, external, or disabled", envName))
		}
	}
	if p.Scaleway != nil {
		errs = append(errs, validateUserDataConfig(fmt.Sprintf("environment %q provider.scaleway", envName), p.Scaleway.UserData, p.Scaleway.UserDataFile)...)
		if p.Scaleway.ProjectID == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.scaleway.project_id is required", envName))
		}
		if p.Scaleway.Zone == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.scaleway.zone is required", envName))
		}
		if p.Scaleway.CommercialType == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.scaleway.commercial_type is required", envName))
		}
		if p.Scaleway.Image == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.scaleway.image is required", envName))
		}
		if p.Scaleway.SecurityGroup.ID != "" && p.Scaleway.SecurityGroup.ManagedValue(true) {
			errs = append(errs, fmt.Sprintf("environment %q provider.scaleway.security_group.id requires security_group.managed: false", envName))
		}
		sshFirewall := p.Scaleway.EffectiveSSHFirewall()
		switch sshFirewall {
		case SSHFirewallManaged:
			if p.Scaleway.SecurityGroup.ManagedValue(true) && len(p.Scaleway.SSHAllowedCIDRs) == 0 {
				errs = append(errs, fmt.Sprintf("environment %q provider.scaleway.ssh_allowed_cidrs is required when managed security group SSH is enabled", envName))
			}
		case SSHFirewallExternal, SSHFirewallDisabled:
		default:
			errs = append(errs, fmt.Sprintf("environment %q provider.scaleway.ssh_firewall must be managed, external, or disabled", envName))
		}
	}
	if p.OpenStack != nil {
		errs = append(errs, validateUserDataConfig(fmt.Sprintf("environment %q provider.openstack", envName), p.OpenStack.UserData, p.OpenStack.UserDataFile)...)
		if p.OpenStack.Flavor == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.openstack.flavor is required", envName))
		}
		if p.OpenStack.Image == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.openstack.image is required", envName))
		}
		if p.OpenStack.ComputeURL == "" && p.OpenStack.Region == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.openstack.region is required when compute_url is not set", envName))
		}
		if p.OpenStack.SecurityGroup.ID != "" && p.OpenStack.SecurityGroup.ManagedValue(true) {
			errs = append(errs, fmt.Sprintf("environment %q provider.openstack.security_group.id requires security_group.managed: false", envName))
		}
		if p.OpenStack.FloatingIP.Requested() && p.OpenStack.FloatingIP.NetworkID == "" && p.OpenStack.FloatingIP.Address == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.openstack.floating_ip.network_id or address is required when floating_ip is enabled", envName))
		}
		switch p.OpenStack.Interface {
		case "", "public", "internal", "admin":
		default:
			errs = append(errs, fmt.Sprintf("environment %q provider.openstack.interface must be public, internal, or admin", envName))
		}
		sshFirewall := p.OpenStack.EffectiveSSHFirewall()
		switch sshFirewall {
		case SSHFirewallManaged:
			if p.OpenStack.SecurityGroup.ManagedValue(true) && len(p.OpenStack.SSHAllowedCIDRs) == 0 {
				errs = append(errs, fmt.Sprintf("environment %q provider.openstack.ssh_allowed_cidrs is required when managed security group SSH is enabled", envName))
			}
		case SSHFirewallExternal, SSHFirewallDisabled:
		default:
			errs = append(errs, fmt.Sprintf("environment %q provider.openstack.ssh_firewall must be managed, external, or disabled", envName))
		}
	}
	if p.Civo != nil {
		errs = append(errs, validateUserDataConfig(fmt.Sprintf("environment %q provider.civo", envName), p.Civo.UserData, p.Civo.UserDataFile)...)
		if p.Civo.Region == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.civo.region is required", envName))
		}
		if p.Civo.Size == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.civo.size is required", envName))
		}
		if p.Civo.Image == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.civo.image is required", envName))
		}
		if p.Civo.NetworkID == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.civo.network_id is required", envName))
		}
		if p.Civo.Firewall.ID != "" && p.Civo.Firewall.ManagedValue(true) {
			errs = append(errs, fmt.Sprintf("environment %q provider.civo.firewall.id requires firewall.managed: false", envName))
		}
		if p.Civo.NetworkBandwidthLimit < 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.civo.network_bandwidth_limit cannot be negative", envName))
		}
		sshFirewall := p.Civo.EffectiveSSHFirewall()
		switch sshFirewall {
		case SSHFirewallManaged:
			if p.Civo.Firewall.ManagedValue(true) && len(p.Civo.SSHAllowedCIDRs) == 0 {
				errs = append(errs, fmt.Sprintf("environment %q provider.civo.ssh_allowed_cidrs is required when managed firewall SSH is enabled", envName))
			}
		case SSHFirewallExternal, SSHFirewallDisabled:
		default:
			errs = append(errs, fmt.Sprintf("environment %q provider.civo.ssh_firewall must be managed, external, or disabled", envName))
		}
	}
	if p.UpCloud != nil {
		errs = append(errs, validateUserDataConfig(fmt.Sprintf("environment %q provider.upcloud", envName), p.UpCloud.UserData, p.UpCloud.UserDataFile)...)
		if p.UpCloud.Zone == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.upcloud.zone is required", envName))
		}
		if p.UpCloud.Plan == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.upcloud.plan is required", envName))
		}
		if p.UpCloud.Template == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.upcloud.template is required", envName))
		}
		if len(p.UpCloud.SSHKeys) == 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.upcloud.ssh_keys is required", envName))
		}
		if p.UpCloud.StorageSizeGB < 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.upcloud.storage_size_gb cannot be negative", envName))
		}
		sshFirewall := p.UpCloud.EffectiveSSHFirewall()
		switch sshFirewall {
		case SSHFirewallManaged:
			if p.UpCloud.Firewall.ManagedValue(true) && len(p.UpCloud.SSHAllowedCIDRs) == 0 {
				errs = append(errs, fmt.Sprintf("environment %q provider.upcloud.ssh_allowed_cidrs is required when managed firewall SSH is enabled", envName))
			}
		case SSHFirewallExternal, SSHFirewallDisabled:
		default:
			errs = append(errs, fmt.Sprintf("environment %q provider.upcloud.ssh_firewall must be managed, external, or disabled", envName))
		}
	}
	if p.OVHCloud != nil {
		errs = append(errs, validateUserDataConfig(fmt.Sprintf("environment %q provider.ovhcloud", envName), p.OVHCloud.UserData, p.OVHCloud.UserDataFile)...)
		if p.OVHCloud.ServiceName == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.ovhcloud.service_name is required", envName))
		}
		if p.OVHCloud.Region == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.ovhcloud.region is required", envName))
		}
		if p.OVHCloud.FlavorID == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.ovhcloud.flavor_id is required", envName))
		}
		if p.OVHCloud.ImageID == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.ovhcloud.image_id is required", envName))
		}
		if p.OVHCloud.SSHKeyID == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.ovhcloud.ssh_key_id is required", envName))
		}
	}
	if p.OCI != nil {
		errs = append(errs, validateUserDataConfig(fmt.Sprintf("environment %q provider.oci", envName), p.OCI.UserData, p.OCI.UserDataFile)...)
		if p.OCI.Region == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.oci.region is required", envName))
		}
		if p.OCI.CompartmentID == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.oci.compartment_id is required", envName))
		}
		if p.OCI.AvailabilityDomain == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.oci.availability_domain is required", envName))
		}
		if p.OCI.Shape == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.oci.shape is required", envName))
		}
		if p.OCI.ImageID == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.oci.image_id is required", envName))
		}
		if p.OCI.SubnetID == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.oci.subnet_id is required", envName))
		}
		if len(p.OCI.SSHAuthorizedKeys) == 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.oci.ssh_authorized_keys is required", envName))
		}
		if p.OCI.NetworkSecurityGroup.ID != "" && p.OCI.NetworkSecurityGroup.ManagedValue(true) {
			errs = append(errs, fmt.Sprintf("environment %q provider.oci.network_security_group.id requires network_security_group.managed: false", envName))
		}
		if p.OCI.BootVolumeSizeGB < 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.oci.boot_volume_size_gb cannot be negative", envName))
		}
		if p.OCI.ShapeConfig.OCPUs < 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.oci.shape_config.ocpus cannot be negative", envName))
		}
		if p.OCI.ShapeConfig.MemoryGB < 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.oci.shape_config.memory_gb cannot be negative", envName))
		}
		sshFirewall := p.OCI.EffectiveSSHFirewall()
		switch sshFirewall {
		case SSHFirewallManaged:
			if p.OCI.NetworkSecurityGroup.ManagedValue(true) {
				if p.OCI.NetworkSecurityGroup.VCNID == "" {
					errs = append(errs, fmt.Sprintf("environment %q provider.oci.network_security_group.vcn_id is required when managed network security group is enabled", envName))
				}
				if len(p.OCI.SSHAllowedCIDRs) == 0 {
					errs = append(errs, fmt.Sprintf("environment %q provider.oci.ssh_allowed_cidrs is required when managed network security group SSH is enabled", envName))
				}
			}
		case SSHFirewallExternal, SSHFirewallDisabled:
		default:
			errs = append(errs, fmt.Sprintf("environment %q provider.oci.ssh_firewall must be managed, external, or disabled", envName))
		}
	}
	if p.Exoscale != nil {
		errs = append(errs, validateUserDataConfig(fmt.Sprintf("environment %q provider.exoscale", envName), p.Exoscale.UserData, p.Exoscale.UserDataFile)...)
		if p.Exoscale.Zone == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.exoscale.zone is required", envName))
		}
		if p.Exoscale.InstanceType == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.exoscale.instance_type is required", envName))
		}
		if p.Exoscale.Template == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.exoscale.template is required", envName))
		}
		if p.Exoscale.DiskSizeGB < 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.exoscale.disk_size_gb cannot be negative", envName))
		}
		if len(p.Exoscale.SSHKeys) == 0 && p.Exoscale.SSHKey == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.exoscale.ssh_keys is required", envName))
		}
		if p.Exoscale.SecurityGroup.ID != "" && p.Exoscale.SecurityGroup.ManagedValue(true) {
			errs = append(errs, fmt.Sprintf("environment %q provider.exoscale.security_group.id requires security_group.managed: false", envName))
		}
		switch p.Exoscale.EffectivePublicIPAssignment() {
		case "", "inet4", "dual", "none":
		default:
			errs = append(errs, fmt.Sprintf("environment %q provider.exoscale.public_ip_assignment must be inet4, dual, or none", envName))
		}
		sshFirewall := p.Exoscale.EffectiveSSHFirewall()
		switch sshFirewall {
		case SSHFirewallManaged:
			if p.Exoscale.SecurityGroup.ManagedValue(true) && len(p.Exoscale.SSHAllowedCIDRs) == 0 {
				errs = append(errs, fmt.Sprintf("environment %q provider.exoscale.ssh_allowed_cidrs is required when managed security group SSH is enabled", envName))
			}
		case SSHFirewallExternal, SSHFirewallDisabled:
		default:
			errs = append(errs, fmt.Sprintf("environment %q provider.exoscale.ssh_firewall must be managed, external, or disabled", envName))
		}
	}
	if p.Cloudscale != nil {
		errs = append(errs, validateUserDataConfig(fmt.Sprintf("environment %q provider.cloudscale", envName), p.Cloudscale.UserData, p.Cloudscale.UserDataFile)...)
		if p.Cloudscale.Zone == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.cloudscale.zone is required", envName))
		}
		if p.Cloudscale.Flavor == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.cloudscale.flavor is required", envName))
		}
		if p.Cloudscale.Image == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.cloudscale.image is required", envName))
		}
		if len(p.Cloudscale.SSHKeys) == 0 && p.Cloudscale.Password == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.cloudscale.ssh_keys or provider.cloudscale.password is required", envName))
		}
		switch p.Cloudscale.UserDataHandling {
		case "", "pass-through", "extend-cloud-config":
		default:
			errs = append(errs, fmt.Sprintf("environment %q provider.cloudscale.user_data_handling must be pass-through or extend-cloud-config", envName))
		}
		if p.Cloudscale.VolumeSizeGB < 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.cloudscale.volume_size_gb cannot be negative", envName))
		}
		if p.Cloudscale.BulkVolumeSizeGB < 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.cloudscale.bulk_volume_size_gb cannot be negative", envName))
		}
		for i, volume := range p.Cloudscale.Volumes {
			if volume.SizeGB <= 0 {
				errs = append(errs, fmt.Sprintf("environment %q provider.cloudscale.volumes[%d].size_gb must be positive", envName, i))
			}
		}
		if p.Cloudscale.ServerGroup.UUID != "" && p.Cloudscale.ServerGroup.ManagedValue(false) {
			errs = append(errs, fmt.Sprintf("environment %q provider.cloudscale.server_group.uuid requires server_group.managed: false", envName))
		}
		if len(p.Cloudscale.Interfaces) > 0 && (p.Cloudscale.UsePublicNetwork != nil || p.Cloudscale.UsePrivateNetwork != nil) {
			errs = append(errs, fmt.Sprintf("environment %q provider.cloudscale.interfaces cannot be combined with use_public_network or use_private_network", envName))
		}
		for i, iface := range p.Cloudscale.Interfaces {
			if iface.Network == "" && len(iface.Addresses) == 0 {
				errs = append(errs, fmt.Sprintf("environment %q provider.cloudscale.interfaces[%d].network or addresses is required", envName, i))
			}
		}
	}
	if p.Latitude != nil {
		errs = append(errs, validateUserDataConfig(fmt.Sprintf("environment %q provider.latitude", envName), p.Latitude.UserData, p.Latitude.UserDataFile)...)
		if p.Latitude.Project == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.latitude.project is required", envName))
		}
		if p.Latitude.Site == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.latitude.site is required", envName))
		}
		if p.Latitude.Plan == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.latitude.plan is required", envName))
		}
		if p.Latitude.OperatingSystem == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.latitude.operating_system is required", envName))
		}
		if len(p.Latitude.SSHKeys) == 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.latitude.ssh_keys is required", envName))
		}
		if p.Latitude.Firewall.ID != "" && p.Latitude.Firewall.ManagedValue(true) {
			errs = append(errs, fmt.Sprintf("environment %q provider.latitude.firewall.id requires firewall.managed: false", envName))
		}
		sshFirewall := p.Latitude.EffectiveSSHFirewall()
		switch sshFirewall {
		case SSHFirewallManaged:
			if p.Latitude.Firewall.ManagedValue(true) && len(p.Latitude.SSHAllowedCIDRs) == 0 {
				errs = append(errs, fmt.Sprintf("environment %q provider.latitude.ssh_allowed_cidrs is required when managed firewall SSH is enabled", envName))
			}
		case SSHFirewallExternal, SSHFirewallDisabled:
		default:
			errs = append(errs, fmt.Sprintf("environment %q provider.latitude.ssh_firewall must be managed, external, or disabled", envName))
		}
		switch p.Latitude.RAID {
		case "", "raid-0", "raid-1":
		default:
			errs = append(errs, fmt.Sprintf("environment %q provider.latitude.raid must be raid-0 or raid-1", envName))
		}
		switch p.Latitude.Billing {
		case "", "hourly", "monthly", "yearly":
		default:
			errs = append(errs, fmt.Sprintf("environment %q provider.latitude.billing must be hourly, monthly, or yearly", envName))
		}
		for i, disk := range p.Latitude.DiskLayout {
			if disk.Count <= 0 {
				errs = append(errs, fmt.Sprintf("environment %q provider.latitude.disk_layout[%d].count must be positive", envName, i))
			}
			switch disk.Role {
			case "os", "storage", "raw":
			default:
				errs = append(errs, fmt.Sprintf("environment %q provider.latitude.disk_layout[%d].role must be os, storage, or raw", envName, i))
			}
			switch disk.RAIDLevel {
			case "", "raid-0", "raid-1":
			default:
				errs = append(errs, fmt.Sprintf("environment %q provider.latitude.disk_layout[%d].raid_level must be raid-0 or raid-1", envName, i))
			}
			switch disk.Filesystem {
			case "", "ext4", "xfs":
			default:
				errs = append(errs, fmt.Sprintf("environment %q provider.latitude.disk_layout[%d].filesystem must be ext4 or xfs", envName, i))
			}
		}
		if p.Latitude.OperatingSystem == "ipxe" && p.Latitude.IPXE == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.latitude.ipxe is required when operating_system is ipxe", envName))
		}
	}
	if p.Kamatera != nil {
		if p.Kamatera.Datacenter == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.kamatera.datacenter is required", envName))
		}
		if p.Kamatera.CPU == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.kamatera.cpu is required", envName))
		}
		if p.Kamatera.RAMMB <= 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.kamatera.ram_mb must be positive", envName))
		}
		if p.Kamatera.Image == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.kamatera.image is required", envName))
		}
		if p.Kamatera.DiskGB <= 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.kamatera.disk_gb must be positive", envName))
		}
		if p.Kamatera.NetworkBits < 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.kamatera.network_bits cannot be negative", envName))
		}
		if p.Kamatera.WaitTimeoutSeconds < 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.kamatera.wait_timeout_seconds cannot be negative", envName))
		}
		if p.Kamatera.PollIntervalSeconds < 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.kamatera.poll_interval_seconds cannot be negative", envName))
		}
	}
	if p.Proxmox != nil {
		if p.Proxmox.APIURL == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.proxmox.api_url is required", envName))
		}
		if p.Proxmox.Node == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.proxmox.node is required", envName))
		}
		if p.Proxmox.TemplateID == 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.proxmox.template_id is required", envName))
		}
		if p.Proxmox.TemplateID < 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.proxmox.template_id cannot be negative", envName))
		}
		if p.Proxmox.MemoryMB < 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.proxmox.memory_mb cannot be negative", envName))
		}
		if p.Proxmox.Cores < 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.proxmox.cores cannot be negative", envName))
		}
		if p.Proxmox.VLAN < 0 {
			errs = append(errs, fmt.Sprintf("environment %q provider.proxmox.vlan cannot be negative", envName))
		}
	}
	if p.Terraform != nil {
		if p.Terraform.Output == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.terraform.output is required", envName))
		}
	}
	if p.Pulumi != nil {
		if p.Pulumi.Output == "" {
			errs = append(errs, fmt.Sprintf("environment %q provider.pulumi.output is required", envName))
		}
	}
	if p.Ansible != nil {
		sources := 0
		if p.Ansible.InventoryFile != "" {
			sources++
		}
		if len(p.Ansible.Command) > 0 {
			sources++
		}
		if sources != 1 {
			errs = append(errs, fmt.Sprintf("environment %q provider.ansible must define exactly one inventory source (inventory_file or command)", envName))
		}
	}
	return errs
}

func (p ProviderConfig) ManualHostsRequired() bool {
	return p.Manual != nil
}

func (h HetznerConfig) EffectiveSSHFirewall() string {
	value := strings.TrimSpace(h.SSHFirewall)
	if value == "" {
		return SSHFirewallManaged
	}
	return value
}

func (v VultrConfig) EffectiveSSHFirewall() string {
	value := strings.TrimSpace(v.SSHFirewall)
	if value == "" {
		return SSHFirewallManaged
	}
	return value
}

func (d DigitalOceanConfig) EffectiveSSHFirewall() string {
	value := strings.TrimSpace(d.SSHFirewall)
	if value == "" {
		return SSHFirewallManaged
	}
	return value
}

func (l LinodeConfig) EffectiveSSHFirewall() string {
	value := strings.TrimSpace(l.SSHFirewall)
	if value == "" {
		return SSHFirewallManaged
	}
	return value
}

func (a AWSConfig) EffectiveSSHFirewall() string {
	value := strings.TrimSpace(a.SSHFirewall)
	if value == "" {
		return SSHFirewallManaged
	}
	return value
}

func (l LightsailConfig) EffectiveSSHFirewall() string {
	value := strings.TrimSpace(l.SSHFirewall)
	if value == "" {
		return SSHFirewallManaged
	}
	return value
}

func (g GCPConfig) EffectiveSSHFirewall() string {
	value := strings.TrimSpace(g.SSHFirewall)
	if value == "" {
		return SSHFirewallManaged
	}
	return value
}

func (a AzureConfig) EffectiveSSHFirewall() string {
	value := strings.TrimSpace(a.SSHFirewall)
	if value == "" {
		return SSHFirewallManaged
	}
	return value
}

func (s ScalewayConfig) EffectiveSSHFirewall() string {
	value := strings.TrimSpace(s.SSHFirewall)
	if value == "" {
		return SSHFirewallManaged
	}
	return value
}

func (o OpenStackConfig) EffectiveSSHFirewall() string {
	value := strings.TrimSpace(o.SSHFirewall)
	if value == "" {
		return SSHFirewallManaged
	}
	return value
}

func (c CivoConfig) EffectiveSSHFirewall() string {
	value := strings.TrimSpace(c.SSHFirewall)
	if value == "" {
		return SSHFirewallManaged
	}
	return value
}

func (u UpCloudConfig) EffectiveSSHFirewall() string {
	value := strings.TrimSpace(u.SSHFirewall)
	if value == "" {
		return SSHFirewallManaged
	}
	return value
}

func (o OCIConfig) EffectiveSSHFirewall() string {
	value := strings.TrimSpace(o.SSHFirewall)
	if value == "" {
		return SSHFirewallManaged
	}
	return value
}

func (e ExoscaleConfig) EffectiveSSHFirewall() string {
	value := strings.TrimSpace(e.SSHFirewall)
	if value == "" {
		return SSHFirewallManaged
	}
	return value
}

func (l LatitudeConfig) EffectiveSSHFirewall() string {
	value := strings.TrimSpace(l.SSHFirewall)
	if value == "" {
		return SSHFirewallManaged
	}
	return value
}

func (n HetznerNetworkConfig) EnabledValue(def bool) bool {
	if n.Enabled == nil {
		return def
	}
	return *n.Enabled
}

func (f HetznerFirewallConfig) EnabledValue(def bool) bool {
	if f.Enabled == nil {
		return def
	}
	return *f.Enabled
}

func (f VultrFirewallConfig) EnabledValue(def bool) bool {
	if f.Enabled == nil {
		return def
	}
	return *f.Enabled
}

func (f DigitalOceanFirewallConfig) EnabledValue(def bool) bool {
	if f.Enabled == nil {
		return def
	}
	return *f.Enabled
}

func (f LinodeFirewallConfig) EnabledValue(def bool) bool {
	if f.Enabled == nil {
		return def
	}
	return *f.Enabled
}

func (s AWSSecurityGroupConfig) ManagedValue(def bool) bool {
	if s.Managed == nil {
		return def
	}
	return *s.Managed
}

func (f LightsailFirewallConfig) ManagedValue(def bool) bool {
	if f.Managed == nil {
		return def
	}
	return *f.Managed
}

func (f GCPFirewallConfig) ManagedValue(def bool) bool {
	if f.Managed == nil {
		return def
	}
	return *f.Managed
}

func (s AzureSecurityGroupConfig) ManagedValue(def bool) bool {
	if s.Managed == nil {
		return def
	}
	return *s.Managed
}

func (s ScalewaySecurityGroup) ManagedValue(def bool) bool {
	if s.Managed == nil {
		return def
	}
	return *s.Managed
}

func (s OpenStackSecurityGroupConfig) ManagedValue(def bool) bool {
	if s.Managed == nil {
		return def
	}
	return *s.Managed
}

func (f CivoFirewallConfig) ManagedValue(def bool) bool {
	if f.Managed == nil {
		return def
	}
	return *f.Managed
}

func (f UpCloudFirewallConfig) ManagedValue(def bool) bool {
	if f.Managed == nil {
		return def
	}
	return *f.Managed
}

func (n OCINetworkSecurityGroup) ManagedValue(def bool) bool {
	if n.Managed == nil {
		return def
	}
	return *n.Managed
}

func (s ExoscaleSecurityGroupConfig) ManagedValue(def bool) bool {
	if s.Managed == nil {
		return def
	}
	return *s.Managed
}

func (s CloudscaleServerGroupConfig) ManagedValue(def bool) bool {
	if s.Managed == nil {
		return def
	}
	return *s.Managed
}

func (f LatitudeFirewall) ManagedValue(def bool) bool {
	if f.Managed == nil {
		return def
	}
	return *f.Managed
}

func (e ExoscaleConfig) EffectiveDiskSizeGB(def int) int {
	if e.DiskSizeGB == 0 {
		return def
	}
	return e.DiskSizeGB
}

func (c CloudscaleConfig) EffectiveVolumeSizeGB(def int) int {
	if c.VolumeSizeGB == 0 {
		return def
	}
	return c.VolumeSizeGB
}

func (e ExoscaleConfig) EffectivePublicIPAssignment() string {
	value := strings.TrimSpace(e.PublicIPAssignment)
	if value == "" {
		return ""
	}
	return value
}

func (o OCIConfig) AssignPublicIPValue(def bool) bool {
	if o.AssignPublicIP == nil {
		return def
	}
	return *o.AssignPublicIP
}

func (o OCIConfig) PreserveBootVolumeValue(def bool) bool {
	if o.PreserveBootVolume == nil {
		return def
	}
	return *o.PreserveBootVolume
}

func (o OVHCloudConfig) MonthlyBillingValue(def bool) bool {
	if o.MonthlyBilling == nil {
		return def
	}
	return *o.MonthlyBilling
}

func (f OpenStackFloatingIPConfig) EnabledValue(def bool) bool {
	if f.Enabled == nil {
		return def
	}
	return *f.Enabled
}

func (f OpenStackFloatingIPConfig) Requested() bool {
	if f.Enabled != nil {
		return *f.Enabled
	}
	return f.NetworkID != "" ||
		f.SubnetID != "" ||
		f.Address != "" ||
		f.FixedIPAddress != "" ||
		f.Description != "" ||
		f.DNSName != "" ||
		f.DNSDomain != "" ||
		f.QOSPolicyID != "" ||
		f.Distributed != nil
}

func (p ProviderConfig) blocks() []string {
	var blocks []string
	if p.Hetzner != nil {
		blocks = append(blocks, ProviderHetzner)
	}
	if p.Vultr != nil {
		blocks = append(blocks, ProviderVultr)
	}
	if p.DigitalOcean != nil {
		blocks = append(blocks, ProviderDigitalOcean)
	}
	if p.Linode != nil {
		blocks = append(blocks, ProviderLinode)
	}
	if p.AWS != nil {
		blocks = append(blocks, ProviderAWS)
	}
	if p.Lightsail != nil {
		blocks = append(blocks, ProviderLightsail)
	}
	if p.GCP != nil {
		blocks = append(blocks, ProviderGCP)
	}
	if p.Azure != nil {
		blocks = append(blocks, ProviderAzure)
	}
	if p.Scaleway != nil {
		blocks = append(blocks, ProviderScaleway)
	}
	if p.OpenStack != nil {
		blocks = append(blocks, ProviderOpenStack)
	}
	if p.Civo != nil {
		blocks = append(blocks, ProviderCivo)
	}
	if p.UpCloud != nil {
		blocks = append(blocks, ProviderUpCloud)
	}
	if p.OVHCloud != nil {
		blocks = append(blocks, ProviderOVHCloud)
	}
	if p.OCI != nil {
		blocks = append(blocks, ProviderOCI)
	}
	if p.Exoscale != nil {
		blocks = append(blocks, ProviderExoscale)
	}
	if p.Cloudscale != nil {
		blocks = append(blocks, ProviderCloudscale)
	}
	if p.Latitude != nil {
		blocks = append(blocks, ProviderLatitude)
	}
	if p.Kamatera != nil {
		blocks = append(blocks, ProviderKamatera)
	}
	if p.Proxmox != nil {
		blocks = append(blocks, ProviderProxmox)
	}
	if p.SSHConfig != nil {
		blocks = append(blocks, ProviderSSHConfig)
	}
	if p.Terraform != nil {
		blocks = append(blocks, ProviderTerraform)
	}
	if p.Pulumi != nil {
		blocks = append(blocks, ProviderPulumi)
	}
	if p.Ansible != nil {
		blocks = append(blocks, ProviderAnsible)
	}
	if p.Manual != nil {
		blocks = append(blocks, ProviderManual)
	}
	blocks = append(blocks, p.Unknown...)
	sort.Strings(blocks)
	return blocks
}

func (v VultrConfig) sourceBlocks() []string {
	var blocks []string
	if v.OSID != 0 {
		blocks = append(blocks, "os_id")
	}
	if v.ImageID != "" {
		blocks = append(blocks, "image_id")
	}
	if v.SnapshotID != "" {
		blocks = append(blocks, "snapshot_id")
	}
	if v.AppID != 0 {
		blocks = append(blocks, "app_id")
	}
	return blocks
}

type HostsConfig struct {
	Labels map[string]string `yaml:"labels"`
	Pools  map[string]Pool   `yaml:"pools"`
}

type DockerConfig struct {
	Network DockerNetworkConfig `yaml:"network"`
}

type DockerNetworkConfig struct {
	Enabled *bool  `yaml:"enabled"`
	Name    string `yaml:"name"`
	Driver  string `yaml:"driver"`
}

func (n DockerNetworkConfig) EnabledValue(def bool) bool {
	if n.Enabled != nil {
		return *n.Enabled
	}
	return def
}

type SSHConfig struct {
	Port           int               `yaml:"port"`
	IdentityFile   string            `yaml:"identity_file"`
	KnownHostsFile string            `yaml:"known_hosts_file"`
	JumpHost       string            `yaml:"jump_host"`
	Options        map[string]string `yaml:"options"`
}

type Pool struct {
	Count        int               `yaml:"count"`
	Hosts        []string          `yaml:"hosts"`
	User         string            `yaml:"user"`
	SSH          SSHConfig         `yaml:"ssh"`
	Location     string            `yaml:"location"`
	Size         string            `yaml:"size"`
	Image        string            `yaml:"image"`
	UserData     string            `yaml:"user_data"`
	UserDataFile string            `yaml:"user_data_file"`
	Labels       map[string]string `yaml:"labels"`
}

type Service struct {
	Image          ImageSpec           `yaml:"image"`
	Command        string              `yaml:"command"`
	Pool           string              `yaml:"pool"`
	Scale          int                 `yaml:"scale"`
	Labels         map[string]string   `yaml:"labels"`
	NetworkAliases []string            `yaml:"network_aliases"`
	Ports          []int               `yaml:"ports"`
	Publish        []string            `yaml:"publish"`
	Health         HealthCheck         `yaml:"health"`
	Ingress        *Ingress            `yaml:"ingress"`
	Env            []string            `yaml:"env"`
	Secrets        []string            `yaml:"secrets"`
	RestartPolicy  string              `yaml:"restart_policy"`
	Volumes        []string            `yaml:"volumes"`
	Logging        LoggingConfig       `yaml:"logging"`
	Resources      ResourceConfig      `yaml:"resources"`
	Runtime        RuntimeConfig       `yaml:"runtime"`
	Rolling        Rolling             `yaml:"rolling"`
	Release        ReleaseCommand      `yaml:"release"`
	Schedules      map[string]Schedule `yaml:"schedules"`
}

type ImageSpec struct {
	Build         string            `yaml:"build"`
	Dockerfile    string            `yaml:"dockerfile"`
	Ref           string            `yaml:"ref"`
	Tags          []string          `yaml:"tags"`
	BuildArgs     map[string]string `yaml:"build_args"`
	Target        string            `yaml:"target"`
	Builder       string            `yaml:"builder"`
	Buildpack     BuildpackConfig   `yaml:"buildpack"`
	Platform      string            `yaml:"platform"`
	Platforms     []string          `yaml:"platforms"`
	Pull          bool              `yaml:"pull"`
	NoCache       bool              `yaml:"no_cache"`
	NoCacheFilter []string          `yaml:"no_cache_filter"`
	CacheFrom     []string          `yaml:"cache_from"`
	CacheTo       []string          `yaml:"cache_to"`
	Secrets       []string          `yaml:"secrets"`
	SSH           []string          `yaml:"ssh"`
	SBOM          BuildxFlag        `yaml:"sbom"`
	Provenance    BuildxFlag        `yaml:"provenance"`
}

type BuildpackConfig struct {
	Builder      string            `yaml:"builder"`
	Buildpacks   []string          `yaml:"buildpacks"`
	Env          map[string]string `yaml:"env"`
	Descriptor   string            `yaml:"descriptor"`
	Publish      bool              `yaml:"publish"`
	PullPolicy   string            `yaml:"pull_policy"`
	TrustBuilder bool              `yaml:"trust_builder"`
}

func (b BuildpackConfig) Enabled() bool {
	return strings.TrimSpace(b.Builder) != "" ||
		len(b.Buildpacks) > 0 ||
		len(b.Env) > 0 ||
		strings.TrimSpace(b.Descriptor) != "" ||
		b.Publish ||
		strings.TrimSpace(b.PullPolicy) != "" ||
		b.TrustBuilder
}

type BuildxFlag string

func (f *BuildxFlag) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		switch value.Tag {
		case "!!bool":
			if value.Value == "true" {
				*f = "true"
				return nil
			}
			*f = ""
			return nil
		case "!!str", "":
			*f = BuildxFlag(strings.TrimSpace(value.Value))
			return nil
		default:
			return fmt.Errorf("must be a boolean or string")
		}
	default:
		return fmt.Errorf("must be a scalar")
	}
}

func (f BuildxFlag) Value() string {
	return strings.TrimSpace(string(f))
}

func (f BuildxFlag) Enabled() bool {
	value := f.Value()
	return value != "" && value != "false"
}

type HealthCheck struct {
	HTTP    string `yaml:"http"`
	Command string `yaml:"command"`
}

type Ingress struct {
	Domains   []string          `yaml:"domains"`
	Redirects []IngressRedirect `yaml:"redirects"`
	Health    IngressHealth     `yaml:"health"`
}

type IngressRedirect struct {
	From        []string `yaml:"from"`
	To          string   `yaml:"to"`
	Code        int      `yaml:"code"`
	PreserveURI *bool    `yaml:"preserve_uri"`
}

type IngressHealth struct {
	Enabled                    *bool    `yaml:"enabled"`
	Path                       string   `yaml:"path"`
	IntervalSeconds            int      `yaml:"interval_seconds"`
	TimeoutSeconds             int      `yaml:"timeout_seconds"`
	Passes                     int      `yaml:"passes"`
	Fails                      int      `yaml:"fails"`
	TryDurationSeconds         int      `yaml:"try_duration_seconds"`
	PassiveFailDurationSeconds int      `yaml:"passive_fail_duration_seconds"`
	PassiveMaxFails            int      `yaml:"passive_max_fails"`
	UnhealthyStatus            []string `yaml:"unhealthy_status"`
}

type LoggingConfig struct {
	Driver  string            `yaml:"driver"`
	Options map[string]string `yaml:"options"`
}

type ResourceConfig struct {
	CPUs              string `yaml:"cpus"`
	Memory            string `yaml:"memory"`
	MemoryReservation string `yaml:"memory_reservation"`
	MemorySwap        string `yaml:"memory_swap"`
	CPUShares         int    `yaml:"cpu_shares"`
	CPUSetCPUs        string `yaml:"cpuset_cpus"`
	PIDsLimit         int    `yaml:"pids_limit"`
}

type RuntimeConfig struct {
	Privileged         bool              `yaml:"privileged"`
	ReadOnly           bool              `yaml:"read_only"`
	Init               bool              `yaml:"init"`
	User               string            `yaml:"user"`
	Workdir            string            `yaml:"workdir"`
	Hostname           string            `yaml:"hostname"`
	Entrypoint         string            `yaml:"entrypoint"`
	IPC                string            `yaml:"ipc"`
	PID                string            `yaml:"pid"`
	CgroupNS           string            `yaml:"cgroupns"`
	StopSignal         string            `yaml:"stop_signal"`
	StopTimeoutSeconds int               `yaml:"stop_timeout_seconds"`
	ShmSize            string            `yaml:"shm_size"`
	GPUs               string            `yaml:"gpus"`
	NoHealthcheck      bool              `yaml:"no_healthcheck"`
	HealthCMD          string            `yaml:"health_cmd"`
	HealthInterval     string            `yaml:"health_interval"`
	HealthTimeout      string            `yaml:"health_timeout"`
	HealthStartPeriod  string            `yaml:"health_start_period"`
	HealthRetries      int               `yaml:"health_retries"`
	CapAdd             []string          `yaml:"cap_add"`
	CapDrop            []string          `yaml:"cap_drop"`
	GroupAdd           []string          `yaml:"group_add"`
	SecurityOpt        []string          `yaml:"security_opt"`
	Sysctls            map[string]string `yaml:"sysctls"`
	Ulimits            []string          `yaml:"ulimits"`
	Mounts             []string          `yaml:"mounts"`
	AddHosts           []string          `yaml:"add_hosts"`
	DNS                []string          `yaml:"dns"`
	DNSSearch          []string          `yaml:"dns_search"`
	DNSOptions         []string          `yaml:"dns_options"`
	Devices            []string          `yaml:"devices"`
	DeviceCgroupRules  []string          `yaml:"device_cgroup_rules"`
	Tmpfs              []string          `yaml:"tmpfs"`
}

type IngressConfig struct {
	Caddy CaddyConfig `yaml:"caddy"`
}

type CaddyConfig struct {
	Image        string `yaml:"image"`
	Email        string `yaml:"email"`
	DataVolume   string `yaml:"data_volume"`
	ConfigVolume string `yaml:"config_volume"`
}

type Rolling struct {
	MaxUnavailable        int `yaml:"max_unavailable"`
	MaxSurge              int `yaml:"max_surge"`
	CanaryReplicas        int `yaml:"canary_replicas"`
	CanaryPauseSeconds    int `yaml:"canary_pause_seconds"`
	DrainTimeoutSeconds   int `yaml:"drain_timeout_seconds"`
	HealthTimeoutSeconds  int `yaml:"health_timeout_seconds"`
	HealthRetries         int `yaml:"health_retries"`
	HealthIntervalSeconds int `yaml:"health_interval_seconds"`
}

type Schedule struct {
	Cron           string `yaml:"cron"`
	Command        string `yaml:"command"`
	Replica        int    `yaml:"replica"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

type ReleaseCommand struct {
	Command        string `yaml:"command"`
	Replica        int    `yaml:"replica"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

type Hooks struct {
	PreDeploy    []HookCommand `yaml:"pre_deploy"`
	PreBuild     []HookCommand `yaml:"pre_build"`
	PostDeploy   []HookCommand `yaml:"post_deploy"`
	DeployFailed []HookCommand `yaml:"deploy_failed"`
}

type Notifications struct {
	Webhooks []WebhookNotification `yaml:"webhooks"`
}

type WebhookNotification struct {
	URL            string            `yaml:"url"`
	URLEnv         string            `yaml:"url_env"`
	Events         []string          `yaml:"events"`
	Headers        map[string]string `yaml:"headers"`
	TimeoutSeconds int               `yaml:"timeout_seconds"`
}

type HookCommand struct {
	Command        string            `yaml:"command"`
	TimeoutSeconds int               `yaml:"timeout_seconds"`
	Env            map[string]string `yaml:"env"`
}

func (h *HookCommand) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var command string
		if err := value.Decode(&command); err != nil {
			return err
		}
		h.Command = command
		return nil
	case yaml.MappingNode:
		type hookCommand HookCommand
		var decoded hookCommand
		if err := value.Decode(&decoded); err != nil {
			return err
		}
		*h = HookCommand(decoded)
		return nil
	default:
		return fmt.Errorf("hook command must be a string or mapping")
	}
}

type Accessory struct {
	Image          string            `yaml:"image"`
	Command        string            `yaml:"command"`
	Pool           string            `yaml:"pool"`
	Primary        bool              `yaml:"primary"`
	Labels         map[string]string `yaml:"labels"`
	NetworkAliases []string          `yaml:"network_aliases"`
	Volumes        []string          `yaml:"volumes"`
	VolumeOwner    string            `yaml:"volume_owner"`
	RestartPolicy  string            `yaml:"restart_policy"`
	Resources      ResourceConfig    `yaml:"resources"`
	Ports          []int             `yaml:"ports"`
	Publish        []string          `yaml:"publish"`
	Runtime        RuntimeConfig     `yaml:"runtime"`
	Env            []string          `yaml:"env"`
	Secrets        []string          `yaml:"secrets"`
	Backup         BackupSpec        `yaml:"backup"`
}

type BackupSpec struct {
	Command              string         `yaml:"command"`
	ExportCommand        string         `yaml:"export_command"`
	RestoreCommand       string         `yaml:"restore_command"`
	ArtifactDir          string         `yaml:"artifact_dir"`
	Required             bool           `yaml:"required"`
	RestoreCheck         bool           `yaml:"restore_check"`
	ExportTimeoutSeconds int            `yaml:"export_timeout_seconds"`
	Schedule             BackupSchedule `yaml:"schedule"`
}

type BackupSchedule struct {
	Cron           string `yaml:"cron"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigFile
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	baseDir := filepath.Dir(path)
	if absPath, err := filepath.Abs(path); err == nil {
		baseDir = filepath.Dir(absPath)
	}
	if err := cfg.ResolveUserDataFiles(baseDir); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) ResolveUserDataFiles(baseDir string) error {
	if c == nil {
		return errors.New("config is required")
	}
	for envName, env := range c.Environments {
		if err := resolveProviderUserDataFiles(baseDir, "environment "+envName, &env.Provider); err != nil {
			return err
		}
		for poolName, pool := range env.Hosts.Pools {
			if err := resolveUserDataFile(baseDir, fmt.Sprintf("environment %q pool %q", envName, poolName), &pool.UserData, &pool.UserDataFile); err != nil {
				return err
			}
			env.Hosts.Pools[poolName] = pool
		}
		c.Environments[envName] = env
	}
	return nil
}

func resolveProviderUserDataFiles(baseDir, label string, provider *ProviderConfig) error {
	if provider == nil {
		return nil
	}
	if provider.Hetzner != nil {
		return resolveUserDataFile(baseDir, label+" provider.hetzner", &provider.Hetzner.UserData, &provider.Hetzner.UserDataFile)
	}
	if provider.Vultr != nil {
		return resolveUserDataFile(baseDir, label+" provider.vultr", &provider.Vultr.UserData, &provider.Vultr.UserDataFile)
	}
	if provider.DigitalOcean != nil {
		return resolveUserDataFile(baseDir, label+" provider.digitalocean", &provider.DigitalOcean.UserData, &provider.DigitalOcean.UserDataFile)
	}
	if provider.Linode != nil {
		return resolveUserDataFile(baseDir, label+" provider.linode", &provider.Linode.UserData, &provider.Linode.UserDataFile)
	}
	if provider.AWS != nil {
		return resolveUserDataFile(baseDir, label+" provider.aws", &provider.AWS.UserData, &provider.AWS.UserDataFile)
	}
	if provider.Lightsail != nil {
		return resolveUserDataFile(baseDir, label+" provider.lightsail", &provider.Lightsail.UserData, &provider.Lightsail.UserDataFile)
	}
	if provider.GCP != nil {
		return resolveUserDataFile(baseDir, label+" provider.gcp", &provider.GCP.UserData, &provider.GCP.UserDataFile)
	}
	if provider.Azure != nil {
		return resolveUserDataFile(baseDir, label+" provider.azure", &provider.Azure.UserData, &provider.Azure.UserDataFile)
	}
	if provider.Scaleway != nil {
		return resolveUserDataFile(baseDir, label+" provider.scaleway", &provider.Scaleway.UserData, &provider.Scaleway.UserDataFile)
	}
	if provider.OpenStack != nil {
		return resolveUserDataFile(baseDir, label+" provider.openstack", &provider.OpenStack.UserData, &provider.OpenStack.UserDataFile)
	}
	if provider.Civo != nil {
		return resolveUserDataFile(baseDir, label+" provider.civo", &provider.Civo.UserData, &provider.Civo.UserDataFile)
	}
	if provider.UpCloud != nil {
		return resolveUserDataFile(baseDir, label+" provider.upcloud", &provider.UpCloud.UserData, &provider.UpCloud.UserDataFile)
	}
	if provider.OVHCloud != nil {
		return resolveUserDataFile(baseDir, label+" provider.ovhcloud", &provider.OVHCloud.UserData, &provider.OVHCloud.UserDataFile)
	}
	if provider.OCI != nil {
		return resolveUserDataFile(baseDir, label+" provider.oci", &provider.OCI.UserData, &provider.OCI.UserDataFile)
	}
	if provider.Exoscale != nil {
		return resolveUserDataFile(baseDir, label+" provider.exoscale", &provider.Exoscale.UserData, &provider.Exoscale.UserDataFile)
	}
	if provider.Cloudscale != nil {
		return resolveUserDataFile(baseDir, label+" provider.cloudscale", &provider.Cloudscale.UserData, &provider.Cloudscale.UserDataFile)
	}
	if provider.Latitude != nil {
		return resolveUserDataFile(baseDir, label+" provider.latitude", &provider.Latitude.UserData, &provider.Latitude.UserDataFile)
	}
	return nil
}

func resolveUserDataFile(baseDir, label string, userData, userDataFile *string) error {
	if userData == nil || userDataFile == nil {
		return nil
	}
	if strings.TrimSpace(*userData) != "" && strings.TrimSpace(*userDataFile) != "" {
		return fmt.Errorf("%s cannot define both user_data and user_data_file", label)
	}
	if strings.TrimSpace(*userDataFile) == "" {
		return nil
	}
	path := *userDataFile
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s read user_data_file: %w", label, err)
	}
	*userData = string(data)
	*userDataFile = ""
	return nil
}

func (c *Config) ResolveEnvironment(name string) (*Config, Environment, error) {
	if c == nil {
		return nil, Environment{}, errors.New("config is required")
	}
	env, err := c.Environment(name)
	if err != nil {
		return nil, Environment{}, err
	}
	resolved := *c
	resolved.Environments = map[string]Environment{name: env}
	resolved.Services = copyServices(c.Services)
	for serviceName, svc := range env.Services {
		if resolved.Services == nil {
			resolved.Services = map[string]Service{}
		}
		if base, ok := resolved.Services[serviceName]; ok {
			resolved.Services[serviceName] = mergeService(base, svc)
		} else {
			resolved.Services[serviceName] = svc
		}
	}
	resolved.Services = applyLoggingDefaults(resolved.Services, c.Logging)
	resolved.Accessories = copyAccessories(c.Accessories)
	for accessoryName, acc := range env.Accessories {
		if resolved.Accessories == nil {
			resolved.Accessories = map[string]Accessory{}
		}
		if base, ok := resolved.Accessories[accessoryName]; ok {
			resolved.Accessories[accessoryName] = mergeAccessory(base, acc)
		} else {
			resolved.Accessories[accessoryName] = acc
		}
	}
	runtimeDefaults := mergeRuntimeConfig(c.Runtime, env.Runtime)
	resolved.Services = applyRuntimeDefaultsToServices(resolved.Services, runtimeDefaults)
	resolved.Accessories = applyRuntimeDefaultsToAccessories(resolved.Accessories, runtimeDefaults)
	resolved.Secrets = mergeNames(c.Secrets, env.Secrets)
	resolved.Docker = mergeDockerConfig(c.Docker, env.Docker)
	resolved.Ingress = mergeIngressConfig(c.Ingress, env.Ingress)
	resolved.Hooks = mergeHooks(c.Hooks, env.Hooks)
	resolved.Notifications = mergeNotifications(c.Notifications, env.Notifications)
	env.SSH = mergeSSHConfig(c.SSH, env.SSH)
	if resolved.Ingress.Caddy.Image == "" {
		resolved.Ingress.Caddy.Image = DefaultCaddyImage
	}
	env.Services = nil
	env.Accessories = nil
	env.Secrets = nil
	env.Docker = resolved.Docker
	env.Ingress = resolved.Ingress
	env.Hooks = resolved.Hooks
	env.Notifications = resolved.Notifications
	resolved.Environments[name] = env
	if err := resolved.ValidateResolved(name); err != nil {
		return nil, Environment{}, err
	}
	return &resolved, env, nil
}

func (c *Config) ValidateResolved(envName string) error {
	if c == nil {
		return errors.New("config is required")
	}
	if len(c.Environments) != 1 {
		return fmt.Errorf("resolved config for %q must contain exactly one environment", envName)
	}
	return c.Validate()
}

func (c *Config) Validate() error {
	var errs []string
	if strings.TrimSpace(c.Project) == "" {
		errs = append(errs, "project is required")
	}
	if strings.TrimSpace(c.Registry) == "" {
		errs = append(errs, "registry is required")
	}
	errs = append(errs, validateSSHConfig("root ssh", c.SSH)...)
	errs = append(errs, validateDockerConfig("root docker", c.Docker)...)
	errs = append(errs, validateHooks("root hooks", c.Hooks)...)
	errs = append(errs, validateNotifications("root notifications", c.Notifications)...)
	errs = append(errs, validateLoggingConfig("root logging", c.Logging)...)
	errs = append(errs, validateRuntimeConfig("root runtime", c.Runtime)...)
	errs = append(errs, validateCaddyConfig("root ingress.caddy", c.Ingress.Caddy)...)
	if len(c.Environments) == 0 {
		errs = append(errs, "at least one environment is required")
	}
	if totalServiceCount(c) == 0 {
		errs = append(errs, "at least one service is required")
	}
	for envName, env := range c.Environments {
		errs = append(errs, validateSSHConfig(fmt.Sprintf("environment %q ssh", envName), env.SSH)...)
		errs = append(errs, validateDockerConfig(fmt.Sprintf("environment %q docker", envName), env.Docker)...)
		errs = append(errs, validateHooks(fmt.Sprintf("environment %q hooks", envName), env.Hooks)...)
		errs = append(errs, validateNotifications(fmt.Sprintf("environment %q notifications", envName), env.Notifications)...)
		errs = append(errs, validateRuntimeConfig(fmt.Sprintf("environment %q runtime", envName), env.Runtime)...)
		errs = append(errs, validateCaddyConfig(fmt.Sprintf("environment %q ingress.caddy", envName), env.Ingress.Caddy)...)
		errs = append(errs, env.Provider.Validate(envName)...)
		if len(env.Hosts.Pools) == 0 {
			errs = append(errs, fmt.Sprintf("environment %q must define hosts.pools", envName))
		}
		errs = append(errs, validateCustomHostLabels(fmt.Sprintf("environment %q hosts.labels", envName), env.Hosts.Labels)...)
		for poolName, pool := range env.Hosts.Pools {
			if pool.Count < 0 {
				errs = append(errs, fmt.Sprintf("environment %q pool %q count cannot be negative", envName, poolName))
			}
			errs = append(errs, validateSSHConfig(fmt.Sprintf("environment %q pool %q ssh", envName, poolName), pool.SSH)...)
			if env.Provider.ManualHostsRequired() && len(pool.Hosts) == 0 {
				errs = append(errs, fmt.Sprintf("environment %q manual provider requires pool %q to define hosts", envName, poolName))
			}
			if env.Provider.SSHConfig != nil && len(pool.Hosts) == 0 {
				errs = append(errs, fmt.Sprintf("environment %q ssh_config provider requires pool %q to define hosts", envName, poolName))
			}
			if env.Provider.AWS != nil && pool.Location != "" && pool.Location != env.Provider.AWS.Region {
				errs = append(errs, fmt.Sprintf("environment %q provider.aws pool %q location override is not supported; use separate environments for multi-region EC2", envName, poolName))
			}
			if env.Provider.Lightsail != nil && pool.Location != "" && !strings.HasPrefix(pool.Location, env.Provider.Lightsail.Region) {
				errs = append(errs, fmt.Sprintf("environment %q provider.lightsail pool %q location override must stay in region %q", envName, poolName, env.Provider.Lightsail.Region))
			}
			if env.Provider.GCP != nil && pool.Location != "" && pool.Location != env.Provider.GCP.Zone {
				errs = append(errs, fmt.Sprintf("environment %q provider.gcp pool %q location override is not supported; use separate environments for multi-zone Compute Engine", envName, poolName))
			}
			if env.Provider.Azure != nil && pool.Location != "" && pool.Location != env.Provider.Azure.Location {
				errs = append(errs, fmt.Sprintf("environment %q provider.azure pool %q location override is not supported; use separate environments for multi-region Azure", envName, poolName))
			}
			if env.Provider.Scaleway != nil && pool.Location != "" && pool.Location != env.Provider.Scaleway.Zone {
				errs = append(errs, fmt.Sprintf("environment %q provider.scaleway pool %q location override is not supported; use separate environments for multi-zone Scaleway", envName, poolName))
			}
			if env.Provider.OpenStack != nil && pool.Location != "" && pool.Location != env.Provider.OpenStack.Region {
				errs = append(errs, fmt.Sprintf("environment %q provider.openstack pool %q location override is not supported; use separate environments for multi-region OpenStack", envName, poolName))
			}
			if env.Provider.Civo != nil && pool.Location != "" && pool.Location != env.Provider.Civo.Region {
				errs = append(errs, fmt.Sprintf("environment %q provider.civo pool %q location override is not supported; use separate environments for multi-region Civo", envName, poolName))
			}
			if env.Provider.UpCloud != nil && pool.Location != "" && pool.Location != env.Provider.UpCloud.Zone {
				errs = append(errs, fmt.Sprintf("environment %q provider.upcloud pool %q location override is not supported; use separate environments for multi-zone UpCloud", envName, poolName))
			}
			if env.Provider.OVHCloud != nil && pool.Location != "" && pool.Location != env.Provider.OVHCloud.Region {
				errs = append(errs, fmt.Sprintf("environment %q provider.ovhcloud pool %q location override is not supported; use separate environments for multi-region OVHcloud", envName, poolName))
			}
			if env.Provider.OCI != nil && pool.Location != "" && pool.Location != env.Provider.OCI.AvailabilityDomain {
				errs = append(errs, fmt.Sprintf("environment %q provider.oci pool %q location override is not supported; use separate environments for multi-AD OCI", envName, poolName))
			}
			if env.Provider.Exoscale != nil && pool.Location != "" && pool.Location != env.Provider.Exoscale.Zone {
				errs = append(errs, fmt.Sprintf("environment %q provider.exoscale pool %q location override is not supported; use separate environments for multi-zone Exoscale", envName, poolName))
			}
			if env.Provider.Cloudscale != nil && pool.Location != "" && pool.Location != env.Provider.Cloudscale.Zone {
				errs = append(errs, fmt.Sprintf("environment %q provider.cloudscale pool %q location override is not supported; use separate environments for multi-zone cloudscale.ch", envName, poolName))
			}
			if env.Provider.Latitude != nil && pool.Location != "" && pool.Location != env.Provider.Latitude.Site {
				errs = append(errs, fmt.Sprintf("environment %q provider.latitude pool %q location override is not supported; use separate environments for multi-site Latitude.sh", envName, poolName))
			}
			if env.Provider.Proxmox != nil && pool.Location != "" && pool.Location != env.Provider.Proxmox.Node {
				errs = append(errs, fmt.Sprintf("environment %q provider.proxmox pool %q location override is not supported; use separate environments for multi-node Proxmox placement", envName, poolName))
			}
			if env.Provider.Kamatera != nil && (strings.TrimSpace(pool.UserData) != "" || strings.TrimSpace(pool.UserDataFile) != "") {
				errs = append(errs, fmt.Sprintf("environment %q provider.kamatera pool %q user_data is not supported by the Kamatera server create API", envName, poolName))
			}
			errs = append(errs, validateUserDataConfig(fmt.Sprintf("environment %q pool %q", envName, poolName), pool.UserData, pool.UserDataFile)...)
			errs = append(errs, validateCustomHostLabels(fmt.Sprintf("environment %q pool %q labels", envName, poolName), pool.Labels)...)
		}
	}
	errs = append(errs, validateServices("", c.Services)...)
	errs = append(errs, validateAccessories("", c.Accessories)...)
	errs = append(errs, validateSecretNames("root", c.Secrets)...)
	for envName, env := range c.Environments {
		errs = append(errs, validateSecretNames(fmt.Sprintf("environment %q", envName), env.Secrets)...)
	}
	for envName := range c.Environments {
		resolved, env, err := c.resolvedForValidation(envName)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if len(resolved.Services) == 0 {
			errs = append(errs, fmt.Sprintf("environment %q must resolve at least one service", envName))
		}
		errs = append(errs, validateServices(fmt.Sprintf("environment %q ", envName), resolved.Services)...)
		errs = append(errs, validateAccessories(fmt.Sprintf("environment %q ", envName), resolved.Accessories)...)
		for svcName, svc := range resolved.Services {
			if _, ok := env.Hosts.Pools[svc.Pool]; !ok {
				errs = append(errs, fmt.Sprintf("service %q references missing pool %q in environment %q", svcName, svc.Pool, envName))
			}
		}
		for accName, acc := range resolved.Accessories {
			if _, ok := env.Hosts.Pools[acc.Pool]; !ok {
				errs = append(errs, fmt.Sprintf("accessory %q references missing pool %q in environment %q", accName, acc.Pool, envName))
			}
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return errors.New(strings.Join(errs, "\n"))
	}
	return nil
}

func totalServiceCount(c *Config) int {
	if c == nil {
		return 0
	}
	total := len(c.Services)
	for _, env := range c.Environments {
		total += len(env.Services)
	}
	return total
}

func validateServices(prefix string, services map[string]Service) []string {
	var errs []string
	for name, svc := range services {
		label := prefix + fmt.Sprintf("service %q", name)
		errs = append(errs, validateSecretNames(label, svc.Secrets)...)
		if svc.Pool == "" {
			errs = append(errs, fmt.Sprintf("%s pool is required", label))
		}
		if svc.Scale < 0 {
			errs = append(errs, fmt.Sprintf("%s scale cannot be negative", label))
		}
		if svc.Rolling.MaxUnavailable < 0 {
			errs = append(errs, fmt.Sprintf("%s rolling.max_unavailable cannot be negative", label))
		}
		if svc.Rolling.MaxSurge < 0 {
			errs = append(errs, fmt.Sprintf("%s rolling.max_surge cannot be negative", label))
		}
		if svc.Rolling.CanaryReplicas < 0 {
			errs = append(errs, fmt.Sprintf("%s rolling.canary_replicas cannot be negative", label))
		}
		if svc.Rolling.CanaryReplicas > svc.Scale {
			errs = append(errs, fmt.Sprintf("%s rolling.canary_replicas %d exceeds service scale %d", label, svc.Rolling.CanaryReplicas, svc.Scale))
		}
		if svc.Rolling.CanaryPauseSeconds < 0 {
			errs = append(errs, fmt.Sprintf("%s rolling.canary_pause_seconds cannot be negative", label))
		}
		if svc.Rolling.DrainTimeoutSeconds < 0 {
			errs = append(errs, fmt.Sprintf("%s rolling.drain_timeout_seconds cannot be negative", label))
		}
		if svc.Rolling.HealthTimeoutSeconds < 0 {
			errs = append(errs, fmt.Sprintf("%s rolling.health_timeout_seconds cannot be negative", label))
		}
		if svc.Rolling.HealthRetries < 0 {
			errs = append(errs, fmt.Sprintf("%s rolling.health_retries cannot be negative", label))
		}
		if svc.Rolling.HealthIntervalSeconds < 0 {
			errs = append(errs, fmt.Sprintf("%s rolling.health_interval_seconds cannot be negative", label))
		}
		if strings.ContainsAny(svc.Health.HTTP, " \t\r\n") {
			errs = append(errs, fmt.Sprintf("%s health.http cannot contain whitespace", label))
		}
		errs = append(errs, validateBuildpackConfig(label+" image.buildpack", svc.Image.Buildpack)...)
		if svc.Image.Buildpack.Enabled() {
			if strings.TrimSpace(svc.Image.Build) == "" {
				errs = append(errs, fmt.Sprintf("%s image.buildpack requires image.build", label))
			}
			if strings.TrimSpace(svc.Image.Dockerfile) != "" {
				errs = append(errs, fmt.Sprintf("%s image.dockerfile cannot be combined with image.buildpack", label))
			}
			if len(svc.Image.BuildArgs) > 0 {
				errs = append(errs, fmt.Sprintf("%s image.build_args cannot be combined with image.buildpack; use image.buildpack.env", label))
			}
			if strings.TrimSpace(svc.Image.Target) != "" {
				errs = append(errs, fmt.Sprintf("%s image.target cannot be combined with image.buildpack", label))
			}
			if strings.TrimSpace(svc.Image.Builder) != "" {
				errs = append(errs, fmt.Sprintf("%s image.builder cannot be combined with image.buildpack; use image.buildpack.builder", label))
			}
			if strings.TrimSpace(svc.Image.Platform) != "" {
				errs = append(errs, fmt.Sprintf("%s image.platform cannot be combined with image.buildpack", label))
			}
			if len(svc.Image.Platforms) > 0 {
				errs = append(errs, fmt.Sprintf("%s image.platforms cannot be combined with image.buildpack", label))
			}
			if svc.Image.Pull {
				errs = append(errs, fmt.Sprintf("%s image.pull cannot be combined with image.buildpack; use image.buildpack.pull_policy", label))
			}
			if svc.Image.NoCache {
				errs = append(errs, fmt.Sprintf("%s image.no_cache cannot be combined with image.buildpack", label))
			}
			if len(svc.Image.NoCacheFilter) > 0 {
				errs = append(errs, fmt.Sprintf("%s image.no_cache_filter cannot be combined with image.buildpack", label))
			}
			if len(svc.Image.CacheFrom) > 0 {
				errs = append(errs, fmt.Sprintf("%s image.cache_from cannot be combined with image.buildpack", label))
			}
			if len(svc.Image.CacheTo) > 0 {
				errs = append(errs, fmt.Sprintf("%s image.cache_to cannot be combined with image.buildpack", label))
			}
			if len(svc.Image.Secrets) > 0 {
				errs = append(errs, fmt.Sprintf("%s image.secrets cannot be combined with image.buildpack", label))
			}
			if len(svc.Image.SSH) > 0 {
				errs = append(errs, fmt.Sprintf("%s image.ssh cannot be combined with image.buildpack", label))
			}
			if svc.Image.SBOM.Enabled() {
				errs = append(errs, fmt.Sprintf("%s image.sbom cannot be combined with image.buildpack", label))
			}
			if svc.Image.Provenance.Enabled() {
				errs = append(errs, fmt.Sprintf("%s image.provenance cannot be combined with image.buildpack", label))
			}
		}
		if svc.Image.Build == "" && svc.Image.Ref == "" {
			errs = append(errs, fmt.Sprintf("%s image.build or image.ref is required", label))
		}
		if svc.Image.Build == "" {
			if svc.Image.Dockerfile != "" {
				errs = append(errs, fmt.Sprintf("%s image.dockerfile requires image.build", label))
			}
			if len(svc.Image.Tags) > 0 {
				errs = append(errs, fmt.Sprintf("%s image.tags requires image.build", label))
			}
			if len(svc.Image.BuildArgs) > 0 {
				errs = append(errs, fmt.Sprintf("%s image.build_args requires image.build", label))
			}
			if svc.Image.Target != "" {
				errs = append(errs, fmt.Sprintf("%s image.target requires image.build", label))
			}
			if svc.Image.Builder != "" {
				errs = append(errs, fmt.Sprintf("%s image.builder requires image.build", label))
			}
			if svc.Image.Platform != "" {
				errs = append(errs, fmt.Sprintf("%s image.platform requires image.build", label))
			}
			if len(svc.Image.Platforms) > 0 {
				errs = append(errs, fmt.Sprintf("%s image.platforms requires image.build", label))
			}
			if svc.Image.Pull {
				errs = append(errs, fmt.Sprintf("%s image.pull requires image.build", label))
			}
			if svc.Image.NoCache {
				errs = append(errs, fmt.Sprintf("%s image.no_cache requires image.build", label))
			}
			if len(svc.Image.NoCacheFilter) > 0 {
				errs = append(errs, fmt.Sprintf("%s image.no_cache_filter requires image.build", label))
			}
			if len(svc.Image.CacheFrom) > 0 {
				errs = append(errs, fmt.Sprintf("%s image.cache_from requires image.build", label))
			}
			if len(svc.Image.CacheTo) > 0 {
				errs = append(errs, fmt.Sprintf("%s image.cache_to requires image.build", label))
			}
			if len(svc.Image.Secrets) > 0 {
				errs = append(errs, fmt.Sprintf("%s image.secrets requires image.build", label))
			}
			if len(svc.Image.SSH) > 0 {
				errs = append(errs, fmt.Sprintf("%s image.ssh requires image.build", label))
			}
			if svc.Image.SBOM.Enabled() {
				errs = append(errs, fmt.Sprintf("%s image.sbom requires image.build", label))
			}
			if svc.Image.Provenance.Enabled() {
				errs = append(errs, fmt.Sprintf("%s image.provenance requires image.build", label))
			}
		}
		if strings.TrimSpace(svc.Image.Platform) != "" && len(svc.Image.Platforms) > 0 {
			errs = append(errs, fmt.Sprintf("%s image.platform cannot be combined with image.platforms", label))
		}
		errs = append(errs, validateDockerBuildSpecs(label+" image.tags", svc.Image.Tags)...)
		errs = append(errs, validateDockerBuildString(label+" image.builder", svc.Image.Builder)...)
		errs = append(errs, validateDockerBuildSpecs(label+" image.platforms", svc.Image.Platforms)...)
		errs = append(errs, validateDockerBuildSpecs(label+" image.no_cache_filter", svc.Image.NoCacheFilter)...)
		errs = append(errs, validateDockerCacheSpecs(label+" image.cache_from", svc.Image.CacheFrom)...)
		errs = append(errs, validateDockerCacheSpecs(label+" image.cache_to", svc.Image.CacheTo)...)
		errs = append(errs, validateDockerBuildSpecs(label+" image.secrets", svc.Image.Secrets)...)
		errs = append(errs, validateDockerBuildSpecs(label+" image.ssh", svc.Image.SSH)...)
		errs = append(errs, validateDockerBuildFlag(label+" image.sbom", svc.Image.SBOM)...)
		errs = append(errs, validateDockerBuildFlag(label+" image.provenance", svc.Image.Provenance)...)
		errs = append(errs, validateReleaseCommand(label, svc.Scale, svc.Release)...)
		errs = append(errs, validateSchedules(label, svc.Scale, svc.Schedules)...)
		errs = append(errs, validateRestartPolicy(label+" restart_policy", svc.RestartPolicy)...)
		errs = append(errs, validateLoggingConfig(label+" logging", svc.Logging)...)
		errs = append(errs, validateContainerLabels(label+" labels", svc.Labels)...)
		errs = append(errs, validateNetworkAliases(label+" network_aliases", svc.NetworkAliases)...)
		errs = append(errs, validateDockerPublishSpecs(label+" publish", svc.Publish)...)
		errs = append(errs, validateVolumeSpecs(label+" volumes", svc.Volumes)...)
		errs = append(errs, validateResourceConfig(label+" resources", svc.Resources)...)
		errs = append(errs, validateRuntimeConfig(label+" runtime", svc.Runtime)...)
		if svc.Ingress != nil {
			errs = append(errs, validateIngressDomains(label+" ingress.domains", svc.Ingress.Domains)...)
			errs = append(errs, validateIngressRedirects(label+" ingress.redirects", svc.Ingress.Redirects)...)
			errs = append(errs, validateIngressDomainConflicts(label+" ingress", *svc.Ingress)...)
			errs = append(errs, validateIngressHealth(label+" ingress.health", svc.Ingress.Health)...)
			if svc.Ingress.Health.Enabled != nil && *svc.Ingress.Health.Enabled && strings.TrimSpace(svc.Ingress.Health.Path) == "" && strings.TrimSpace(svc.Health.HTTP) == "" {
				errs = append(errs, fmt.Sprintf("%s ingress.health.path or health.http is required when ingress.health.enabled is true", label))
			}
		}
	}
	return errs
}

func validateIngressDomains(label string, domains []string) []string {
	var errs []string
	for i, domain := range domains {
		domainLabel := fmt.Sprintf("%s[%d]", label, i)
		if strings.TrimSpace(domain) == "" {
			errs = append(errs, fmt.Sprintf("%s is required", domainLabel))
			continue
		}
		if strings.ContainsAny(domain, " \t\r\n") {
			errs = append(errs, fmt.Sprintf("%s cannot contain whitespace", domainLabel))
		}
	}
	return errs
}

func validateIngressRedirects(label string, redirects []IngressRedirect) []string {
	var errs []string
	for i, redirect := range redirects {
		redirectLabel := fmt.Sprintf("%s[%d]", label, i)
		if len(redirect.From) == 0 {
			errs = append(errs, fmt.Sprintf("%s.from is required", redirectLabel))
		}
		for j, domain := range redirect.From {
			domainLabel := fmt.Sprintf("%s.from[%d]", redirectLabel, j)
			if strings.TrimSpace(domain) == "" {
				errs = append(errs, fmt.Sprintf("%s is required", domainLabel))
				continue
			}
			if strings.ContainsAny(domain, " \t\r\n") {
				errs = append(errs, fmt.Sprintf("%s cannot contain whitespace", domainLabel))
			}
		}
		target := strings.TrimSpace(redirect.To)
		if target == "" {
			errs = append(errs, fmt.Sprintf("%s.to is required", redirectLabel))
		} else {
			if strings.ContainsAny(target, " \t\r\n") {
				errs = append(errs, fmt.Sprintf("%s.to cannot contain whitespace", redirectLabel))
			}
			if !strings.HasPrefix(target, "https://") && !strings.HasPrefix(target, "http://") {
				errs = append(errs, fmt.Sprintf("%s.to must start with http:// or https://", redirectLabel))
			}
		}
		if redirect.Code != 0 && redirect.Code != 301 && redirect.Code != 302 && redirect.Code != 303 && redirect.Code != 307 && redirect.Code != 308 {
			errs = append(errs, fmt.Sprintf("%s.code must be one of 301, 302, 303, 307, or 308", redirectLabel))
		}
	}
	return errs
}

func validateIngressDomainConflicts(label string, ingress Ingress) []string {
	proxied := map[string]bool{}
	for _, domain := range ingress.Domains {
		domain = strings.TrimSpace(domain)
		if domain != "" {
			proxied[domain] = true
		}
	}
	if len(proxied) == 0 {
		return nil
	}
	var errs []string
	for i, redirect := range ingress.Redirects {
		for j, domain := range redirect.From {
			domain = strings.TrimSpace(domain)
			if proxied[domain] {
				errs = append(errs, fmt.Sprintf("%s.redirects[%d].from[%d] conflicts with proxied domain %q", label, i, j, domain))
			}
		}
	}
	return errs
}

func validateIngressHealth(label string, health IngressHealth) []string {
	var errs []string
	for field, value := range map[string]int{
		"interval_seconds":              health.IntervalSeconds,
		"timeout_seconds":               health.TimeoutSeconds,
		"passes":                        health.Passes,
		"fails":                         health.Fails,
		"try_duration_seconds":          health.TryDurationSeconds,
		"passive_fail_duration_seconds": health.PassiveFailDurationSeconds,
		"passive_max_fails":             health.PassiveMaxFails,
	} {
		if value < 0 {
			errs = append(errs, fmt.Sprintf("%s.%s cannot be negative", label, field))
		}
	}
	if health.Path != "" && !strings.HasPrefix(health.Path, "/") {
		errs = append(errs, fmt.Sprintf("%s.path must start with /", label))
	}
	if strings.ContainsAny(health.Path, " \t\r\n") {
		errs = append(errs, fmt.Sprintf("%s.path cannot contain whitespace", label))
	}
	for i, status := range health.UnhealthyStatus {
		status = strings.TrimSpace(status)
		if status == "" {
			errs = append(errs, fmt.Sprintf("%s.unhealthy_status[%d] is required", label, i))
		}
		if strings.ContainsAny(status, " \t\r\n") {
			errs = append(errs, fmt.Sprintf("%s.unhealthy_status[%d] cannot contain whitespace", label, i))
		}
	}
	return errs
}

func validateResourceConfig(label string, resources ResourceConfig) []string {
	var errs []string
	for field, value := range map[string]string{
		"cpus":               resources.CPUs,
		"memory":             resources.Memory,
		"memory_reservation": resources.MemoryReservation,
		"memory_swap":        resources.MemorySwap,
		"cpuset_cpus":        resources.CPUSetCPUs,
	} {
		if strings.ContainsAny(value, "\r\n") {
			errs = append(errs, fmt.Sprintf("%s.%s cannot contain newlines", label, field))
		}
	}
	if resources.CPUShares < 0 {
		errs = append(errs, fmt.Sprintf("%s.cpu_shares cannot be negative", label))
	}
	if resources.PIDsLimit < 0 {
		errs = append(errs, fmt.Sprintf("%s.pids_limit cannot be negative", label))
	}
	return errs
}

func validateBuildpackConfig(label string, buildpack BuildpackConfig) []string {
	var errs []string
	errs = append(errs, validateDockerRuntimeString(label+" builder", buildpack.Builder)...)
	errs = append(errs, validateDockerRuntimeString(label+" descriptor", buildpack.Descriptor)...)
	pullPolicy := strings.TrimSpace(buildpack.PullPolicy)
	if pullPolicy != "" && pullPolicy != "always" && pullPolicy != "never" && pullPolicy != "if-not-present" {
		errs = append(errs, fmt.Sprintf("%s pull_policy must be one of always, never, or if-not-present", label))
	}
	for i, value := range buildpack.Buildpacks {
		item := fmt.Sprintf("%s buildpacks[%d]", label, i)
		if strings.TrimSpace(value) == "" {
			errs = append(errs, fmt.Sprintf("%s is required", item))
			continue
		}
		if strings.ContainsAny(value, "\r\n") {
			errs = append(errs, fmt.Sprintf("%s cannot contain newlines", item))
		}
	}
	for key, value := range buildpack.Env {
		item := fmt.Sprintf("%s env[%q]", label, key)
		if strings.TrimSpace(key) == "" {
			errs = append(errs, fmt.Sprintf("%s key is required", item))
			continue
		}
		if strings.ContainsAny(key, " \t\r\n=") {
			errs = append(errs, fmt.Sprintf("%s key cannot contain whitespace, newlines, or '='", item))
		}
		if strings.ContainsAny(value, "\r\n") {
			errs = append(errs, fmt.Sprintf("%s value cannot contain newlines", item))
		}
	}
	sort.Strings(errs)
	return errs
}

func validateRuntimeConfig(label string, runtime RuntimeConfig) []string {
	var errs []string
	errs = append(errs, validateDockerRuntimeString(label+" user", runtime.User)...)
	errs = append(errs, validateDockerRuntimeString(label+" workdir", runtime.Workdir)...)
	errs = append(errs, validateDockerRuntimeString(label+" hostname", runtime.Hostname)...)
	errs = append(errs, validateDockerRuntimeString(label+" entrypoint", runtime.Entrypoint)...)
	errs = append(errs, validateDockerRuntimeString(label+" ipc", runtime.IPC)...)
	errs = append(errs, validateDockerRuntimeString(label+" pid", runtime.PID)...)
	errs = append(errs, validateDockerRuntimeString(label+" cgroupns", runtime.CgroupNS)...)
	errs = append(errs, validateDockerRuntimeString(label+" stop_signal", runtime.StopSignal)...)
	errs = append(errs, validateDockerRuntimeString(label+" shm_size", runtime.ShmSize)...)
	errs = append(errs, validateDockerRuntimeString(label+" gpus", runtime.GPUs)...)
	errs = append(errs, validateDockerRuntimeString(label+" health_cmd", runtime.HealthCMD)...)
	errs = append(errs, validateDockerRuntimeString(label+" health_interval", runtime.HealthInterval)...)
	errs = append(errs, validateDockerRuntimeString(label+" health_timeout", runtime.HealthTimeout)...)
	errs = append(errs, validateDockerRuntimeString(label+" health_start_period", runtime.HealthStartPeriod)...)
	if runtime.StopTimeoutSeconds < 0 {
		errs = append(errs, fmt.Sprintf("%s stop_timeout_seconds cannot be negative", label))
	}
	if runtime.HealthRetries < 0 {
		errs = append(errs, fmt.Sprintf("%s health_retries cannot be negative", label))
	}
	if runtime.NoHealthcheck && runtimeHasExplicitHealthcheck(runtime) {
		errs = append(errs, fmt.Sprintf("%s no_healthcheck cannot be combined with explicit healthcheck settings", label))
	}
	errs = append(errs, validateDockerRuntimeSpecs(label+" cap_add", runtime.CapAdd)...)
	errs = append(errs, validateDockerRuntimeSpecs(label+" cap_drop", runtime.CapDrop)...)
	errs = append(errs, validateDockerRuntimeSpecs(label+" group_add", runtime.GroupAdd)...)
	errs = append(errs, validateDockerRuntimeSpecs(label+" security_opt", runtime.SecurityOpt)...)
	errs = append(errs, validateDockerRuntimeSpecs(label+" ulimits", runtime.Ulimits)...)
	errs = append(errs, validateDockerRuntimeSpecs(label+" mounts", runtime.Mounts)...)
	errs = append(errs, validateDockerRuntimeSpecs(label+" add_hosts", runtime.AddHosts)...)
	errs = append(errs, validateDockerRuntimeSpecs(label+" dns", runtime.DNS)...)
	errs = append(errs, validateDockerRuntimeSpecs(label+" dns_search", runtime.DNSSearch)...)
	errs = append(errs, validateDockerRuntimeSpecs(label+" dns_options", runtime.DNSOptions)...)
	errs = append(errs, validateDockerRuntimeSpecs(label+" devices", runtime.Devices)...)
	errs = append(errs, validateDockerRuntimeSpecs(label+" device_cgroup_rules", runtime.DeviceCgroupRules)...)
	errs = append(errs, validateDockerRuntimeSpecs(label+" tmpfs", runtime.Tmpfs)...)
	for key, value := range runtime.Sysctls {
		item := fmt.Sprintf("%s sysctls[%q]", label, key)
		if strings.TrimSpace(key) == "" {
			errs = append(errs, fmt.Sprintf("%s key is required", item))
			continue
		}
		if strings.ContainsAny(key, " \t\r\n=") {
			errs = append(errs, fmt.Sprintf("%s key cannot contain whitespace, newlines, or '='", item))
		}
		if strings.TrimSpace(value) == "" {
			errs = append(errs, fmt.Sprintf("%s value is required", item))
		}
		if strings.ContainsAny(value, "\r\n") {
			errs = append(errs, fmt.Sprintf("%s value cannot contain newlines", item))
		}
	}
	sort.Strings(errs)
	return errs
}

func runtimeHasExplicitHealthcheck(runtime RuntimeConfig) bool {
	return strings.TrimSpace(runtime.HealthCMD) != "" ||
		strings.TrimSpace(runtime.HealthInterval) != "" ||
		strings.TrimSpace(runtime.HealthTimeout) != "" ||
		strings.TrimSpace(runtime.HealthStartPeriod) != "" ||
		runtime.HealthRetries > 0
}

func validateDockerRuntimeString(label, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if strings.ContainsAny(value, "\r\n") {
		return []string{fmt.Sprintf("%s cannot contain newlines", label)}
	}
	return nil
}

func validateDockerRuntimeSpecs(label string, specs []string) []string {
	var errs []string
	for i, spec := range specs {
		item := fmt.Sprintf("%s[%d]", label, i)
		if strings.TrimSpace(spec) == "" {
			errs = append(errs, fmt.Sprintf("%s is required", item))
			continue
		}
		if strings.ContainsAny(spec, "\r\n") {
			errs = append(errs, fmt.Sprintf("%s cannot contain newlines", item))
		}
	}
	return errs
}

func validateRestartPolicy(label, policy string) []string {
	policy = strings.TrimSpace(policy)
	if policy == "" {
		return nil
	}
	if strings.ContainsAny(policy, " \t\r\n") {
		return []string{fmt.Sprintf("%s cannot contain whitespace", label)}
	}
	switch policy {
	case "no", "always", DefaultRestartPolicy, "on-failure":
		return nil
	}
	const prefix = "on-failure:"
	if strings.HasPrefix(policy, prefix) {
		retries := strings.TrimPrefix(policy, prefix)
		if retries == "" {
			return []string{fmt.Sprintf("%s on-failure retry count is required", label)}
		}
		for _, r := range retries {
			if r < '0' || r > '9' {
				return []string{fmt.Sprintf("%s on-failure retry count must be a positive integer", label)}
			}
		}
		if strings.TrimLeft(retries, "0") == "" {
			return []string{fmt.Sprintf("%s on-failure retry count must be a positive integer", label)}
		}
		return nil
	}
	return []string{fmt.Sprintf("%s must be one of no, always, unless-stopped, on-failure, or on-failure:N", label)}
}

func validateVolumeSpecs(label string, specs []string) []string {
	var errs []string
	for i, spec := range specs {
		specLabel := fmt.Sprintf("%s[%d]", label, i)
		if strings.TrimSpace(spec) == "" {
			errs = append(errs, fmt.Sprintf("%s is required", specLabel))
			continue
		}
		if strings.ContainsAny(spec, "\r\n") {
			errs = append(errs, fmt.Sprintf("%s cannot contain newlines", specLabel))
		}
		if !strings.Contains(spec, ":") {
			errs = append(errs, fmt.Sprintf("%s must use source:target syntax", specLabel))
		}
	}
	return errs
}

func validateDockerPublishSpecs(label string, specs []string) []string {
	var errs []string
	for i, spec := range specs {
		specLabel := fmt.Sprintf("%s[%d]", label, i)
		if strings.TrimSpace(spec) == "" {
			errs = append(errs, fmt.Sprintf("%s is required", specLabel))
			continue
		}
		if strings.ContainsAny(spec, "\r\n") {
			errs = append(errs, fmt.Sprintf("%s cannot contain newlines", specLabel))
		}
	}
	return errs
}

func validateLoggingConfig(label string, logging LoggingConfig) []string {
	var errs []string
	if strings.ContainsAny(logging.Driver, " \t\r\n") {
		errs = append(errs, fmt.Sprintf("%s.driver cannot contain whitespace", label))
	}
	for key, value := range logging.Options {
		optionLabel := fmt.Sprintf("%s.options[%q]", label, key)
		if strings.TrimSpace(key) == "" {
			errs = append(errs, fmt.Sprintf("%s key is required", optionLabel))
		}
		if strings.ContainsAny(key, " \t\r\n=") {
			errs = append(errs, fmt.Sprintf("%s key cannot contain whitespace or '='", optionLabel))
		}
		if strings.ContainsAny(value, "\r\n") {
			errs = append(errs, fmt.Sprintf("%s value cannot contain newlines", optionLabel))
		}
	}
	return errs
}

func validateCaddyConfig(label string, caddy CaddyConfig) []string {
	var errs []string
	for field, value := range map[string]string{
		"data_volume":   caddy.DataVolume,
		"config_volume": caddy.ConfigVolume,
	} {
		if strings.ContainsAny(value, " \t\r\n:") {
			errs = append(errs, fmt.Sprintf("%s.%s cannot contain whitespace, newlines, or ':'", label, field))
		}
	}
	return errs
}

func validateReleaseCommand(label string, scale int, release ReleaseCommand) []string {
	var errs []string
	hasRelease := strings.TrimSpace(release.Command) != "" || release.Replica != 0 || release.TimeoutSeconds != 0
	if !hasRelease {
		return nil
	}
	if strings.TrimSpace(release.Command) == "" {
		errs = append(errs, fmt.Sprintf("%s release.command is required", label))
	}
	if strings.ContainsAny(release.Command, "\r\n") {
		errs = append(errs, fmt.Sprintf("%s release.command cannot contain newlines", label))
	}
	if release.Replica < 0 {
		errs = append(errs, fmt.Sprintf("%s release.replica cannot be negative", label))
	}
	if scale <= 0 {
		errs = append(errs, fmt.Sprintf("%s release requires service scale to be at least 1", label))
	}
	if release.Replica > scale {
		errs = append(errs, fmt.Sprintf("%s release.replica %d exceeds service scale %d", label, release.Replica, scale))
	}
	if release.TimeoutSeconds < 0 {
		errs = append(errs, fmt.Sprintf("%s release.timeout_seconds cannot be negative", label))
	}
	return errs
}

func validateHooks(label string, hooks Hooks) []string {
	var errs []string
	errs = append(errs, validateHookCommands(label+".pre_deploy", hooks.PreDeploy)...)
	errs = append(errs, validateHookCommands(label+".pre_build", hooks.PreBuild)...)
	errs = append(errs, validateHookCommands(label+".post_deploy", hooks.PostDeploy)...)
	errs = append(errs, validateHookCommands(label+".deploy_failed", hooks.DeployFailed)...)
	return errs
}

func validateNotifications(label string, notifications Notifications) []string {
	var errs []string
	for i, webhook := range notifications.Webhooks {
		webhookLabel := fmt.Sprintf("%s.webhooks[%d]", label, i)
		hasURL := strings.TrimSpace(webhook.URL) != ""
		hasURLEnv := strings.TrimSpace(webhook.URLEnv) != ""
		if hasURL == hasURLEnv {
			errs = append(errs, fmt.Sprintf("%s requires exactly one of url or url_env", webhookLabel))
		}
		if strings.ContainsAny(webhook.URL, "\r\n") {
			errs = append(errs, fmt.Sprintf("%s.url cannot contain newlines", webhookLabel))
		} else {
			errs = append(errs, validateWebhookURL(webhookLabel+".url", webhook.URL)...)
		}
		if webhook.URLEnv != "" && !validEnvName(webhook.URLEnv) {
			errs = append(errs, fmt.Sprintf("%s.url_env %q must be a valid environment variable name", webhookLabel, webhook.URLEnv))
		}
		if webhook.TimeoutSeconds < 0 {
			errs = append(errs, fmt.Sprintf("%s.timeout_seconds cannot be negative", webhookLabel))
		}
		for name := range webhook.Headers {
			if strings.TrimSpace(name) == "" {
				errs = append(errs, fmt.Sprintf("%s.headers key is required", webhookLabel))
			}
			if strings.ContainsAny(name, "\r\n") || strings.ContainsAny(webhook.Headers[name], "\r\n") {
				errs = append(errs, fmt.Sprintf("%s.headers %q cannot contain newlines", webhookLabel, name))
			}
		}
		for j, event := range webhook.Events {
			if strings.TrimSpace(event) == "" {
				errs = append(errs, fmt.Sprintf("%s.events[%d] is required", webhookLabel, j))
			}
		}
	}
	return errs
}

func validateWebhookURL(label, raw string) []string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return []string{fmt.Sprintf("%s must be an absolute http or https URL", label)}
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return []string{fmt.Sprintf("%s scheme must be http or https", label)}
	}
	return nil
}

func validateHookCommands(label string, hooks []HookCommand) []string {
	var errs []string
	for i, hook := range hooks {
		hookLabel := fmt.Sprintf("%s[%d]", label, i)
		if strings.TrimSpace(hook.Command) == "" {
			errs = append(errs, fmt.Sprintf("%s.command is required", hookLabel))
		}
		if strings.ContainsAny(hook.Command, "\r\n") {
			errs = append(errs, fmt.Sprintf("%s.command cannot contain newlines", hookLabel))
		}
		if hook.TimeoutSeconds < 0 {
			errs = append(errs, fmt.Sprintf("%s.timeout_seconds cannot be negative", hookLabel))
		}
		for name := range hook.Env {
			if !validEnvName(name) {
				errs = append(errs, fmt.Sprintf("%s.env %q must be a valid environment variable name", hookLabel, name))
			}
		}
	}
	return errs
}

func validateSchedules(label string, scale int, schedules map[string]Schedule) []string {
	var errs []string
	for name, schedule := range schedules {
		scheduleLabel := fmt.Sprintf("%s schedule %q", label, name)
		if !validScheduleName(name) {
			errs = append(errs, fmt.Sprintf("%s name must contain only letters, numbers, dots, underscores, or dashes", scheduleLabel))
		}
		if strings.TrimSpace(schedule.Cron) == "" {
			errs = append(errs, fmt.Sprintf("%s cron is required", scheduleLabel))
		} else if !validCronSpec(schedule.Cron) {
			errs = append(errs, fmt.Sprintf("%s cron must have exactly five fields", scheduleLabel))
		}
		if strings.TrimSpace(schedule.Command) == "" {
			errs = append(errs, fmt.Sprintf("%s command is required", scheduleLabel))
		}
		if strings.ContainsAny(schedule.Command, "\r\n") {
			errs = append(errs, fmt.Sprintf("%s command cannot contain newlines", scheduleLabel))
		}
		if schedule.Replica < 0 {
			errs = append(errs, fmt.Sprintf("%s replica cannot be negative", scheduleLabel))
		}
		if scale <= 0 {
			errs = append(errs, fmt.Sprintf("%s requires service scale to be at least 1", scheduleLabel))
		}
		if schedule.Replica > scale {
			errs = append(errs, fmt.Sprintf("%s replica %d exceeds service scale %d", scheduleLabel, schedule.Replica, scale))
		}
		if schedule.TimeoutSeconds < 0 {
			errs = append(errs, fmt.Sprintf("%s timeout_seconds cannot be negative", scheduleLabel))
		}
	}
	return errs
}

func validScheduleName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		allowed := r >= 'a' && r <= 'z' ||
			r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' ||
			r == '_' || r == '.' || r == '-'
		if !allowed {
			return false
		}
	}
	return true
}

func validCronSpec(spec string) bool {
	fields := strings.Fields(spec)
	return len(fields) == 5
}

func validateAccessories(prefix string, accessories map[string]Accessory) []string {
	var errs []string
	for name, acc := range accessories {
		label := prefix + fmt.Sprintf("accessory %q", name)
		errs = append(errs, validateSecretNames(label, acc.Secrets)...)
		if acc.Image == "" {
			errs = append(errs, fmt.Sprintf("%s image is required", label))
		}
		if acc.Pool == "" {
			errs = append(errs, fmt.Sprintf("%s pool is required", label))
		}
		if acc.Backup.Required && acc.Backup.Command == "" {
			errs = append(errs, fmt.Sprintf("%s requires backup.command", label))
		}
		if strings.TrimSpace(acc.Backup.ExportCommand) != "" {
			if strings.TrimSpace(acc.Backup.Command) == "" {
				errs = append(errs, fmt.Sprintf("%s backup.export_command requires backup.command", label))
			}
			if strings.ContainsAny(acc.Backup.ExportCommand, "\r\n") {
				errs = append(errs, fmt.Sprintf("%s backup.export_command cannot contain newlines", label))
			}
		}
		if acc.Backup.ExportTimeoutSeconds < 0 {
			errs = append(errs, fmt.Sprintf("%s backup.export_timeout_seconds cannot be negative", label))
		}
		if strings.TrimSpace(acc.Backup.Schedule.Cron) != "" {
			if !validCronSpec(acc.Backup.Schedule.Cron) {
				errs = append(errs, fmt.Sprintf("%s backup.schedule.cron must have exactly five fields", label))
			}
			if strings.TrimSpace(acc.Backup.Command) == "" {
				errs = append(errs, fmt.Sprintf("%s backup.schedule requires backup.command", label))
			}
			if strings.ContainsAny(acc.Backup.Command, "\r\n") {
				errs = append(errs, fmt.Sprintf("%s backup.command cannot contain newlines when backup.schedule is set", label))
			}
		}
		if acc.Backup.Schedule.TimeoutSeconds < 0 {
			errs = append(errs, fmt.Sprintf("%s backup.schedule.timeout_seconds cannot be negative", label))
		}
		if acc.Backup.RestoreCheck && !acc.Backup.Required {
			errs = append(errs, fmt.Sprintf("%s backup.restore_check requires backup.required", label))
		}
		errs = append(errs, validateRestartPolicy(label+" restart_policy", acc.RestartPolicy)...)
		errs = append(errs, validateContainerLabels(label+" labels", acc.Labels)...)
		errs = append(errs, validateNetworkAliases(label+" network_aliases", acc.NetworkAliases)...)
		errs = append(errs, validateDockerPublishSpecs(label+" publish", acc.Publish)...)
		errs = append(errs, validateResourceConfig(label+" resources", acc.Resources)...)
		errs = append(errs, validateRuntimeConfig(label+" runtime", acc.Runtime)...)
	}
	return errs
}

func (c *Config) resolvedForValidation(envName string) (*Config, Environment, error) {
	env, err := c.Environment(envName)
	if err != nil {
		return nil, Environment{}, err
	}
	resolved := *c
	resolved.Environments = map[string]Environment{envName: env}
	resolved.Services = copyServices(c.Services)
	for serviceName, svc := range env.Services {
		if resolved.Services == nil {
			resolved.Services = map[string]Service{}
		}
		if base, ok := resolved.Services[serviceName]; ok {
			resolved.Services[serviceName] = mergeService(base, svc)
		} else {
			resolved.Services[serviceName] = svc
		}
	}
	resolved.Services = applyLoggingDefaults(resolved.Services, c.Logging)
	resolved.Accessories = copyAccessories(c.Accessories)
	for accessoryName, acc := range env.Accessories {
		if resolved.Accessories == nil {
			resolved.Accessories = map[string]Accessory{}
		}
		if base, ok := resolved.Accessories[accessoryName]; ok {
			resolved.Accessories[accessoryName] = mergeAccessory(base, acc)
		} else {
			resolved.Accessories[accessoryName] = acc
		}
	}
	runtimeDefaults := mergeRuntimeConfig(c.Runtime, env.Runtime)
	resolved.Services = applyRuntimeDefaultsToServices(resolved.Services, runtimeDefaults)
	resolved.Accessories = applyRuntimeDefaultsToAccessories(resolved.Accessories, runtimeDefaults)
	resolved.Secrets = mergeNames(c.Secrets, env.Secrets)
	resolved.Docker = mergeDockerConfig(c.Docker, env.Docker)
	resolved.Ingress = mergeIngressConfig(c.Ingress, env.Ingress)
	resolved.Hooks = mergeHooks(c.Hooks, env.Hooks)
	resolved.Notifications = mergeNotifications(c.Notifications, env.Notifications)
	env.SSH = mergeSSHConfig(c.SSH, env.SSH)
	env.Docker = resolved.Docker
	env.Hooks = resolved.Hooks
	env.Notifications = resolved.Notifications
	resolved.Environments[envName] = env
	return &resolved, env, nil
}

func copyServices(in map[string]Service) map[string]Service {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]Service, len(in))
	for name, svc := range in {
		out[name] = svc
	}
	return out
}

func applyLoggingDefaults(services map[string]Service, defaults LoggingConfig) map[string]Service {
	if len(services) == 0 {
		return services
	}
	for name, svc := range services {
		svc.Logging = mergeLoggingConfig(defaults, svc.Logging)
		services[name] = svc
	}
	return services
}

func mergeLoggingConfig(base, override LoggingConfig) LoggingConfig {
	merged := LoggingConfig{Driver: strings.TrimSpace(base.Driver)}
	if len(base.Options) > 0 {
		merged.Options = map[string]string{}
		for key, value := range base.Options {
			merged.Options[key] = value
		}
	}
	if strings.TrimSpace(override.Driver) != "" {
		merged.Driver = strings.TrimSpace(override.Driver)
	}
	if len(override.Options) > 0 {
		if merged.Options == nil {
			merged.Options = map[string]string{}
		}
		for key, value := range override.Options {
			merged.Options[key] = value
		}
	}
	return merged
}

func applyRuntimeDefaultsToServices(services map[string]Service, defaults RuntimeConfig) map[string]Service {
	if len(services) == 0 {
		return services
	}
	for name, svc := range services {
		svc.Runtime = mergeRuntimeConfig(defaults, svc.Runtime)
		services[name] = svc
	}
	return services
}

func applyRuntimeDefaultsToAccessories(accessories map[string]Accessory, defaults RuntimeConfig) map[string]Accessory {
	if len(accessories) == 0 {
		return accessories
	}
	for name, acc := range accessories {
		acc.Runtime = mergeRuntimeConfig(defaults, acc.Runtime)
		accessories[name] = acc
	}
	return accessories
}

func mergeRuntimeConfig(base, override RuntimeConfig) RuntimeConfig {
	merged := base
	merged.CapAdd = appendStringSlices(base.CapAdd, override.CapAdd)
	merged.CapDrop = appendStringSlices(base.CapDrop, override.CapDrop)
	merged.GroupAdd = appendStringSlices(base.GroupAdd, override.GroupAdd)
	merged.SecurityOpt = appendStringSlices(base.SecurityOpt, override.SecurityOpt)
	merged.Ulimits = appendStringSlices(base.Ulimits, override.Ulimits)
	merged.Mounts = appendStringSlices(base.Mounts, override.Mounts)
	merged.AddHosts = appendStringSlices(base.AddHosts, override.AddHosts)
	merged.DNS = appendStringSlices(base.DNS, override.DNS)
	merged.DNSSearch = appendStringSlices(base.DNSSearch, override.DNSSearch)
	merged.DNSOptions = appendStringSlices(base.DNSOptions, override.DNSOptions)
	merged.Devices = appendStringSlices(base.Devices, override.Devices)
	merged.DeviceCgroupRules = appendStringSlices(base.DeviceCgroupRules, override.DeviceCgroupRules)
	merged.Tmpfs = appendStringSlices(base.Tmpfs, override.Tmpfs)
	merged.Sysctls = mergeStringMap(base.Sysctls, override.Sysctls)

	merged.Privileged = base.Privileged || override.Privileged
	merged.ReadOnly = base.ReadOnly || override.ReadOnly
	merged.Init = base.Init || override.Init
	merged.NoHealthcheck = base.NoHealthcheck || override.NoHealthcheck

	merged.User = overrideString(base.User, override.User)
	merged.Workdir = overrideString(base.Workdir, override.Workdir)
	merged.Hostname = overrideString(base.Hostname, override.Hostname)
	merged.Entrypoint = overrideString(base.Entrypoint, override.Entrypoint)
	merged.IPC = overrideString(base.IPC, override.IPC)
	merged.PID = overrideString(base.PID, override.PID)
	merged.CgroupNS = overrideString(base.CgroupNS, override.CgroupNS)
	merged.StopSignal = overrideString(base.StopSignal, override.StopSignal)
	merged.ShmSize = overrideString(base.ShmSize, override.ShmSize)
	merged.GPUs = overrideString(base.GPUs, override.GPUs)
	merged.HealthCMD = overrideString(base.HealthCMD, override.HealthCMD)
	merged.HealthInterval = overrideString(base.HealthInterval, override.HealthInterval)
	merged.HealthTimeout = overrideString(base.HealthTimeout, override.HealthTimeout)
	merged.HealthStartPeriod = overrideString(base.HealthStartPeriod, override.HealthStartPeriod)

	if override.StopTimeoutSeconds != 0 {
		merged.StopTimeoutSeconds = override.StopTimeoutSeconds
	}
	if override.HealthRetries != 0 {
		merged.HealthRetries = override.HealthRetries
	}
	return merged
}

func appendStringSlices(base, override []string) []string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := make([]string, 0, len(base)+len(override))
	out = append(out, base...)
	out = append(out, override...)
	return out
}

func overrideString(base, override string) string {
	if strings.TrimSpace(override) != "" {
		return override
	}
	return base
}

func copyAccessories(in map[string]Accessory) map[string]Accessory {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]Accessory, len(in))
	for name, acc := range in {
		out[name] = acc
	}
	return out
}

func mergeService(base, override Service) Service {
	out := base
	out.Image = mergeImageSpec(base.Image, override.Image)
	if strings.TrimSpace(override.Command) != "" {
		out.Command = override.Command
	}
	if strings.TrimSpace(override.Pool) != "" {
		out.Pool = override.Pool
	}
	if override.Scale > 0 {
		out.Scale = override.Scale
	}
	if len(override.Labels) > 0 {
		out.Labels = mergeStringMap(base.Labels, override.Labels)
	}
	if len(override.NetworkAliases) > 0 {
		out.NetworkAliases = append([]string(nil), override.NetworkAliases...)
	}
	if len(override.Ports) > 0 {
		out.Ports = append([]int(nil), override.Ports...)
	}
	if len(override.Publish) > 0 {
		out.Publish = append([]string(nil), override.Publish...)
	}
	if override.Health.HTTP != "" || override.Health.Command != "" {
		out.Health = override.Health
	}
	if override.Ingress != nil {
		out.Ingress = mergeIngressPtr(base.Ingress, override.Ingress)
	}
	if len(override.Env) > 0 {
		out.Env = mergeEnvList(base.Env, override.Env)
	}
	if len(override.Secrets) > 0 {
		out.Secrets = append([]string(nil), override.Secrets...)
	}
	if strings.TrimSpace(override.RestartPolicy) != "" {
		out.RestartPolicy = override.RestartPolicy
	}
	if len(override.Volumes) > 0 {
		out.Volumes = append([]string(nil), override.Volumes...)
	}
	out.Logging = mergeLoggingConfig(base.Logging, override.Logging)
	out.Resources = mergeResourceConfig(base.Resources, override.Resources)
	out.Runtime = mergeRuntimeConfig(base.Runtime, override.Runtime)
	out.Rolling = mergeRollingConfig(base.Rolling, override.Rolling)
	if strings.TrimSpace(override.Release.Command) != "" || override.Release.Replica > 0 || override.Release.TimeoutSeconds > 0 {
		out.Release = override.Release
	}
	if len(override.Schedules) > 0 {
		out.Schedules = copySchedules(override.Schedules)
	}
	return out
}

func mergeAccessory(base, override Accessory) Accessory {
	out := base
	if strings.TrimSpace(override.Image) != "" {
		out.Image = override.Image
	}
	if strings.TrimSpace(override.Command) != "" {
		out.Command = override.Command
	}
	if strings.TrimSpace(override.Pool) != "" {
		out.Pool = override.Pool
	}
	if override.Primary {
		out.Primary = override.Primary
	}
	if len(override.Labels) > 0 {
		out.Labels = mergeStringMap(base.Labels, override.Labels)
	}
	if len(override.NetworkAliases) > 0 {
		out.NetworkAliases = append([]string(nil), override.NetworkAliases...)
	}
	if len(override.Volumes) > 0 {
		out.Volumes = append([]string(nil), override.Volumes...)
	}
	if strings.TrimSpace(override.VolumeOwner) != "" {
		out.VolumeOwner = override.VolumeOwner
	}
	if strings.TrimSpace(override.RestartPolicy) != "" {
		out.RestartPolicy = override.RestartPolicy
	}
	out.Resources = mergeResourceConfig(base.Resources, override.Resources)
	if len(override.Ports) > 0 {
		out.Ports = append([]int(nil), override.Ports...)
	}
	if len(override.Publish) > 0 {
		out.Publish = append([]string(nil), override.Publish...)
	}
	out.Runtime = mergeRuntimeConfig(base.Runtime, override.Runtime)
	if len(override.Env) > 0 {
		out.Env = mergeEnvList(base.Env, override.Env)
	}
	if len(override.Secrets) > 0 {
		out.Secrets = append([]string(nil), override.Secrets...)
	}
	out.Backup = mergeBackupSpec(base.Backup, override.Backup)
	return out
}

func mergeImageSpec(base, override ImageSpec) ImageSpec {
	out := base
	if strings.TrimSpace(override.Build) != "" {
		out.Build = override.Build
	}
	if strings.TrimSpace(override.Dockerfile) != "" {
		out.Dockerfile = override.Dockerfile
	}
	if strings.TrimSpace(override.Ref) != "" {
		out.Ref = override.Ref
	}
	if len(override.Tags) > 0 {
		out.Tags = append([]string(nil), override.Tags...)
	}
	if len(override.BuildArgs) > 0 {
		out.BuildArgs = mergeStringMap(base.BuildArgs, override.BuildArgs)
	}
	if strings.TrimSpace(override.Target) != "" {
		out.Target = override.Target
	}
	if strings.TrimSpace(override.Builder) != "" {
		out.Builder = override.Builder
	}
	out.Buildpack = mergeBuildpackConfig(base.Buildpack, override.Buildpack)
	if strings.TrimSpace(override.Platform) != "" {
		out.Platform = override.Platform
	}
	if len(override.Platforms) > 0 {
		out.Platforms = append([]string(nil), override.Platforms...)
	}
	if override.Pull {
		out.Pull = override.Pull
	}
	if override.NoCache {
		out.NoCache = override.NoCache
	}
	if len(override.NoCacheFilter) > 0 {
		out.NoCacheFilter = append([]string(nil), override.NoCacheFilter...)
	}
	if len(override.CacheFrom) > 0 {
		out.CacheFrom = append([]string(nil), override.CacheFrom...)
	}
	if len(override.CacheTo) > 0 {
		out.CacheTo = append([]string(nil), override.CacheTo...)
	}
	if len(override.Secrets) > 0 {
		out.Secrets = append([]string(nil), override.Secrets...)
	}
	if len(override.SSH) > 0 {
		out.SSH = append([]string(nil), override.SSH...)
	}
	if strings.TrimSpace(string(override.SBOM)) != "" {
		out.SBOM = override.SBOM
	}
	if strings.TrimSpace(string(override.Provenance)) != "" {
		out.Provenance = override.Provenance
	}
	return out
}

func mergeBuildpackConfig(base, override BuildpackConfig) BuildpackConfig {
	out := base
	if strings.TrimSpace(override.Builder) != "" {
		out.Builder = override.Builder
	}
	if len(override.Buildpacks) > 0 {
		out.Buildpacks = append([]string(nil), override.Buildpacks...)
	}
	if len(override.Env) > 0 {
		out.Env = mergeStringMap(base.Env, override.Env)
	}
	if strings.TrimSpace(override.Descriptor) != "" {
		out.Descriptor = override.Descriptor
	}
	if override.Publish {
		out.Publish = override.Publish
	}
	if strings.TrimSpace(override.PullPolicy) != "" {
		out.PullPolicy = override.PullPolicy
	}
	if override.TrustBuilder {
		out.TrustBuilder = override.TrustBuilder
	}
	return out
}

func mergeIngressPtr(base *Ingress, override *Ingress) *Ingress {
	if override == nil {
		return base
	}
	if base == nil {
		merged := *override
		return &merged
	}
	merged := *base
	if len(override.Domains) > 0 {
		merged.Domains = append([]string(nil), override.Domains...)
	}
	if len(override.Redirects) > 0 {
		merged.Redirects = append([]IngressRedirect(nil), override.Redirects...)
	}
	merged.Health = mergeIngressHealth(base.Health, override.Health)
	return &merged
}

func mergeIngressHealth(base, override IngressHealth) IngressHealth {
	out := base
	if override.Enabled != nil {
		out.Enabled = override.Enabled
	}
	if strings.TrimSpace(override.Path) != "" {
		out.Path = override.Path
	}
	if override.IntervalSeconds > 0 {
		out.IntervalSeconds = override.IntervalSeconds
	}
	if override.TimeoutSeconds > 0 {
		out.TimeoutSeconds = override.TimeoutSeconds
	}
	if override.Passes > 0 {
		out.Passes = override.Passes
	}
	if override.Fails > 0 {
		out.Fails = override.Fails
	}
	if override.TryDurationSeconds > 0 {
		out.TryDurationSeconds = override.TryDurationSeconds
	}
	if override.PassiveFailDurationSeconds > 0 {
		out.PassiveFailDurationSeconds = override.PassiveFailDurationSeconds
	}
	if override.PassiveMaxFails > 0 {
		out.PassiveMaxFails = override.PassiveMaxFails
	}
	if len(override.UnhealthyStatus) > 0 {
		out.UnhealthyStatus = append([]string(nil), override.UnhealthyStatus...)
	}
	return out
}

func mergeResourceConfig(base, override ResourceConfig) ResourceConfig {
	out := base
	if strings.TrimSpace(override.CPUs) != "" {
		out.CPUs = override.CPUs
	}
	if strings.TrimSpace(override.Memory) != "" {
		out.Memory = override.Memory
	}
	if strings.TrimSpace(override.MemoryReservation) != "" {
		out.MemoryReservation = override.MemoryReservation
	}
	if strings.TrimSpace(override.MemorySwap) != "" {
		out.MemorySwap = override.MemorySwap
	}
	if override.CPUShares > 0 {
		out.CPUShares = override.CPUShares
	}
	if strings.TrimSpace(override.CPUSetCPUs) != "" {
		out.CPUSetCPUs = override.CPUSetCPUs
	}
	if override.PIDsLimit > 0 {
		out.PIDsLimit = override.PIDsLimit
	}
	return out
}

func mergeRollingConfig(base, override Rolling) Rolling {
	out := base
	if override.MaxSurge > 0 {
		out.MaxSurge = override.MaxSurge
	}
	if override.MaxUnavailable > 0 {
		out.MaxUnavailable = override.MaxUnavailable
	}
	if override.HealthRetries > 0 {
		out.HealthRetries = override.HealthRetries
	}
	if override.DrainTimeoutSeconds > 0 {
		out.DrainTimeoutSeconds = override.DrainTimeoutSeconds
	}
	if override.CanaryReplicas > 0 {
		out.CanaryReplicas = override.CanaryReplicas
	}
	if override.CanaryPauseSeconds > 0 {
		out.CanaryPauseSeconds = override.CanaryPauseSeconds
	}
	if override.HealthTimeoutSeconds > 0 {
		out.HealthTimeoutSeconds = override.HealthTimeoutSeconds
	}
	if override.HealthIntervalSeconds > 0 {
		out.HealthIntervalSeconds = override.HealthIntervalSeconds
	}
	return out
}

func mergeBackupSpec(base, override BackupSpec) BackupSpec {
	out := base
	if strings.TrimSpace(override.Command) != "" {
		out.Command = override.Command
	}
	if strings.TrimSpace(override.ExportCommand) != "" {
		out.ExportCommand = override.ExportCommand
	}
	if strings.TrimSpace(override.RestoreCommand) != "" {
		out.RestoreCommand = override.RestoreCommand
	}
	if strings.TrimSpace(override.ArtifactDir) != "" {
		out.ArtifactDir = override.ArtifactDir
	}
	if override.Required {
		out.Required = override.Required
	}
	if override.RestoreCheck {
		out.RestoreCheck = override.RestoreCheck
	}
	if override.ExportTimeoutSeconds > 0 {
		out.ExportTimeoutSeconds = override.ExportTimeoutSeconds
	}
	if strings.TrimSpace(override.Schedule.Cron) != "" || override.Schedule.TimeoutSeconds > 0 {
		out.Schedule = override.Schedule
	}
	return out
}

func copySchedules(in map[string]Schedule) map[string]Schedule {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]Schedule, len(in))
	for name, schedule := range in {
		out[name] = schedule
	}
	return out
}

func mergeIngressConfig(base, override IngressConfig) IngressConfig {
	out := base
	if override.Caddy.Image != "" {
		out.Caddy.Image = override.Caddy.Image
	}
	if override.Caddy.Email != "" {
		out.Caddy.Email = override.Caddy.Email
	}
	if override.Caddy.DataVolume != "" {
		out.Caddy.DataVolume = override.Caddy.DataVolume
	}
	if override.Caddy.ConfigVolume != "" {
		out.Caddy.ConfigVolume = override.Caddy.ConfigVolume
	}
	return out
}

func mergeDockerConfig(base, override DockerConfig) DockerConfig {
	out := base
	if override.Network.Enabled != nil {
		out.Network.Enabled = override.Network.Enabled
	}
	if override.Network.Name != "" {
		out.Network.Name = override.Network.Name
	}
	if override.Network.Driver != "" {
		out.Network.Driver = override.Network.Driver
	}
	return out
}

func mergeHooks(base, override Hooks) Hooks {
	return Hooks{
		PreDeploy:    appendHookCommands(base.PreDeploy, override.PreDeploy),
		PreBuild:     appendHookCommands(base.PreBuild, override.PreBuild),
		PostDeploy:   appendHookCommands(base.PostDeploy, override.PostDeploy),
		DeployFailed: appendHookCommands(base.DeployFailed, override.DeployFailed),
	}
}

func mergeNotifications(base, override Notifications) Notifications {
	return Notifications{Webhooks: appendWebhookNotifications(base.Webhooks, override.Webhooks)}
}

func appendWebhookNotifications(groups ...[]WebhookNotification) []WebhookNotification {
	var out []WebhookNotification
	for _, group := range groups {
		for _, webhook := range group {
			out = append(out, webhook)
		}
	}
	return out
}

func appendHookCommands(groups ...[]HookCommand) []HookCommand {
	var out []HookCommand
	for _, group := range groups {
		for _, hook := range group {
			out = append(out, hook)
		}
	}
	return out
}

func mergeSSHConfig(base, override SSHConfig) SSHConfig {
	out := base
	if override.Port != 0 {
		out.Port = override.Port
	}
	if override.IdentityFile != "" {
		out.IdentityFile = override.IdentityFile
	}
	if override.KnownHostsFile != "" {
		out.KnownHostsFile = override.KnownHostsFile
	}
	if override.JumpHost != "" {
		out.JumpHost = override.JumpHost
	}
	if len(override.Options) > 0 {
		out.Options = mergeStringMap(base.Options, override.Options)
	}
	return out
}

func mergeStringMap(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(override))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range override {
		out[key] = value
	}
	return out
}

func mergeEnvList(base, override []string) []string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := make([]string, 0, len(base)+len(override))
	positions := map[string]int{}
	for _, raw := range base {
		item := strings.TrimSpace(raw)
		if item == "" {
			continue
		}
		name := envListName(item)
		if name == "" {
			out = append(out, item)
			continue
		}
		if idx, ok := positions[name]; ok {
			out[idx] = item
			continue
		}
		positions[name] = len(out)
		out = append(out, item)
	}
	for _, raw := range override {
		item := strings.TrimSpace(raw)
		if item == "" {
			continue
		}
		name := envListName(item)
		if name == "" {
			out = append(out, item)
			continue
		}
		if idx, ok := positions[name]; ok {
			out[idx] = item
			continue
		}
		positions[name] = len(out)
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func envListName(item string) string {
	name, _, _ := strings.Cut(item, "=")
	return strings.TrimSpace(name)
}

func mergeNames(groups ...[]string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, group := range groups {
		for _, raw := range group {
			name := strings.TrimSpace(raw)
			if name == "" {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func validateSecretNames(scope string, names []string) []string {
	var errs []string
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !validEnvName(name) {
			errs = append(errs, fmt.Sprintf("%s secret name %q is invalid", scope, name))
		}
	}
	return errs
}

func validEnvName(name string) bool {
	for i, r := range name {
		if i == 0 {
			if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
				return false
			}
			continue
		}
		if r != '_' &&
			(r < 'A' || r > 'Z') &&
			(r < 'a' || r > 'z') &&
			(r < '0' || r > '9') {
			return false
		}
	}
	return name != ""
}

func validateUserDataConfig(scope, userData, userDataFile string) []string {
	if strings.TrimSpace(userData) != "" && strings.TrimSpace(userDataFile) != "" {
		return []string{fmt.Sprintf("%s cannot define both user_data and user_data_file", scope)}
	}
	return nil
}

func validateSSHConfig(scope string, ssh SSHConfig) []string {
	var errs []string
	if ssh.Port < 0 || ssh.Port > 65535 {
		errs = append(errs, fmt.Sprintf("%s.port must be between 1 and 65535", scope))
	}
	for key := range ssh.Options {
		if strings.TrimSpace(key) == "" {
			errs = append(errs, fmt.Sprintf("%s.options cannot contain an empty option name", scope))
		}
	}
	return errs
}

func validateDockerConfig(scope string, docker DockerConfig) []string {
	var errs []string
	if name := strings.TrimSpace(docker.Network.Name); name != "" && !validDockerObjectName(name) {
		errs = append(errs, fmt.Sprintf("%s.network.name must contain only letters, numbers, dots, underscores, or dashes", scope))
	}
	if reservedDockerNetworkName(docker.Network.Name) {
		errs = append(errs, fmt.Sprintf("%s.network.name cannot be bridge, host, or none", scope))
	}
	if driver := strings.TrimSpace(docker.Network.Driver); driver != "" && !validDockerObjectName(driver) {
		errs = append(errs, fmt.Sprintf("%s.network.driver must contain only letters, numbers, dots, underscores, or dashes", scope))
	}
	return errs
}

func reservedDockerNetworkName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "bridge", "host", "none":
		return true
	default:
		return false
	}
}

func validDockerObjectName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		allowed := r >= 'a' && r <= 'z' ||
			r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' ||
			r == '_' || r == '.' || r == '-'
		if !allowed {
			return false
		}
	}
	return true
}

func validateCustomHostLabels(scope string, labels map[string]string) []string {
	var errs []string
	reserved := map[string]bool{
		"managed-by":  true,
		"project":     true,
		"environment": true,
		"pool":        true,
	}
	for key := range labels {
		if reserved[key] {
			errs = append(errs, fmt.Sprintf("%s cannot override reserved Ship label %q", scope, key))
		}
	}
	sort.Strings(errs)
	return errs
}

func validateContainerLabels(scope string, labels map[string]string) []string {
	var errs []string
	reserved := map[string]bool{
		"managed-by":  true,
		"project":     true,
		"environment": true,
		"service":     true,
		"accessory":   true,
		"replica":     true,
		"release":     true,
	}
	for key, value := range labels {
		label := fmt.Sprintf("%s[%q]", scope, key)
		if strings.TrimSpace(key) == "" {
			errs = append(errs, fmt.Sprintf("%s key is required", label))
			continue
		}
		if strings.ContainsAny(key, " \t\r\n=") {
			errs = append(errs, fmt.Sprintf("%s key cannot contain whitespace, newlines, or '='", label))
		}
		if reserved[key] {
			errs = append(errs, fmt.Sprintf("%s cannot override reserved Ship label %q", scope, key))
		}
		if strings.ContainsAny(value, "\r\n") {
			errs = append(errs, fmt.Sprintf("%s value cannot contain newlines", label))
		}
	}
	sort.Strings(errs)
	return errs
}

func validateNetworkAliases(scope string, aliases []string) []string {
	var errs []string
	for i, alias := range aliases {
		label := fmt.Sprintf("%s[%d]", scope, i)
		if strings.TrimSpace(alias) == "" {
			errs = append(errs, fmt.Sprintf("%s is required", label))
			continue
		}
		if !validDockerObjectName(alias) {
			errs = append(errs, fmt.Sprintf("%s must contain only letters, numbers, dots, underscores, or dashes", label))
		}
	}
	return errs
}

func validateDockerCacheSpecs(scope string, specs []string) []string {
	return validateDockerBuildSpecs(scope, specs)
}

func validateDockerBuildSpecs(scope string, specs []string) []string {
	var errs []string
	for i, spec := range specs {
		label := fmt.Sprintf("%s[%d]", scope, i)
		if strings.TrimSpace(spec) == "" {
			errs = append(errs, fmt.Sprintf("%s is required", label))
			continue
		}
		if strings.ContainsAny(spec, "\r\n") {
			errs = append(errs, fmt.Sprintf("%s cannot contain newlines", label))
		}
	}
	return errs
}

func validateDockerBuildString(scope, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if strings.ContainsAny(value, "\r\n") {
		return []string{fmt.Sprintf("%s cannot contain newlines", scope)}
	}
	return nil
}

func validateDockerBuildFlag(scope string, flag BuildxFlag) []string {
	value := flag.Value()
	if value == "" {
		return nil
	}
	if strings.ContainsAny(value, "\r\n") {
		return []string{fmt.Sprintf("%s cannot contain newlines", scope)}
	}
	return nil
}

func (c *Config) Environment(name string) (Environment, error) {
	env, ok := c.Environments[name]
	if !ok {
		return Environment{}, fmt.Errorf("unknown environment %q", name)
	}
	return env, nil
}

func ProjectRoot(start string) (string, error) {
	if start == "" {
		var err error
		start, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, DefaultConfigFile)); err == nil {
			return dir, nil
		}
		next := filepath.Dir(dir)
		if next == dir {
			return "", fmt.Errorf("could not find %s", DefaultConfigFile)
		}
		dir = next
	}
}

func Sample() string {
	return `project: example
registry: ghcr.io/acme/example

ingress:
  caddy:
    image: caddy:2

logging:
  driver: json-file
  options:
    max-size: 10m
    max-file: "3"

environments:
  staging:
    provider:
      hetzner:
        location: hel1
        server_type: cx23
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    hosts:
      pools:
        web:
          count: 1
        worker:
          count: 1
    services:
      web:
        image:
          build: .
          dockerfile: Dockerfile
        command: ./bin/server
        pool: web
        scale: 1
        ports: [3000]
        health:
          http: /up
        ingress:
          domains:
            - staging.example.com
        secrets:
          - DATABASE_URL

  production:
    provider:
      hetzner:
        location: hel1
        server_type: cx23
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    hosts:
      pools:
        web:
          count: 3
        worker:
          count: 2
        ingress:
          count: 2

services:
  web:
    image:
      build: .
      dockerfile: Dockerfile
    command: ./bin/server
    pool: web
    scale: 6
    ports: [3000]
    restart_policy: unless-stopped
    volumes:
      - uploads:/app/uploads
    resources:
      cpus: "1"
      memory: 512m
    health:
      http: /up
    ingress:
      domains:
        - example.com
      redirects:
        - from: [www.example.com]
          to: https://example.com
    secrets:
      - DATABASE_URL

  worker:
    image:
      build: .
    command: ./bin/worker
    pool: worker
    scale: 2
    health:
      command: ./bin/health-worker
    secrets:
      - JOB_SECRET

accessories:
  postgres:
    image: postgres:17
    pool: worker
    primary: true
    restart_policy: unless-stopped
    volumes:
      - postgres-data:/var/lib/postgresql/data
    backup:
      command: pg_dumpall
      restore_command: psql -f "$SHIP_BACKUP_ARTIFACT"
      required: true
      restore_check: true
    secrets:
      - POSTGRES_PASSWORD

secrets:
  - SESSION_SECRET
`
}
