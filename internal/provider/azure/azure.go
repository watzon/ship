package azure

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

const (
	defaultARMBase = "https://management.azure.com"
	computeAPI     = "2026-03-01"
	networkAPI     = "2025-05-01"
	armScope       = "https://management.azure.com/.default"

	defaultOperationPollInterval = 2 * time.Second
	defaultOperationTimeout      = 30 * time.Minute

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
	// PollInterval controls how often Azure long-running operations are checked
	// when the service does not return Retry-After.
	PollInterval time.Duration
	// OperationTimeout bounds Azure long-running operations when the caller has
	// no earlier deadline.
	OperationTimeout time.Duration
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
	backend, err := c.reconcileBackendFor(ctx, project, environment, env)
	if err != nil {
		return provider.ReconcileResult{}, err
	}
	return provider.ReconcileHosts(ctx, project, environment, desired, backend)
}

// reconcileBackendFor resolves the Azure subscription, resource group and
// security group and builds the reconcile backend shared by Reconcile and
// CreateHost so both create virtual machines identically.
func (c Client) reconcileBackendFor(ctx context.Context, project, environment string, env config.Environment) (reconcileBackend, error) {
	azure := *env.Provider.Azure
	c.SubscriptionID = azure.SubscriptionID
	c.ResourceGroup = azure.ResourceGroup
	securityGroupID := strings.TrimSpace(azure.SecurityGroup.ID)
	if azure.SecurityGroup.ManagedValue(true) {
		securityGroup, err := c.EnsureSecurityGroup(ctx, project, environment, azure)
		if err != nil {
			return reconcileBackend{}, err
		}
		securityGroupID = securityGroup.ID
	}
	return reconcileBackend{
		client:          c,
		azure:           azure,
		securityGroupID: securityGroupID,
	}, nil
}

// CreateHost provisions a single virtual machine using the backend Reconcile
// would build, so `ship migrate` can add a replacement alongside the existing
// one.
func (c Client) CreateHost(ctx context.Context, project, environment string, env config.Environment, plan provider.HostPlan) (provider.Host, error) {
	if env.Provider.Azure == nil {
		return provider.Host{}, fmt.Errorf("environment %q must define provider.azure", environment)
	}
	backend, err := c.reconcileBackendFor(ctx, project, environment, env)
	if err != nil {
		return provider.Host{}, err
	}
	return backend.Create(ctx, plan)
}

var _ provider.HostCreator = Client{}

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
	nicName := networkInterfaceName(name)
	publicIPAddressName := publicIPName(name)
	var cleanupErrors []error
	if err := c.DeleteNetworkInterface(ctx, nicName); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("network interface %q cleanup failed: %w", nicName, err))
	}
	if err := c.DeletePublicIPAddress(ctx, publicIPAddressName); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("public IP address %q cleanup failed: %w", publicIPAddressName, err))
	}
	return errors.Join(cleanupErrors...)
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
	return c.deleteResource(
		ctx,
		c.resourceGroupPath("providers/Microsoft.Compute/virtualMachines/"+url.PathEscape(name), computeAPI),
		fmt.Sprintf("virtual machine %q", name),
		false,
	)
}

func (c Client) DeleteNetworkInterface(ctx context.Context, name string) error {
	if c.DryRun {
		return nil
	}
	return c.deleteResource(
		ctx,
		c.resourceGroupPath("providers/Microsoft.Network/networkInterfaces/"+url.PathEscape(name), networkAPI),
		fmt.Sprintf("network interface %q", name),
		true,
	)
}

func (c Client) DeletePublicIPAddress(ctx context.Context, name string) error {
	if c.DryRun {
		return nil
	}
	return c.deleteResource(
		ctx,
		c.resourceGroupPath("providers/Microsoft.Network/publicIPAddresses/"+url.PathEscape(name), networkAPI),
		fmt.Sprintf("public IP address %q", name),
		true,
	)
}

func (c Client) do(ctx context.Context, method, path string, body any, out any) error {
	resp, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	if !resp.successful() {
		return responseError(method, path, resp)
	}
	if out == nil || len(strings.TrimSpace(string(resp.Body))) == 0 {
		return nil
	}
	return json.Unmarshal(resp.Body, out)
}

type azureResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

func (r azureResponse) successful() bool {
	return r.StatusCode >= 200 && r.StatusCode < 300
}

func (c Client) request(ctx context.Context, method, target string, body any) (azureResponse, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return azureResponse{}, err
		}
		reader = bytes.NewReader(data)
	}
	requestURL, err := c.resolveRequestURL(target)
	if err != nil {
		return azureResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, reader)
	if err != nil {
		return azureResponse{}, err
	}
	token, err := c.bearerToken(ctx)
	if err != nil {
		return azureResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return azureResponse{}, err
	}
	data, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	result := azureResponse{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: data}
	if readErr != nil {
		return result, readErr
	}
	if closeErr != nil {
		return result, closeErr
	}
	return result, nil
}

func responseError(method, target string, resp azureResponse) error {
	return fmt.Errorf("azure %s %s failed: %s", method, target, strings.TrimSpace(string(resp.Body)))
}

func (c Client) deleteResource(ctx context.Context, path, resource string, notFoundOK bool) error {
	resp, err := c.request(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	if notFoundOK && resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if !resp.successful() {
		return responseError(http.MethodDelete, path, resp)
	}
	if resp.StatusCode != http.StatusAccepted {
		return nil
	}
	operationURL := strings.TrimSpace(resp.Header.Get("Azure-AsyncOperation"))
	if operationURL == "" {
		operationURL = strings.TrimSpace(resp.Header.Get("Location"))
	}
	if operationURL == "" {
		return fmt.Errorf("azure delete %s returned 202 without an operation URL", resource)
	}
	resolvedURL, err := c.resolveOperationURL(operationURL)
	if err != nil {
		return fmt.Errorf("azure delete %s returned an invalid operation URL: %w", resource, err)
	}
	return c.waitOperation(ctx, resolvedURL, resource)
}

type azureOperationError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type azureOperation struct {
	Status string               `json:"status"`
	Error  *azureOperationError `json:"error"`
}

func (c Client) waitOperation(ctx context.Context, operationURL, resource string) error {
	ctx, cancel := c.operationContext(ctx)
	defer cancel()
	interval := c.PollInterval
	if interval <= 0 {
		interval = defaultOperationPollInterval
	}
	for {
		resp, err := c.request(ctx, http.MethodGet, operationURL, nil)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if !resp.successful() {
			return fmt.Errorf("azure %s deletion operation request failed with status %d: %s", resource, resp.StatusCode, strings.TrimSpace(string(resp.Body)))
		}
		var operation azureOperation
		if err := json.Unmarshal(resp.Body, &operation); err != nil {
			return fmt.Errorf("decode azure %s deletion operation: %w", resource, err)
		}
		status := strings.TrimSpace(operation.Status)
		if status == "" {
			return fmt.Errorf("azure %s deletion operation response missing status", resource)
		}
		switch strings.ToLower(status) {
		case "succeeded":
			return nil
		case "failed", "canceled":
			return operationFailure(resource, status, operation.Error)
		}

		delay := interval
		if retryDelay, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
			delay = retryDelay
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func operationFailure(resource, status string, operationError *azureOperationError) error {
	message := fmt.Sprintf("azure %s deletion operation status %s", resource, status)
	if operationError == nil {
		return errors.New(message)
	}
	code := strings.TrimSpace(operationError.Code)
	detail := strings.TrimSpace(operationError.Message)
	switch {
	case code != "" && detail != "":
		return fmt.Errorf("%s: %s: %s", message, code, detail)
	case code != "":
		return fmt.Errorf("%s: %s", message, code)
	case detail != "":
		return fmt.Errorf("%s: %s", message, detail)
	default:
		return errors.New(message)
	}
}

func (c Client) operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := c.OperationTimeout
	if timeout <= 0 {
		timeout = defaultOperationTimeout
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= timeout {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 && seconds <= int64((1<<63-1)/time.Second) {
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := when.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

func (c Client) resolveOperationURL(operationURL string) (string, error) {
	return c.resolveRequestURL(operationURL)
}

func (c Client) resolveRequestURL(target string) (string, error) {
	baseURL := c.apiBase()
	base, err := url.Parse(baseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("invalid Azure API base URL")
	}
	reference, err := url.Parse(target)
	if err != nil {
		return "", fmt.Errorf("invalid Azure request URL")
	}
	var resolved *url.URL
	if strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "//") {
		resolved, err = url.Parse(strings.TrimRight(baseURL, "/") + target)
		if err != nil {
			return "", fmt.Errorf("invalid Azure request URL")
		}
	} else {
		baseCopy := *base
		baseCopy.Path = strings.TrimRight(baseCopy.Path, "/") + "/"
		resolved = baseCopy.ResolveReference(reference)
	}
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return "", fmt.Errorf("Azure request URL must use HTTP or HTTPS")
	}
	if !strings.EqualFold(resolved.Scheme, base.Scheme) || !strings.EqualFold(resolved.Host, base.Host) {
		return "", fmt.Errorf("Azure request URL must use the configured API origin")
	}
	return resolved.String(), nil
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
