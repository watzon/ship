package openstack

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
	LabelManagedBy   = provider.LabelManagedBy
	LabelProject     = provider.LabelProject
	LabelEnvironment = provider.LabelEnvironment
	LabelPool        = provider.LabelPool
)

type Client struct {
	AuthURL                     string
	ComputeURL                  string
	NetworkURL                  string
	Token                       string
	Region                      string
	Interface                   string
	ProjectID                   string
	ProjectName                 string
	ProjectDomainID             string
	ProjectDomainName           string
	UserID                      string
	Username                    string
	Password                    string
	UserDomainID                string
	UserDomainName              string
	ApplicationCredentialID     string
	ApplicationCredentialName   string
	ApplicationCredentialSecret string
	DryRun                      bool
	HTTP                        *http.Client
}

type ServerPlan = provider.HostPlan

type Server struct {
	ID         string               `json:"id"`
	Name       string               `json:"name"`
	Status     string               `json:"status"`
	AccessIPv4 string               `json:"accessIPv4"`
	AccessIPv6 string               `json:"accessIPv6"`
	Addresses  map[string][]Address `json:"addresses"`
	Metadata   map[string]string    `json:"metadata"`
	Tags       []string             `json:"tags"`
}

type Address struct {
	Address string `json:"addr"`
	Type    string `json:"OS-EXT-IPS:type"`
	Version int    `json:"version"`
}

type SecurityGroup struct {
	ID          string              `json:"id"`
	Name        string              `json:"name"`
	Description string              `json:"description"`
	ProjectID   string              `json:"project_id"`
	Tags        []string            `json:"tags"`
	Rules       []SecurityGroupRule `json:"security_group_rules"`
}

type SecurityGroupRule struct {
	ID              string `json:"id,omitempty"`
	Direction       string `json:"direction"`
	EtherType       string `json:"ethertype"`
	Protocol        string `json:"protocol,omitempty"`
	PortRangeMin    *int   `json:"port_range_min,omitempty"`
	PortRangeMax    *int   `json:"port_range_max,omitempty"`
	RemoteIPPrefix  string `json:"remote_ip_prefix,omitempty"`
	SecurityGroupID string `json:"security_group_id"`
	Description     string `json:"description,omitempty"`
}

type Port struct {
	ID          string    `json:"id"`
	NetworkID   string    `json:"network_id"`
	DeviceID    string    `json:"device_id"`
	DeviceOwner string    `json:"device_owner"`
	FixedIPs    []FixedIP `json:"fixed_ips"`
}

type FixedIP struct {
	IPAddress string `json:"ip_address"`
	SubnetID  string `json:"subnet_id"`
}

type FloatingIP struct {
	ID                string   `json:"id"`
	FloatingNetworkID string   `json:"floating_network_id"`
	FloatingIPAddress string   `json:"floating_ip_address"`
	FixedIPAddress    string   `json:"fixed_ip_address"`
	PortID            string   `json:"port_id"`
	Status            string   `json:"status"`
	Description       string   `json:"description"`
	Tags              []string `json:"tags"`
}

type ReconcileResult = provider.ReconcileResult

func NewFromEnv(dryRun bool, configs ...config.OpenStackConfig) Client {
	cfg := config.OpenStackConfig{}
	if len(configs) > 0 {
		cfg = configs[0]
	}
	return Client{
		AuthURL:                     firstNonEmpty(cfg.AuthURL, os.Getenv("OS_AUTH_URL")),
		ComputeURL:                  firstNonEmpty(cfg.ComputeURL, os.Getenv("OS_COMPUTE_API_URL")),
		NetworkURL:                  firstNonEmpty(cfg.NetworkURL, os.Getenv("OS_NETWORK_API_URL")),
		Token:                       os.Getenv("OS_AUTH_TOKEN"),
		Region:                      firstNonEmpty(cfg.Region, os.Getenv("OS_REGION_NAME")),
		Interface:                   firstNonEmpty(cfg.Interface, os.Getenv("OS_INTERFACE")),
		ProjectID:                   firstNonEmpty(cfg.ProjectID, os.Getenv("OS_PROJECT_ID")),
		ProjectName:                 firstNonEmpty(cfg.ProjectName, os.Getenv("OS_PROJECT_NAME")),
		ProjectDomainID:             firstNonEmpty(cfg.ProjectDomainID, os.Getenv("OS_PROJECT_DOMAIN_ID")),
		ProjectDomainName:           firstNonEmpty(cfg.ProjectDomainName, os.Getenv("OS_PROJECT_DOMAIN_NAME")),
		UserID:                      firstNonEmpty(cfg.UserID, os.Getenv("OS_USER_ID")),
		Username:                    firstNonEmpty(cfg.Username, os.Getenv("OS_USERNAME")),
		Password:                    os.Getenv("OS_PASSWORD"),
		UserDomainID:                firstNonEmpty(cfg.UserDomainID, os.Getenv("OS_USER_DOMAIN_ID")),
		UserDomainName:              firstNonEmpty(cfg.UserDomainName, os.Getenv("OS_USER_DOMAIN_NAME")),
		ApplicationCredentialID:     firstNonEmpty(cfg.ApplicationCredentialID, os.Getenv("OS_APPLICATION_CREDENTIAL_ID")),
		ApplicationCredentialName:   firstNonEmpty(cfg.ApplicationCredentialName, os.Getenv("OS_APPLICATION_CREDENTIAL_NAME")),
		ApplicationCredentialSecret: firstNonEmpty(cfg.ApplicationCredentialSecret, os.Getenv("OS_APPLICATION_CREDENTIAL_SECRET")),
		DryRun:                      dryRun,
		HTTP:                        http.DefaultClient,
	}
}

func (c Client) Name() string {
	return config.ProviderOpenStack
}

func (c Client) CredentialChecks(lookupEnv func(string) (string, bool)) []provider.CredentialCheck {
	_, tokenOK := lookupEnv("OS_AUTH_TOKEN")
	_, computeOK := lookupEnv("OS_COMPUTE_API_URL")
	_, authOK := lookupEnv("OS_AUTH_URL")
	_, appIDOK := lookupEnv("OS_APPLICATION_CREDENTIAL_ID")
	_, appNameOK := lookupEnv("OS_APPLICATION_CREDENTIAL_NAME")
	_, appSecretOK := lookupEnv("OS_APPLICATION_CREDENTIAL_SECRET")
	_, usernameOK := lookupEnv("OS_USERNAME")
	_, userIDOK := lookupEnv("OS_USER_ID")
	_, passwordOK := lookupEnv("OS_PASSWORD")
	_, projectIDOK := lookupEnv("OS_PROJECT_ID")
	_, projectNameOK := lookupEnv("OS_PROJECT_NAME")
	hasScopedToken := tokenOK && computeOK
	hasApplicationCredential := authOK && appSecretOK && (appIDOK || appNameOK)
	hasPassword := authOK && passwordOK && (usernameOK || userIDOK) && (projectIDOK || projectNameOK)
	return []provider.CredentialCheck{{
		Name:           "openstack credentials",
		Present:        hasScopedToken || hasApplicationCredential || hasPassword,
		Required:       true,
		PresentMessage: "OpenStack credentials are set",
		MissingMessage: "missing OS_AUTH_TOKEN/OS_COMPUTE_API_URL, OS_APPLICATION_CREDENTIAL_ID/SECRET, or OS_USERNAME/OS_PASSWORD/OS_PROJECT_*",
	}}
}

func DesiredServers(env config.Environment) []provider.HostPlan {
	return DesiredServersFor("", "", env)
}

func DesiredServersFor(project, environment string, env config.Environment) []provider.HostPlan {
	openstack := env.Provider.OpenStack
	if openstack == nil {
		return nil
	}
	return provider.HostPlans(project, environment, env, provider.HostPlanOptions{
		Location: openstack.Region,
		Size:     openstack.Flavor,
		Image:    openstack.Image,
		UserData: openstack.UserData,
	})
}

func (c Client) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.OpenStack == nil {
		return nil, fmt.Errorf("environment %q must define provider.openstack", environment)
	}
	return DesiredServersFor(project, environment, env), nil
}

func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	if env.Provider.OpenStack == nil {
		return provider.ReconcileResult{}, fmt.Errorf("environment %q must define provider.openstack", environment)
	}
	if strings.TrimSpace(project) == "" {
		return provider.ReconcileResult{}, fmt.Errorf("project is required")
	}
	if strings.TrimSpace(environment) == "" {
		return provider.ReconcileResult{}, fmt.Errorf("environment is required")
	}
	desired := DesiredServersFor(project, environment, env)
	result := provider.ReconcileResult{Desired: desired}
	if c.DryRun {
		return result, nil
	}
	client, err := c.authenticated(ctx)
	if err != nil {
		return provider.ReconcileResult{}, err
	}
	openstack := *env.Provider.OpenStack
	if openstack.SecurityGroup.ManagedValue(true) {
		sg, err := client.EnsureSecurityGroup(ctx, project, environment, openstack)
		if err != nil {
			return provider.ReconcileResult{}, err
		}
		openstack.SecurityGroups = appendSecurityGroup(openstack.SecurityGroups, sg.Name)
	}
	return provider.ReconcileHosts(ctx, project, environment, desired, reconcileBackend{client: client, openstack: openstack})
}

type reconcileBackend struct {
	client    Client
	openstack config.OpenStackConfig
}

func (b reconcileBackend) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	return b.client.List(ctx, project, environment)
}

func (b reconcileBackend) Create(ctx context.Context, plan provider.HostPlan) (provider.Host, error) {
	server, err := b.client.CreateServer(ctx, plan, b.openstack)
	if err != nil {
		return provider.Host{}, err
	}
	return hostFromServer(server), nil
}

func (c Client) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	client, err := c.authenticated(ctx)
	if err != nil {
		return nil, err
	}
	servers, err := client.ListServers(ctx, project, environment)
	if err != nil {
		return nil, err
	}
	hosts := make([]provider.Host, 0, len(servers))
	for _, server := range servers {
		hosts = append(hosts, hostFromServer(server))
	}
	return hosts, nil
}

func (c Client) Delete(ctx context.Context, host provider.Host) error {
	if strings.TrimSpace(host.ID) == "" {
		return fmt.Errorf("server id is required")
	}
	client, err := c.authenticated(ctx)
	if err != nil {
		return err
	}
	return client.DeleteServer(ctx, host.ID)
}

func (c Client) ListServers(ctx context.Context, project, environment string) ([]Server, error) {
	var out listServersResponse
	values := url.Values{}
	values.Set("limit", "1000")
	if err := c.computeRequest(ctx, http.MethodGet, "/servers/detail?"+values.Encode(), nil, &out); err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(out.Servers))
	for _, server := range out.Servers {
		if serverMatches(server, project, environment) {
			servers = append(servers, server)
		}
	}
	return servers, nil
}

func (c Client) CreateServer(ctx context.Context, plan provider.HostPlan, openstack config.OpenStackConfig) (Server, error) {
	openstack = withPlanDefaults(plan, openstack)
	serverBody := map[string]any{
		"name":      plan.Name,
		"imageRef":  plan.Image,
		"flavorRef": plan.Size,
		"metadata":  metadataForPlan(plan, openstack),
	}
	if openstack.Network != "" {
		if openstack.Network == "auto" || openstack.Network == "none" {
			serverBody["networks"] = openstack.Network
		} else {
			serverBody["networks"] = []map[string]string{{"uuid": openstack.Network}}
		}
	}
	if openstack.KeyName != "" {
		serverBody["key_name"] = openstack.KeyName
	}
	if len(openstack.SecurityGroups) > 0 {
		groups := make([]map[string]string, 0, len(openstack.SecurityGroups))
		for _, group := range openstack.SecurityGroups {
			if strings.TrimSpace(group) != "" {
				groups = append(groups, map[string]string{"name": group})
			}
		}
		serverBody["security_groups"] = groups
	}
	if openstack.AvailabilityZone != "" {
		serverBody["availability_zone"] = openstack.AvailabilityZone
	}
	if openstack.ConfigDrive != nil {
		serverBody["config_drive"] = *openstack.ConfigDrive
	}
	if strings.TrimSpace(plan.UserData) != "" {
		serverBody["user_data"] = base64.StdEncoding.EncodeToString([]byte(plan.UserData))
	}
	if len(openstack.Tags) > 0 {
		serverBody["tags"] = tagsForPlan(plan, openstack)
	}
	body := map[string]any{"server": serverBody}
	if len(openstack.SchedulerHints) > 0 {
		body["OS-SCH-HNT:scheduler_hints"] = openstack.SchedulerHints
	}
	var out createServerResponse
	if err := c.computeRequest(ctx, http.MethodPost, "/servers", body, &out); err != nil {
		return Server{}, err
	}
	server := out.Server
	if openstack.FloatingIP.Requested() {
		floatingIP, err := c.EnsureFloatingIP(ctx, server.ID, plan, openstack)
		if err != nil {
			return Server{}, err
		}
		server.AccessIPv4 = floatingIP.FloatingIPAddress
	}
	return server, nil
}

func (c Client) DeleteServer(ctx context.Context, serverID string) error {
	return c.computeRequest(ctx, http.MethodDelete, "/servers/"+url.PathEscape(serverID), nil, nil)
}

func (c Client) EnsureSecurityGroup(ctx context.Context, project, environment string, openstack config.OpenStackConfig) (SecurityGroup, error) {
	if strings.TrimSpace(c.NetworkURL) == "" {
		return SecurityGroup{}, fmt.Errorf("openstack network endpoint is required for managed security groups; set network_url or authenticate through Keystone catalog")
	}
	name := openstack.SecurityGroup.Name
	if name == "" {
		name = resourceName(project, environment, "security-group")
	}
	sg, ok, err := c.FindSecurityGroup(ctx, project, environment, name)
	if err != nil {
		return SecurityGroup{}, err
	}
	if !ok {
		sg, err = c.CreateSecurityGroup(ctx, project, environment, name, openstack)
		if err != nil {
			return SecurityGroup{}, err
		}
	}
	if err := c.EnsureSecurityGroupRules(ctx, sg.ID, securityGroupRules(openstack)); err != nil {
		return SecurityGroup{}, err
	}
	return sg, nil
}

func (c Client) FindSecurityGroup(ctx context.Context, project, environment, name string) (SecurityGroup, bool, error) {
	values := url.Values{}
	values.Set("name", name)
	var out listSecurityGroupsResponse
	if err := c.networkRequest(ctx, http.MethodGet, "/security-groups?"+values.Encode(), nil, &out); err != nil {
		return SecurityGroup{}, false, err
	}
	for _, sg := range out.SecurityGroups {
		if sg.Name == name && securityGroupMatches(sg, project, environment) {
			return sg, true, nil
		}
	}
	return SecurityGroup{}, false, nil
}

func (c Client) CreateSecurityGroup(ctx context.Context, project, environment, name string, openstack config.OpenStackConfig) (SecurityGroup, error) {
	description := openstack.SecurityGroup.Description
	if description == "" {
		description = "Managed by Ship for " + project + "/" + environment
	}
	body := map[string]any{"security_group": map[string]any{
		"name":        name,
		"description": description,
		"stateful":    true,
		"tags":        tagsFromLabels(provider.ShipLabels(project, environment, "security-group")),
	}}
	var out createSecurityGroupResponse
	if err := c.networkRequest(ctx, http.MethodPost, "/security-groups", body, &out); err != nil {
		return SecurityGroup{}, err
	}
	return out.SecurityGroup, nil
}

func (c Client) EnsureSecurityGroupRules(ctx context.Context, securityGroupID string, rules []SecurityGroupRule) error {
	existing, err := c.ListSecurityGroupRules(ctx, securityGroupID)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		rule.SecurityGroupID = securityGroupID
		if containsSecurityGroupRule(existing, rule) {
			continue
		}
		if err := c.CreateSecurityGroupRule(ctx, rule); err != nil {
			return err
		}
	}
	return nil
}

func (c Client) ListSecurityGroupRules(ctx context.Context, securityGroupID string) ([]SecurityGroupRule, error) {
	values := url.Values{}
	values.Set("security_group_id", securityGroupID)
	var out listSecurityGroupRulesResponse
	if err := c.networkRequest(ctx, http.MethodGet, "/security-group-rules?"+values.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.SecurityGroupRules, nil
}

func (c Client) CreateSecurityGroupRule(ctx context.Context, rule SecurityGroupRule) error {
	body := map[string]any{"security_group_rule": rule}
	var out createSecurityGroupRuleResponse
	return c.networkRequest(ctx, http.MethodPost, "/security-group-rules", body, &out)
}

func (c Client) EnsureFloatingIP(ctx context.Context, serverID string, plan provider.HostPlan, openstack config.OpenStackConfig) (FloatingIP, error) {
	if strings.TrimSpace(c.NetworkURL) == "" {
		return FloatingIP{}, fmt.Errorf("openstack network endpoint is required for floating IPs; set network_url or authenticate through Keystone catalog")
	}
	port, err := c.FindServerPort(ctx, serverID, openstack.Network)
	if err != nil {
		return FloatingIP{}, err
	}
	fipConfig := openstack.FloatingIP
	if fipConfig.Address != "" {
		floatingIP, ok, err := c.FindFloatingIPByAddress(ctx, fipConfig.Address)
		if err != nil {
			return FloatingIP{}, err
		}
		if !ok {
			if fipConfig.NetworkID == "" {
				return FloatingIP{}, fmt.Errorf("openstack floating_ip.address %q was not found and floating_ip.network_id is not set for allocation", fipConfig.Address)
			}
			return c.CreateFloatingIP(ctx, port, plan, fipConfig)
		}
		if floatingIP.PortID == port.ID {
			return floatingIP, nil
		}
		return c.UpdateFloatingIP(ctx, floatingIP.ID, port, fipConfig)
	}
	existing, err := c.ListFloatingIPs(ctx, url.Values{"port_id": []string{port.ID}})
	if err != nil {
		return FloatingIP{}, err
	}
	if len(existing) > 0 {
		return existing[0], nil
	}
	if fipConfig.NetworkID == "" {
		return FloatingIP{}, fmt.Errorf("openstack floating_ip.network_id is required to allocate a floating IP")
	}
	return c.CreateFloatingIP(ctx, port, plan, fipConfig)
}

func (c Client) FindServerPort(ctx context.Context, serverID, preferredNetworkID string) (Port, error) {
	ports, err := c.ListPorts(ctx, url.Values{"device_id": []string{serverID}})
	if err != nil {
		return Port{}, err
	}
	if len(ports) == 0 {
		return Port{}, fmt.Errorf("openstack server %q has no Neutron ports", serverID)
	}
	if preferredNetworkID != "" && preferredNetworkID != "auto" && preferredNetworkID != "none" {
		for _, port := range ports {
			if port.NetworkID == preferredNetworkID {
				return port, nil
			}
		}
	}
	return ports[0], nil
}

func (c Client) ListPorts(ctx context.Context, values url.Values) ([]Port, error) {
	path := "/ports"
	if len(values) > 0 {
		path += "?" + values.Encode()
	}
	var out listPortsResponse
	if err := c.networkRequest(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Ports, nil
}

func (c Client) FindFloatingIPByAddress(ctx context.Context, address string) (FloatingIP, bool, error) {
	floatingIPs, err := c.ListFloatingIPs(ctx, url.Values{"floating_ip_address": []string{address}})
	if err != nil {
		return FloatingIP{}, false, err
	}
	for _, floatingIP := range floatingIPs {
		if floatingIP.FloatingIPAddress == address {
			return floatingIP, true, nil
		}
	}
	return FloatingIP{}, false, nil
}

func (c Client) ListFloatingIPs(ctx context.Context, values url.Values) ([]FloatingIP, error) {
	path := "/floatingips"
	if len(values) > 0 {
		path += "?" + values.Encode()
	}
	var out listFloatingIPsResponse
	if err := c.networkRequest(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.FloatingIPs, nil
}

func (c Client) CreateFloatingIP(ctx context.Context, port Port, plan provider.HostPlan, cfg config.OpenStackFloatingIPConfig) (FloatingIP, error) {
	body := floatingIPBody(port, plan, cfg)
	var out floatingIPResponse
	if err := c.networkRequest(ctx, http.MethodPost, "/floatingips", map[string]any{"floatingip": body}, &out); err != nil {
		return FloatingIP{}, err
	}
	return out.FloatingIP, nil
}

func (c Client) UpdateFloatingIP(ctx context.Context, floatingIPID string, port Port, cfg config.OpenStackFloatingIPConfig) (FloatingIP, error) {
	body := map[string]any{"port_id": port.ID}
	if cfg.FixedIPAddress != "" {
		body["fixed_ip_address"] = cfg.FixedIPAddress
	}
	if cfg.Description != "" {
		body["description"] = cfg.Description
	}
	var out floatingIPResponse
	if err := c.networkRequest(ctx, http.MethodPut, "/floatingips/"+url.PathEscape(floatingIPID), map[string]any{"floatingip": body}, &out); err != nil {
		return FloatingIP{}, err
	}
	return out.FloatingIP, nil
}

func (c Client) authenticated(ctx context.Context) (Client, error) {
	if c.HTTP == nil {
		c.HTTP = http.DefaultClient
	}
	if strings.TrimSpace(c.ComputeURL) != "" && strings.TrimSpace(c.Token) != "" {
		return c, nil
	}
	token, computeURL, networkURL, err := c.authenticate(ctx)
	if err != nil {
		return Client{}, err
	}
	c.Token = token
	c.ComputeURL = computeURL
	if c.NetworkURL == "" {
		c.NetworkURL = networkURL
	}
	return c, nil
}

func (c Client) authenticate(ctx context.Context) (string, string, string, error) {
	if strings.TrimSpace(c.AuthURL) == "" {
		return "", "", "", fmt.Errorf("OS_AUTH_URL is required")
	}
	body, err := c.authPayload()
	if err != nil {
		return "", "", "", err
	}
	var out authResponse
	headers, err := c.request(ctx, http.MethodPost, strings.TrimRight(c.AuthURL, "/")+"/auth/tokens", "", body, &out)
	if err != nil {
		return "", "", "", err
	}
	token := headers.Get("X-Subject-Token")
	if token == "" {
		return "", "", "", fmt.Errorf("openstack auth response missing X-Subject-Token")
	}
	computeURL := c.endpointFromCatalog(out.Token.Catalog, "compute")
	if computeURL == "" {
		return "", "", "", fmt.Errorf("openstack auth catalog missing compute endpoint for region %q interface %q", c.Region, c.effectiveInterface())
	}
	networkURL := c.NetworkURL
	if networkURL == "" {
		networkURL = c.endpointFromCatalog(out.Token.Catalog, "network")
	}
	return token, computeURL, networkURL, nil
}

func (c Client) authPayload() (map[string]any, error) {
	if c.ApplicationCredentialID != "" || c.ApplicationCredentialName != "" {
		credential := map[string]any{"secret": c.ApplicationCredentialSecret}
		if c.ApplicationCredentialID != "" {
			credential["id"] = c.ApplicationCredentialID
		} else {
			credential["name"] = c.ApplicationCredentialName
			user := map[string]any{}
			if c.UserID != "" {
				user["id"] = c.UserID
			} else if c.Username != "" {
				user["name"] = c.Username
				user["domain"] = domain(c.UserDomainID, c.UserDomainName)
			}
			if len(user) > 0 {
				credential["user"] = user
			}
		}
		return map[string]any{"auth": map[string]any{"identity": map[string]any{
			"methods":                []string{"application_credential"},
			"application_credential": credential,
		}}}, nil
	}
	if c.Password == "" {
		return nil, fmt.Errorf("OS_PASSWORD or OS_APPLICATION_CREDENTIAL_SECRET is required")
	}
	user := map[string]any{"password": c.Password}
	if c.UserID != "" {
		user["id"] = c.UserID
	} else if c.Username != "" {
		user["name"] = c.Username
		user["domain"] = domain(c.UserDomainID, c.UserDomainName)
	} else {
		return nil, fmt.Errorf("OS_USERNAME or OS_USER_ID is required")
	}
	project := map[string]any{}
	if c.ProjectID != "" {
		project["id"] = c.ProjectID
	} else if c.ProjectName != "" {
		project["name"] = c.ProjectName
		project["domain"] = domain(c.ProjectDomainID, c.ProjectDomainName)
	} else {
		return nil, fmt.Errorf("OS_PROJECT_ID or OS_PROJECT_NAME is required")
	}
	return map[string]any{"auth": map[string]any{
		"identity": map[string]any{
			"methods":  []string{"password"},
			"password": map[string]any{"user": user},
		},
		"scope": map[string]any{"project": project},
	}}, nil
}

func (c Client) endpointFromCatalog(catalog []catalogEntry, serviceType string) string {
	wantInterface := c.effectiveInterface()
	for _, service := range catalog {
		if service.Type != serviceType {
			continue
		}
		for _, endpoint := range service.Endpoints {
			if endpoint.Interface != wantInterface {
				continue
			}
			if c.Region != "" && endpoint.Region != c.Region && endpoint.RegionID != c.Region {
				continue
			}
			return strings.TrimRight(endpoint.URL, "/")
		}
	}
	return ""
}

func (c Client) effectiveInterface() string {
	if c.Interface == "" {
		return "public"
	}
	return c.Interface
}

func (c Client) computeRequest(ctx context.Context, method, path string, payload any, out any) error {
	if strings.TrimSpace(c.ComputeURL) == "" {
		return fmt.Errorf("openstack compute endpoint is required")
	}
	if strings.TrimSpace(c.Token) == "" {
		return fmt.Errorf("openstack token is required")
	}
	_, err := c.request(ctx, method, strings.TrimRight(c.ComputeURL, "/")+path, c.Token, payload, out)
	return err
}

func (c Client) networkRequest(ctx context.Context, method, path string, payload any, out any) error {
	if strings.TrimSpace(c.NetworkURL) == "" {
		return fmt.Errorf("openstack network endpoint is required")
	}
	if strings.TrimSpace(c.Token) == "" {
		return fmt.Errorf("openstack token is required")
	}
	_, err := c.request(ctx, method, strings.TrimRight(c.NetworkURL, "/")+path, c.Token, payload, out)
	return err
}

func (c Client) request(ctx context.Context, method, endpoint, token string, payload any, out any) (http.Header, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	if c.HTTP == nil {
		c.HTTP = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Auth-Token", token)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return nil, fmt.Errorf("openstack api %s %s failed: %s: %s", method, endpoint, res.Status, strings.TrimSpace(string(data)))
	}
	if out != nil && res.StatusCode != http.StatusNoContent && res.ContentLength != 0 {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil && err != io.EOF {
			return nil, err
		}
	}
	return res.Header, nil
}

func withPlanDefaults(plan provider.HostPlan, openstack config.OpenStackConfig) config.OpenStackConfig {
	if plan.Location != "" {
		openstack.Region = plan.Location
	}
	if plan.Size != "" {
		openstack.Flavor = plan.Size
	}
	if plan.Image != "" {
		openstack.Image = plan.Image
	}
	if plan.UserData != "" {
		openstack.UserData = plan.UserData
	}
	return openstack
}

func metadataForPlan(plan provider.HostPlan, openstack config.OpenStackConfig) map[string]string {
	metadata := map[string]string{}
	for key, value := range openstack.Metadata {
		if key != "" && value != "" {
			metadata[key] = value
		}
	}
	labels := plan.Labels
	if labels == nil {
		labels = provider.ShipLabels(plan.Project, plan.Environment, plan.Pool)
	}
	for key, value := range labels {
		if key != "" && value != "" {
			metadata[key] = value
		}
	}
	return metadata
}

func tagsForPlan(plan provider.HostPlan, openstack config.OpenStackConfig) []string {
	seen := map[string]bool{}
	var tags []string
	for _, tag := range openstack.Tags {
		if tag != "" && !seen[tag] {
			tags = append(tags, tag)
			seen[tag] = true
		}
	}
	labels := plan.Labels
	if labels == nil {
		labels = provider.ShipLabels(plan.Project, plan.Environment, plan.Pool)
	}
	for key, value := range labels {
		tag := key + "-" + value
		tag = strings.ReplaceAll(tag, "_", "-")
		if tag != "" && !seen[tag] {
			tags = append(tags, tag)
			seen[tag] = true
		}
	}
	sort.Strings(tags)
	return tags
}

func securityGroupRules(openstack config.OpenStackConfig) []SecurityGroupRule {
	var rules []SecurityGroupRule
	if openstack.EffectiveSSHFirewall() == config.SSHFirewallManaged {
		for _, cidr := range openstack.SSHAllowedCIDRs {
			rules = append(rules, securityRule("ship-ssh", "tcp", cidr, 22))
		}
	}
	for _, cidr := range []string{"0.0.0.0/0", "::/0"} {
		rules = append(rules, securityRule("ship-http", "tcp", cidr, 80))
		rules = append(rules, securityRule("ship-https", "tcp", cidr, 443))
		rules = append(rules, securityRule("ship-http3", "udp", cidr, 443))
	}
	return rules
}

func securityRule(description, protocol, cidr string, port int) SecurityGroupRule {
	return SecurityGroupRule{
		Direction:      "ingress",
		EtherType:      etherType(cidr),
		Protocol:       protocol,
		PortRangeMin:   intPtr(port),
		PortRangeMax:   intPtr(port),
		RemoteIPPrefix: cidr,
		Description:    description,
	}
}

func floatingIPBody(port Port, plan provider.HostPlan, cfg config.OpenStackFloatingIPConfig) map[string]any {
	body := map[string]any{
		"port_id": port.ID,
		"tags":    tagsFromLabels(planLabels(plan)),
	}
	if cfg.NetworkID != "" {
		body["floating_network_id"] = cfg.NetworkID
	}
	if cfg.SubnetID != "" {
		body["subnet_id"] = cfg.SubnetID
	}
	if cfg.Address != "" {
		body["floating_ip_address"] = cfg.Address
	}
	if cfg.FixedIPAddress != "" {
		body["fixed_ip_address"] = cfg.FixedIPAddress
	}
	if cfg.Description != "" {
		body["description"] = cfg.Description
	}
	if cfg.DNSName != "" {
		body["dns_name"] = cfg.DNSName
	}
	if cfg.DNSDomain != "" {
		body["dns_domain"] = cfg.DNSDomain
	}
	if cfg.QOSPolicyID != "" {
		body["qos_policy_id"] = cfg.QOSPolicyID
	}
	if cfg.Distributed != nil {
		body["distributed"] = *cfg.Distributed
	}
	return body
}

func containsSecurityGroupRule(existing []SecurityGroupRule, want SecurityGroupRule) bool {
	for _, got := range existing {
		if got.Direction == want.Direction &&
			got.EtherType == want.EtherType &&
			got.Protocol == want.Protocol &&
			intValue(got.PortRangeMin) == intValue(want.PortRangeMin) &&
			intValue(got.PortRangeMax) == intValue(want.PortRangeMax) &&
			got.RemoteIPPrefix == want.RemoteIPPrefix {
			return true
		}
	}
	return false
}

func securityGroupMatches(sg SecurityGroup, project, environment string) bool {
	tags := map[string]bool{}
	for _, tag := range sg.Tags {
		tags[tag] = true
	}
	shipTags := tagsFromLabels(provider.ShipLabels(project, environment, "security-group"))
	for _, tag := range shipTags {
		if !tags[tag] {
			return false
		}
	}
	return true
}

func tagsFromLabels(labels map[string]string) []string {
	tags := make([]string, 0, len(labels))
	for key, value := range labels {
		if key == "" || value == "" {
			continue
		}
		tags = append(tags, safeTag(key+"-"+value))
	}
	sort.Strings(tags)
	return tags
}

func planLabels(plan provider.HostPlan) map[string]string {
	if plan.Labels != nil {
		return plan.Labels
	}
	return provider.ShipLabels(plan.Project, plan.Environment, plan.Pool)
}

func appendSecurityGroup(groups []string, name string) []string {
	for _, group := range groups {
		if group == name {
			return groups
		}
	}
	return append(groups, name)
}

func resourceName(project, environment, kind string) string {
	return safeTag("ship-" + project + "-" + environment + "-" + kind)
}

func safeTag(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func etherType(cidr string) string {
	if strings.Contains(cidr, ":") {
		return "IPv6"
	}
	return "IPv4"
}

func intPtr(v int) *int {
	return &v
}

func intValue(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func serverMatches(server Server, project, environment string) bool {
	metadata := server.Metadata
	return metadata[LabelManagedBy] == "ship" &&
		metadata[LabelProject] == project &&
		metadata[LabelEnvironment] == environment
}

func hostFromServer(server Server) provider.Host {
	labels := map[string]string{}
	for key, value := range server.Metadata {
		labels[key] = value
	}
	return provider.Host{
		ID:            server.ID,
		Name:          server.Name,
		Pool:          labels[LabelPool],
		PublicAddress: publicAddress(server),
		Labels:        labels,
	}
}

func publicAddress(server Server) string {
	if server.AccessIPv4 != "" {
		return server.AccessIPv4
	}
	for _, addresses := range server.Addresses {
		for _, addr := range addresses {
			if addr.Version == 4 && (addr.Type == "floating" || addr.Type == "public" || addr.Type == "") {
				return addr.Address
			}
		}
	}
	for _, addresses := range server.Addresses {
		for _, addr := range addresses {
			if addr.Version == 4 {
				return addr.Address
			}
		}
	}
	return server.AccessIPv6
}

func domain(id, name string) map[string]string {
	if id != "" {
		return map[string]string{"id": id}
	}
	if name != "" {
		return map[string]string{"name": name}
	}
	return map[string]string{"id": "default"}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

type listServersResponse struct {
	Servers []Server `json:"servers"`
}

type createServerResponse struct {
	Server Server `json:"server"`
}

type listSecurityGroupsResponse struct {
	SecurityGroups []SecurityGroup `json:"security_groups"`
}

type createSecurityGroupResponse struct {
	SecurityGroup SecurityGroup `json:"security_group"`
}

type listSecurityGroupRulesResponse struct {
	SecurityGroupRules []SecurityGroupRule `json:"security_group_rules"`
}

type createSecurityGroupRuleResponse struct {
	SecurityGroupRule SecurityGroupRule `json:"security_group_rule"`
}

type listPortsResponse struct {
	Ports []Port `json:"ports"`
}

type listFloatingIPsResponse struct {
	FloatingIPs []FloatingIP `json:"floatingips"`
}

type floatingIPResponse struct {
	FloatingIP FloatingIP `json:"floatingip"`
}

type authResponse struct {
	Token token `json:"token"`
}

type token struct {
	Catalog []catalogEntry `json:"catalog"`
}

type catalogEntry struct {
	Type      string            `json:"type"`
	Name      string            `json:"name"`
	Endpoints []catalogEndpoint `json:"endpoints"`
}

type catalogEndpoint struct {
	Interface string `json:"interface"`
	Region    string `json:"region"`
	RegionID  string `json:"region_id"`
	URL       string `json:"url"`
}
