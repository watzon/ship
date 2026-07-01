package azure

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

const (
	defaultARMBase = "https://management.azure.com"
	computeAPI     = "2026-03-01"
	networkAPI     = "2025-05-01"
	armScope       = "https://management.azure.com/.default"

	LabelManagedBy   = provider.LabelManagedBy
	LabelProject     = provider.LabelProject
	LabelEnvironment = provider.LabelEnvironment
	LabelPool        = provider.LabelPool
)

type Client struct {
	AccessToken      string
	TenantID         string
	ClientID         string
	ClientSecret     string
	SubscriptionID   string
	ResourceGroup    string
	DryRun           bool
	HTTP             *http.Client
	BaseURL          string
	TokenURLTemplate string
}

type InstancePlan = provider.HostPlan

type VirtualMachine struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Location   string            `json:"location"`
	Tags       map[string]string `json:"tags"`
	Properties struct {
		NetworkProfile struct {
			NetworkInterfaces []struct {
				ID string `json:"id"`
			} `json:"networkInterfaces"`
		} `json:"networkProfile"`
	} `json:"properties"`
}

type PublicIPAddress struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Tags       map[string]string `json:"tags"`
	Properties struct {
		IPAddress string `json:"ipAddress"`
	} `json:"properties"`
}

type SecurityGroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ReconcileResult = provider.ReconcileResult

func NewFromEnv(dryRun bool, configs ...config.AzureConfig) Client {
	var cfg config.AzureConfig
	if len(configs) > 0 {
		cfg = configs[0]
	}
	return Client{
		AccessToken:    os.Getenv("AZURE_ACCESS_TOKEN"),
		TenantID:       os.Getenv("AZURE_TENANT_ID"),
		ClientID:       os.Getenv("AZURE_CLIENT_ID"),
		ClientSecret:   os.Getenv("AZURE_CLIENT_SECRET"),
		SubscriptionID: cfg.SubscriptionID,
		ResourceGroup:  cfg.ResourceGroup,
		DryRun:         dryRun,
		HTTP:           http.DefaultClient,
	}
}

func (c Client) Name() string {
	return config.ProviderAzure
}

func (c Client) CredentialChecks(lookupEnv func(string) (string, bool)) []provider.CredentialCheck {
	_, tokenOK := lookupEnv("AZURE_ACCESS_TOKEN")
	_, tenantOK := lookupEnv("AZURE_TENANT_ID")
	_, clientOK := lookupEnv("AZURE_CLIENT_ID")
	_, secretOK := lookupEnv("AZURE_CLIENT_SECRET")
	return []provider.CredentialCheck{{
		Name:           "azure credentials",
		Present:        tokenOK || tenantOK && clientOK && secretOK,
		Required:       true,
		PresentMessage: "AZURE_ACCESS_TOKEN or AZURE_TENANT_ID/AZURE_CLIENT_ID/AZURE_CLIENT_SECRET is set",
		MissingMessage: "missing AZURE_ACCESS_TOKEN or AZURE_TENANT_ID/AZURE_CLIENT_ID/AZURE_CLIENT_SECRET",
	}}
}

func DesiredVirtualMachines(env config.Environment) []provider.HostPlan {
	return DesiredVirtualMachinesFor("", "", env)
}

func DesiredVirtualMachinesFor(project, environment string, env config.Environment) []provider.HostPlan {
	azure := env.Provider.Azure
	if azure == nil {
		return nil
	}
	return provider.HostPlans(project, environment, env, provider.HostPlanOptions{
		Location: azure.Location,
		Size:     azure.VMSize,
		Image:    azure.Image,
		UserData: azure.UserData,
	})
}

func (c Client) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.Azure == nil {
		return nil, fmt.Errorf("environment %q must define provider.azure", environment)
	}
	return DesiredVirtualMachinesFor(project, environment, env), nil
}

func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	if env.Provider.Azure == nil {
		return provider.ReconcileResult{}, fmt.Errorf("environment %q must define provider.azure", environment)
	}
	if strings.TrimSpace(project) == "" {
		return provider.ReconcileResult{}, fmt.Errorf("project is required")
	}
	if strings.TrimSpace(environment) == "" {
		return provider.ReconcileResult{}, fmt.Errorf("environment is required")
	}
	desired := DesiredVirtualMachinesFor(project, environment, env)
	result := provider.ReconcileResult{Desired: desired}
	if c.DryRun {
		return result, nil
	}
	azure := *env.Provider.Azure
	c.SubscriptionID = azure.SubscriptionID
	c.ResourceGroup = azure.ResourceGroup
	securityGroupID := strings.TrimSpace(azure.SecurityGroup.ID)
	if azure.SecurityGroup.ManagedValue(true) {
		securityGroup, err := c.EnsureSecurityGroup(ctx, project, environment, azure)
		if err != nil {
			return provider.ReconcileResult{}, err
		}
		securityGroupID = securityGroup.ID
	}
	return provider.ReconcileHosts(ctx, project, environment, desired, reconcileBackend{
		client:          c,
		azure:           azure,
		securityGroupID: securityGroupID,
	})
}

type reconcileBackend struct {
	client          Client
	azure           config.AzureConfig
	securityGroupID string
}

func (b reconcileBackend) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	return b.client.List(ctx, project, environment)
}

func (b reconcileBackend) Create(ctx context.Context, plan provider.HostPlan) (provider.Host, error) {
	vm, err := b.client.CreateVirtualMachine(ctx, plan, b.azure, b.securityGroupID)
	if err != nil {
		return provider.Host{}, err
	}
	return b.client.hostFromVirtualMachine(ctx, vm), nil
}

func (c Client) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	vms, err := c.ListVirtualMachines(ctx, project, environment)
	if err != nil {
		return nil, err
	}
	hosts := make([]provider.Host, 0, len(vms))
	for _, vm := range vms {
		hosts = append(hosts, c.hostFromVirtualMachine(ctx, vm))
	}
	return hosts, nil
}

func (c Client) Delete(ctx context.Context, host provider.Host) error {
	if strings.TrimSpace(host.ID) == "" {
		return fmt.Errorf("virtual machine name is required")
	}
	if strings.TrimSpace(c.SubscriptionID) == "" || strings.TrimSpace(c.ResourceGroup) == "" {
		return fmt.Errorf("azure subscription_id and resource_group are required")
	}
	name := host.ID
	if err := c.DeleteVirtualMachine(ctx, name); err != nil {
		return err
	}
	_ = c.DeleteNetworkInterface(ctx, networkInterfaceName(name))
	_ = c.DeletePublicIPAddress(ctx, publicIPName(name))
	return nil
}

func (c Client) ListVirtualMachines(ctx context.Context, project, environment string) ([]VirtualMachine, error) {
	var out listVirtualMachinesResponse
	if err := c.do(ctx, http.MethodGet, c.resourceGroupPath("providers/Microsoft.Compute/virtualMachines", computeAPI), nil, &out); err != nil {
		return nil, err
	}
	var vms []VirtualMachine
	for _, vm := range out.Value {
		if vm.Tags[LabelManagedBy] == "ship" && vm.Tags[LabelProject] == project && vm.Tags[LabelEnvironment] == environment {
			vms = append(vms, vm)
		}
	}
	sort.SliceStable(vms, func(i, j int) bool {
		if vms[i].Tags[LabelPool] != vms[j].Tags[LabelPool] {
			return vms[i].Tags[LabelPool] < vms[j].Tags[LabelPool]
		}
		return vms[i].Name < vms[j].Name
	})
	return vms, nil
}

func (c Client) CreateVirtualMachine(ctx context.Context, plan provider.HostPlan, azure config.AzureConfig, securityGroupID string) (VirtualMachine, error) {
	tags := tagsForPlan(plan)
	publicIPID := ""
	if azure.PublicIP == nil || *azure.PublicIP {
		publicIP, err := c.EnsurePublicIPAddress(ctx, plan, azure)
		if err != nil {
			return VirtualMachine{}, err
		}
		publicIPID = publicIP.ID
	}
	nic, err := c.EnsureNetworkInterface(ctx, plan, azure, securityGroupID, publicIPID)
	if err != nil {
		return VirtualMachine{}, err
	}
	body := map[string]any{
		"location": plan.Location,
		"tags":     tags,
		"properties": map[string]any{
			"hardwareProfile": map[string]any{"vmSize": plan.Size},
			"storageProfile":  storageProfile(plan.Image, azure),
			"osProfile":       osProfile(plan, azure),
			"networkProfile": map[string]any{"networkInterfaces": []map[string]any{{
				"id":         nic.ID,
				"properties": map[string]any{"primary": true},
			}}},
		},
	}
	if c.DryRun {
		return VirtualMachine{Name: plan.Name, Location: plan.Location, Tags: tags}, nil
	}
	var vm VirtualMachine
	if err := c.do(ctx, http.MethodPut, c.resourceGroupPath("providers/Microsoft.Compute/virtualMachines/"+url.PathEscape(plan.Name), computeAPI), body, &vm); err != nil {
		return VirtualMachine{}, err
	}
	if vm.Name == "" {
		vm.Name = plan.Name
		vm.Tags = tags
	}
	return vm, nil
}

func (c Client) EnsureSecurityGroup(ctx context.Context, project, environment string, azure config.AzureConfig) (SecurityGroup, error) {
	name := azure.SecurityGroup.Name
	if name == "" {
		name = resourceName(project, environment, "nsg")
	}
	body := map[string]any{
		"location": azure.Location,
		"tags":     environmentTags(project, environment),
		"properties": map[string]any{
			"securityRules": securityRules(azure),
		},
	}
	if c.DryRun {
		return SecurityGroup{Name: name, ID: c.resourceID("Microsoft.Network/networkSecurityGroups", name)}, nil
	}
	var out SecurityGroup
	if err := c.do(ctx, http.MethodPut, c.resourceGroupPath("providers/Microsoft.Network/networkSecurityGroups/"+url.PathEscape(name), networkAPI), body, &out); err != nil {
		return SecurityGroup{}, err
	}
	if out.ID == "" {
		out.ID = c.resourceID("Microsoft.Network/networkSecurityGroups", name)
		out.Name = name
	}
	return out, nil
}

func (c Client) EnsurePublicIPAddress(ctx context.Context, plan provider.HostPlan, azure config.AzureConfig) (PublicIPAddress, error) {
	name := publicIPName(plan.Name)
	body := map[string]any{
		"location": plan.Location,
		"tags":     tagsForPlan(plan),
		"sku":      map[string]any{"name": "Standard"},
		"properties": map[string]any{
			"publicIPAllocationMethod": "Static",
		},
	}
	if c.DryRun {
		return PublicIPAddress{ID: c.resourceID("Microsoft.Network/publicIPAddresses", name), Name: name, Tags: tagsForPlan(plan)}, nil
	}
	var out PublicIPAddress
	if err := c.do(ctx, http.MethodPut, c.resourceGroupPath("providers/Microsoft.Network/publicIPAddresses/"+url.PathEscape(name), networkAPI), body, &out); err != nil {
		return PublicIPAddress{}, err
	}
	if out.ID == "" {
		out.ID = c.resourceID("Microsoft.Network/publicIPAddresses", name)
		out.Name = name
	}
	return out, nil
}

func (c Client) EnsureNetworkInterface(ctx context.Context, plan provider.HostPlan, azure config.AzureConfig, securityGroupID, publicIPID string) (networkInterface, error) {
	name := networkInterfaceName(plan.Name)
	ipConfigProperties := map[string]any{
		"privateIPAllocationMethod": "Dynamic",
		"subnet":                    map[string]any{"id": subnetID(c.SubscriptionID, azure)},
	}
	if publicIPID != "" {
		ipConfigProperties["publicIPAddress"] = map[string]any{"id": publicIPID}
	}
	properties := map[string]any{
		"ipConfigurations": []map[string]any{{
			"name":       "primary",
			"properties": ipConfigProperties,
		}},
	}
	if securityGroupID != "" {
		properties["networkSecurityGroup"] = map[string]any{"id": securityGroupID}
	}
	body := map[string]any{
		"location":   plan.Location,
		"tags":       tagsForPlan(plan),
		"properties": properties,
	}
	if c.DryRun {
		return networkInterface{ID: c.resourceID("Microsoft.Network/networkInterfaces", name), Name: name}, nil
	}
	var out networkInterface
	if err := c.do(ctx, http.MethodPut, c.resourceGroupPath("providers/Microsoft.Network/networkInterfaces/"+url.PathEscape(name), networkAPI), body, &out); err != nil {
		return networkInterface{}, err
	}
	if out.ID == "" {
		out.ID = c.resourceID("Microsoft.Network/networkInterfaces", name)
		out.Name = name
	}
	return out, nil
}

func (c Client) GetPublicIPAddress(ctx context.Context, name string) (PublicIPAddress, error) {
	var out PublicIPAddress
	if err := c.do(ctx, http.MethodGet, c.resourceGroupPath("providers/Microsoft.Network/publicIPAddresses/"+url.PathEscape(name), networkAPI), nil, &out); err != nil {
		return PublicIPAddress{}, err
	}
	return out, nil
}

func (c Client) DeleteVirtualMachine(ctx context.Context, name string) error {
	if c.DryRun {
		return nil
	}
	return c.do(ctx, http.MethodDelete, c.resourceGroupPath("providers/Microsoft.Compute/virtualMachines/"+url.PathEscape(name), computeAPI), nil, nil)
}

func (c Client) DeleteNetworkInterface(ctx context.Context, name string) error {
	if c.DryRun {
		return nil
	}
	return c.do(ctx, http.MethodDelete, c.resourceGroupPath("providers/Microsoft.Network/networkInterfaces/"+url.PathEscape(name), networkAPI), nil, nil)
}

func (c Client) DeletePublicIPAddress(ctx context.Context, name string) error {
	if c.DryRun {
		return nil
	}
	return c.do(ctx, http.MethodDelete, c.resourceGroupPath("providers/Microsoft.Network/publicIPAddresses/"+url.PathEscape(name), networkAPI), nil, nil)
}

func (c Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase()+path, reader)
	if err != nil {
		return err
	}
	token, err := c.bearerToken(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("azure %s %s failed: %s", method, path, strings.TrimSpace(string(data)))
	}
	if out == nil || len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c Client) bearerToken(ctx context.Context) (string, error) {
	if strings.TrimSpace(c.AccessToken) != "" {
		return strings.TrimSpace(c.AccessToken), nil
	}
	if strings.TrimSpace(c.TenantID) == "" || strings.TrimSpace(c.ClientID) == "" || strings.TrimSpace(c.ClientSecret) == "" {
		return "", fmt.Errorf("AZURE_ACCESS_TOKEN or AZURE_TENANT_ID/AZURE_CLIENT_ID/AZURE_CLIENT_SECRET is required")
	}
	values := url.Values{}
	values.Set("client_id", c.ClientID)
	values.Set("client_secret", c.ClientSecret)
	values.Set("scope", armScope)
	values.Set("grant_type", "client_credentials")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL(), strings.NewReader(values.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("azure token request failed: %s", strings.TrimSpace(string(data)))
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("azure token response missing access_token")
	}
	return out.AccessToken, nil
}

func (c Client) apiBase() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return defaultARMBase
}

func (c Client) tokenURL() string {
	if c.TokenURLTemplate != "" {
		return strings.ReplaceAll(c.TokenURLTemplate, "{tenant}", url.PathEscape(c.TenantID))
	}
	return "https://login.microsoftonline.com/" + url.PathEscape(c.TenantID) + "/oauth2/v2.0/token"
}

func (c Client) resourceGroupPath(path, apiVersion string) string {
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/%s?api-version=%s", url.PathEscape(c.SubscriptionID), url.PathEscape(c.ResourceGroup), path, url.QueryEscape(apiVersion))
}

func (c Client) resourceID(resourceType, name string) string {
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/%s/%s", c.SubscriptionID, c.ResourceGroup, resourceType, name)
}

func subnetID(subscriptionID string, azure config.AzureConfig) string {
	if azure.SubnetID != "" {
		return azure.SubnetID
	}
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/virtualNetworks/%s/subnets/%s", subscriptionID, azure.ResourceGroup, azure.VirtualNetwork, azure.Subnet)
}

func storageProfile(image string, azure config.AzureConfig) map[string]any {
	profile := map[string]any{
		"imageReference": imageReference(image),
		"osDisk": map[string]any{
			"createOption": "FromImage",
			"managedDisk":  map[string]any{"storageAccountType": defaultString(azure.OSDisk.Type, "Premium_LRS")},
		},
	}
	if azure.OSDisk.SizeGB > 0 {
		profile["osDisk"].(map[string]any)["diskSizeGB"] = azure.OSDisk.SizeGB
	}
	return profile
}

func imageReference(image string) map[string]any {
	if strings.HasPrefix(image, "/subscriptions/") {
		return map[string]any{"id": image}
	}
	parts := strings.Split(image, ":")
	if len(parts) == 4 {
		return map[string]any{
			"publisher": parts[0],
			"offer":     parts[1],
			"sku":       parts[2],
			"version":   parts[3],
		}
	}
	return map[string]any{"id": image}
}

func osProfile(plan provider.HostPlan, azure config.AzureConfig) map[string]any {
	disablePassword := true
	if azure.DisablePasswordLogin != nil {
		disablePassword = *azure.DisablePasswordLogin
	}
	profile := map[string]any{
		"computerName":  plan.Name,
		"adminUsername": azure.AdminUsername,
		"linuxConfiguration": map[string]any{
			"disablePasswordAuthentication": disablePassword,
			"ssh": map[string]any{"publicKeys": []map[string]any{{
				"path":    "/home/" + azure.AdminUsername + "/.ssh/authorized_keys",
				"keyData": azure.SSHPublicKey,
			}}},
		},
	}
	if plan.UserData != "" {
		profile["customData"] = base64.StdEncoding.EncodeToString([]byte(plan.UserData))
	}
	return profile
}

func securityRules(azure config.AzureConfig) []map[string]any {
	rules := []map[string]any{
		securityRule("ship-http", 110, "Tcp", "80", []string{"*"}),
		securityRule("ship-https", 120, "Tcp", "443", []string{"*"}),
		securityRule("ship-http3", 130, "Udp", "443", []string{"*"}),
	}
	if azure.EffectiveSSHFirewall() == config.SSHFirewallManaged {
		rules = append([]map[string]any{securityRule("ship-ssh", 100, "Tcp", "22", azure.SSHAllowedCIDRs)}, rules...)
	}
	return rules
}

func securityRule(name string, priority int, protocol, port string, sourcePrefixes []string) map[string]any {
	properties := map[string]any{
		"priority":                 priority,
		"protocol":                 protocol,
		"access":                   "Allow",
		"direction":                "Inbound",
		"sourcePortRange":          "*",
		"destinationPortRange":     port,
		"destinationAddressPrefix": "*",
	}
	if len(sourcePrefixes) == 1 {
		properties["sourceAddressPrefix"] = sourcePrefixes[0]
	} else {
		properties["sourceAddressPrefixes"] = sourcePrefixes
	}
	return map[string]any{"name": name, "properties": properties}
}

func (c Client) hostFromVirtualMachine(ctx context.Context, vm VirtualMachine) provider.Host {
	address := ""
	if vm.Name != "" {
		if publicIP, err := c.GetPublicIPAddress(ctx, publicIPName(vm.Name)); err == nil {
			address = publicIP.Properties.IPAddress
		}
	}
	return provider.Host{
		ID:            vm.Name,
		Name:          vm.Name,
		Pool:          vm.Tags[LabelPool],
		PublicAddress: address,
		Labels:        vm.Tags,
	}
}

func tagsForPlan(plan provider.HostPlan) map[string]string {
	tags := map[string]string{"Name": plan.Name}
	for key, value := range plan.Labels {
		if key != "" && value != "" {
			tags[key] = value
		}
	}
	return tags
}

func environmentTags(project, environment string) map[string]string {
	return map[string]string{
		LabelManagedBy:   "ship",
		LabelProject:     project,
		LabelEnvironment: environment,
	}
}

func publicIPName(vmName string) string {
	return vmName + "-pip"
}

func networkInterfaceName(vmName string) string {
	return vmName + "-nic"
}

func resourceName(project, environment, kind string) string {
	return strings.Join([]string{"ship", safeName(project), safeName(environment), kind}, "-")
}

func safeName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		allowed := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-'
		if allowed {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "ship"
	}
	return out
}

func defaultString(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

type networkInterface struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type listVirtualMachinesResponse struct {
	Value []VirtualMachine `json:"value"`
}
