package scaleway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

const (
	defaultAPIBase = "https://api.scaleway.com"

	LabelManagedBy   = provider.LabelManagedBy
	LabelProject     = provider.LabelProject
	LabelEnvironment = provider.LabelEnvironment
	LabelPool        = provider.LabelPool

	tagPrefix = "ship:"
)

type Client struct {
	SecretKey string
	ProjectID string
	Zone      string
	DryRun    bool
	HTTP      *http.Client
	BaseURL   string
}

type ServerPlan = provider.HostPlan

type Server struct {
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	Project        string        `json:"project"`
	Tags           []string      `json:"tags"`
	CommercialType string        `json:"commercial_type"`
	State          string        `json:"state"`
	AllowedActions []string      `json:"allowed_actions"`
	PublicIP       *IPAddress    `json:"public_ip"`
	PublicIPs      []IPAddress   `json:"public_ips"`
	SecurityGroup  SecurityGroup `json:"security_group"`
	Zone           string        `json:"zone"`
}

type IPAddress struct {
	Address string `json:"address"`
	Family  string `json:"family"`
}

type SecurityGroup struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Project     string   `json:"project"`
	Tags        []string `json:"tags"`
	Stateful    bool     `json:"stateful"`
	State       string   `json:"state"`
	Zone        string   `json:"zone"`
}

type SecurityGroupRule struct {
	ID           string `json:"id,omitempty"`
	Protocol     string `json:"protocol"`
	Direction    string `json:"direction"`
	Action       string `json:"action"`
	IPRange      string `json:"ip_range"`
	DestPortFrom int    `json:"dest_port_from,omitempty"`
	DestPortTo   int    `json:"dest_port_to,omitempty"`
	Position     int    `json:"position,omitempty"`
	Editable     bool   `json:"editable,omitempty"`
}

type ReconcileResult = provider.ReconcileResult

func NewFromEnv(dryRun bool, configs ...config.ScalewayConfig) Client {
	cfg := config.ScalewayConfig{}
	if len(configs) > 0 {
		cfg = configs[0]
	}
	secretKey := os.Getenv("SCW_SECRET_KEY")
	if secretKey == "" {
		secretKey = os.Getenv("SCALEWAY_SECRET_KEY")
	}
	return Client{
		SecretKey: secretKey,
		ProjectID: cfg.ProjectID,
		Zone:      cfg.Zone,
		DryRun:    dryRun,
		HTTP:      http.DefaultClient,
	}
}

func (c Client) Name() string {
	return config.ProviderScaleway
}

func (c Client) TokenPresent() bool {
	return strings.TrimSpace(c.SecretKey) != ""
}

func (c Client) CredentialChecks(lookupEnv func(string) (string, bool)) []provider.CredentialCheck {
	_, scwOK := lookupEnv("SCW_SECRET_KEY")
	_, legacyOK := lookupEnv("SCALEWAY_SECRET_KEY")
	return []provider.CredentialCheck{{
		Name:           "scaleway token",
		Present:        scwOK || legacyOK,
		Required:       true,
		PresentMessage: "SCW_SECRET_KEY is set",
		MissingMessage: "missing SCW_SECRET_KEY",
	}}
}

func DesiredServers(env config.Environment) []provider.HostPlan {
	return DesiredServersFor("", "", env)
}

func DesiredServersFor(project, environment string, env config.Environment) []provider.HostPlan {
	scaleway := env.Provider.Scaleway
	if scaleway == nil {
		return nil
	}
	return provider.HostPlans(project, environment, env, provider.HostPlanOptions{
		Location: scaleway.Zone,
		Size:     scaleway.CommercialType,
		Image:    scaleway.Image,
		UserData: scaleway.UserData,
	})
}

func (c Client) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.Scaleway == nil {
		return nil, fmt.Errorf("environment %q must define provider.scaleway", environment)
	}
	return DesiredServersFor(project, environment, env), nil
}

func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	if env.Provider.Scaleway == nil {
		return provider.ReconcileResult{}, fmt.Errorf("environment %q must define provider.scaleway", environment)
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

	backend, err := c.reconcileBackendFor(ctx, project, environment, env)
	if err != nil {
		return provider.ReconcileResult{}, err
	}
	return provider.ReconcileHosts(ctx, project, environment, desired, backend)
}

// reconcileBackendFor scopes the client, resolves the security group and builds
// the reconcile backend shared by Reconcile and CreateHost so both create
// servers identically.
func (c Client) reconcileBackendFor(ctx context.Context, project, environment string, env config.Environment) (reconcileBackend, error) {
	scaleway := *env.Provider.Scaleway
	client := c.withScope(scaleway)
	if scaleway.SecurityGroup.ManagedValue(true) {
		sg, err := client.EnsureSecurityGroup(ctx, project, environment, scaleway)
		if err != nil {
			return reconcileBackend{}, err
		}
		scaleway.SecurityGroup.ID = sg.ID
	}
	return reconcileBackend{client: client, scaleway: scaleway}, nil
}

// CreateHost provisions a single server using the backend Reconcile would
// build, so `ship migrate` can add a replacement alongside the existing one.
func (c Client) CreateHost(ctx context.Context, project, environment string, env config.Environment, plan provider.HostPlan) (provider.Host, error) {
	if env.Provider.Scaleway == nil {
		return provider.Host{}, fmt.Errorf("environment %q must define provider.scaleway", environment)
	}
	backend, err := c.reconcileBackendFor(ctx, project, environment, env)
	if err != nil {
		return provider.Host{}, err
	}
	return backend.Create(ctx, plan)
}

var _ provider.HostCreator = Client{}

type reconcileBackend struct {
	client   Client
	scaleway config.ScalewayConfig
}

func (b reconcileBackend) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	return b.client.List(ctx, project, environment)
}

func (b reconcileBackend) Create(ctx context.Context, plan provider.HostPlan) (provider.Host, error) {
	server, err := b.client.CreateServer(ctx, plan, b.scaleway)
	if err != nil {
		return provider.Host{}, err
	}
	return hostFromServer(server), nil
}

func (c Client) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	servers, err := c.ListServers(ctx, project, environment)
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
	return c.DeleteServer(ctx, host.ID)
}

func (c Client) ListServers(ctx context.Context, project, environment string) ([]Server, error) {
	if !c.TokenPresent() {
		return nil, fmt.Errorf("SCW_SECRET_KEY is required")
	}
	if strings.TrimSpace(c.ProjectID) == "" {
		return nil, fmt.Errorf("scaleway project_id is required")
	}
	if strings.TrimSpace(c.Zone) == "" {
		return nil, fmt.Errorf("scaleway zone is required")
	}

	page := 1
	var servers []Server
	for {
		values := url.Values{}
		values.Set("project", c.ProjectID)
		values.Set("tags", strings.Join([]string{
			tagForLabel(LabelProject, project),
			tagForLabel(LabelEnvironment, environment),
		}, ","))
		values.Set("per_page", "100")
		values.Set("page", strconv.Itoa(page))

		var out listServersResponse
		headers, err := c.request(ctx, http.MethodGet, "/instance/v1/zones/"+url.PathEscape(c.Zone)+"/servers?"+values.Encode(), nil, &out)
		if err != nil {
			return nil, err
		}
		for _, server := range out.Servers {
			if serverMatches(server, project, environment) {
				servers = append(servers, server)
			}
		}
		total, _ := strconv.Atoi(headers.Get("X-Total-Count"))
		if total > 0 && len(servers) >= total {
			break
		}
		if len(out.Servers) < 100 {
			break
		}
		page++
	}
	return servers, nil
}

func (c Client) CreateServer(ctx context.Context, plan provider.HostPlan, scaleway config.ScalewayConfig) (Server, error) {
	if !c.TokenPresent() {
		return Server{}, fmt.Errorf("SCW_SECRET_KEY is required")
	}
	scaleway = withPlanDefaults(plan, scaleway)
	client := c.withScope(scaleway)

	payload := map[string]any{
		"name":            plan.Name,
		"project":         scaleway.ProjectID,
		"commercial_type": plan.Size,
		"image":           plan.Image,
		"tags":            tagsForPlan(plan),
	}
	if scaleway.DynamicIPRequired != nil {
		payload["dynamic_ip_required"] = *scaleway.DynamicIPRequired
	}
	if scaleway.RoutedIPEnabled != nil {
		payload["routed_ip_enabled"] = *scaleway.RoutedIPEnabled
	}
	if scaleway.EnableIPv6 != nil {
		payload["enable_ipv6"] = *scaleway.EnableIPv6
	}
	if scaleway.Volumes != nil {
		payload["volumes"] = scaleway.Volumes
	}
	if scaleway.SecurityGroup.ID != "" {
		payload["security_group"] = scaleway.SecurityGroup.ID
	}

	var out createServerResponse
	if _, err := client.request(ctx, http.MethodPost, "/instance/v1/zones/"+url.PathEscape(client.Zone)+"/servers", payload, &out); err != nil {
		return Server{}, err
	}
	server := out.Server
	if strings.TrimSpace(plan.UserData) != "" {
		if err := client.SetUserData(ctx, server.ID, "cloud-init", plan.UserData); err != nil {
			return Server{}, err
		}
	}
	if bootAfterCreate(scaleway) && serverCanPowerOn(server) {
		if err := client.PowerOnServer(ctx, server.ID); err != nil {
			return Server{}, err
		}
	}
	return server, nil
}

func (c Client) DeleteServer(ctx context.Context, serverID string) error {
	if !c.TokenPresent() {
		return fmt.Errorf("SCW_SECRET_KEY is required")
	}
	_, err := c.request(ctx, http.MethodDelete, "/instance/v1/zones/"+url.PathEscape(c.Zone)+"/servers/"+url.PathEscape(serverID), nil, nil)
	return err
}

func (c Client) SetUserData(ctx context.Context, serverID, key, value string) error {
	if !c.TokenPresent() {
		return fmt.Errorf("SCW_SECRET_KEY is required")
	}
	path := "/instance/v1/zones/" + url.PathEscape(c.Zone) + "/servers/" + url.PathEscape(serverID) + "/user_data/" + url.PathEscape(key)
	_, err := c.requestRaw(ctx, http.MethodPatch, path, "text/plain", []byte(value), nil)
	return err
}

func (c Client) PowerOnServer(ctx context.Context, serverID string) error {
	if !c.TokenPresent() {
		return fmt.Errorf("SCW_SECRET_KEY is required")
	}
	path := "/instance/v1/zones/" + url.PathEscape(c.Zone) + "/servers/" + url.PathEscape(serverID) + "/action"
	_, err := c.request(ctx, http.MethodPost, path, map[string]string{"action": "poweron"}, nil)
	return err
}

func (c Client) EnsureSecurityGroup(ctx context.Context, project, environment string, scaleway config.ScalewayConfig) (SecurityGroup, error) {
	client := c.withScope(scaleway)
	if !client.TokenPresent() {
		return SecurityGroup{}, fmt.Errorf("SCW_SECRET_KEY is required")
	}
	if strings.TrimSpace(client.ProjectID) == "" {
		return SecurityGroup{}, fmt.Errorf("scaleway project_id is required")
	}
	if strings.TrimSpace(client.Zone) == "" {
		return SecurityGroup{}, fmt.Errorf("scaleway zone is required")
	}

	name := scaleway.SecurityGroup.Name
	if name == "" {
		name = resourceName(project, environment, "security-group")
	}
	sg, ok, err := client.FindSecurityGroup(ctx, project, environment, name)
	if err != nil {
		return SecurityGroup{}, err
	}
	if !ok {
		sg, err = client.CreateSecurityGroup(ctx, project, environment, name, scaleway)
		if err != nil {
			return SecurityGroup{}, err
		}
	}
	if err := client.SetSecurityGroupRules(ctx, sg.ID, securityGroupRules(scaleway)); err != nil {
		return SecurityGroup{}, err
	}
	return sg, nil
}

func (c Client) FindSecurityGroup(ctx context.Context, project, environment, name string) (SecurityGroup, bool, error) {
	values := url.Values{}
	values.Set("project", c.ProjectID)
	values.Set("name", name)
	values.Set("tags", strings.Join([]string{
		tagForLabel(LabelProject, project),
		tagForLabel(LabelEnvironment, environment),
		tagForLabel(LabelPool, "security-group"),
	}, ","))
	values.Set("per_page", "100")

	var out listSecurityGroupsResponse
	if _, err := c.request(ctx, http.MethodGet, "/instance/v1/zones/"+url.PathEscape(c.Zone)+"/security_groups?"+values.Encode(), nil, &out); err != nil {
		return SecurityGroup{}, false, err
	}
	for _, sg := range out.SecurityGroups {
		if sg.Name == name && securityGroupMatches(sg, project, environment) {
			return sg, true, nil
		}
	}
	return SecurityGroup{}, false, nil
}

func (c Client) CreateSecurityGroup(ctx context.Context, project, environment, name string, scaleway config.ScalewayConfig) (SecurityGroup, error) {
	description := scaleway.SecurityGroup.Description
	if description == "" {
		description = "Managed by Ship for " + project + "/" + environment
	}
	payload := map[string]any{
		"name":                    name,
		"description":             description,
		"project":                 c.ProjectID,
		"tags":                    tagsFromLabels(provider.ShipLabels(project, environment, "security-group")),
		"stateful":                true,
		"inbound_default_policy":  "drop",
		"outbound_default_policy": "accept",
	}
	var out createSecurityGroupResponse
	if _, err := c.request(ctx, http.MethodPost, "/instance/v1/zones/"+url.PathEscape(c.Zone)+"/security_groups", payload, &out); err != nil {
		return SecurityGroup{}, err
	}
	return out.SecurityGroup, nil
}

func (c Client) SetSecurityGroupRules(ctx context.Context, securityGroupID string, rules []SecurityGroupRule) error {
	payload := map[string]any{"rules": rules}
	var out setSecurityGroupRulesResponse
	_, err := c.request(ctx, http.MethodPut, "/instance/v1/zones/"+url.PathEscape(c.Zone)+"/security_groups/"+url.PathEscape(securityGroupID)+"/rules", payload, &out)
	return err
}

func (c Client) withScope(scaleway config.ScalewayConfig) Client {
	if c.ProjectID == "" {
		c.ProjectID = scaleway.ProjectID
	}
	if c.Zone == "" {
		c.Zone = scaleway.Zone
	}
	if c.HTTP == nil {
		c.HTTP = http.DefaultClient
	}
	return c
}

func (c Client) request(ctx context.Context, method, path string, payload any, out any) (http.Header, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	return c.do(ctx, method, path, "application/json", body, out)
}

func (c Client) requestRaw(ctx context.Context, method, path, contentType string, data []byte, out any) (http.Header, error) {
	return c.do(ctx, method, path, contentType, bytes.NewReader(data), out)
}

func (c Client) do(ctx context.Context, method, path, contentType string, body io.Reader, out any) (http.Header, error) {
	if c.HTTP == nil {
		c.HTTP = http.DefaultClient
	}
	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = defaultAPIBase
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Auth-Token", c.SecretKey)
	req.Header.Set("Accept", "application/json")
	if body != nil && contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return nil, fmt.Errorf("scaleway api %s %s failed: %s: %s", method, path, res.Status, strings.TrimSpace(string(data)))
	}
	if out != nil && res.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil && err != io.EOF {
			return nil, err
		}
	}
	return res.Header, nil
}

func withPlanDefaults(plan provider.HostPlan, scaleway config.ScalewayConfig) config.ScalewayConfig {
	if plan.Location != "" {
		scaleway.Zone = plan.Location
	}
	if plan.Size != "" {
		scaleway.CommercialType = plan.Size
	}
	if plan.Image != "" {
		scaleway.Image = plan.Image
	}
	if plan.UserData != "" {
		scaleway.UserData = plan.UserData
	}
	return scaleway
}

func bootAfterCreate(scaleway config.ScalewayConfig) bool {
	if scaleway.BootAfterCreate == nil {
		return true
	}
	return *scaleway.BootAfterCreate
}

func serverCanPowerOn(server Server) bool {
	for _, action := range server.AllowedActions {
		if action == "poweron" {
			return true
		}
	}
	return server.State == "stopped" || server.State == "stopped in place"
}

func securityGroupRules(scaleway config.ScalewayConfig) []SecurityGroupRule {
	var rules []SecurityGroupRule
	position := 1
	addRule := func(cidr, protocol string, port int) {
		rules = append(rules, SecurityGroupRule{
			Protocol:     protocol,
			Direction:    "inbound",
			Action:       "accept",
			IPRange:      cidr,
			DestPortFrom: port,
			DestPortTo:   port,
			Position:     position,
		})
		position++
	}
	if scaleway.EffectiveSSHFirewall() == config.SSHFirewallManaged {
		for _, cidr := range scaleway.SSHAllowedCIDRs {
			addRule(cidr, "TCP", 22)
		}
	}
	publicCIDRs := []string{"0.0.0.0/0"}
	if scaleway.EnableIPv6 != nil && *scaleway.EnableIPv6 {
		publicCIDRs = append(publicCIDRs, "::/0")
	}
	for _, cidr := range publicCIDRs {
		addRule(cidr, "TCP", 80)
		addRule(cidr, "TCP", 443)
		addRule(cidr, "UDP", 443)
	}
	return rules
}

func tagsForPlan(plan provider.HostPlan) []string {
	labels := plan.Labels
	if labels == nil {
		labels = provider.ShipLabels(plan.Project, plan.Environment, plan.Pool)
	}
	return tagsFromLabels(labels)
}

func tagsFromLabels(labels map[string]string) []string {
	tags := make([]string, 0, len(labels))
	for key, value := range labels {
		if key == "" || value == "" {
			continue
		}
		tags = append(tags, tagForLabel(key, value))
	}
	sort.Strings(tags)
	return tags
}

func tagForLabel(key, value string) string {
	return tagPrefix + key + "=" + value
}

func labelsFromTags(tags []string) map[string]string {
	labels := map[string]string{}
	for _, tag := range tags {
		if !strings.HasPrefix(tag, tagPrefix) {
			continue
		}
		parts := strings.SplitN(strings.TrimPrefix(tag, tagPrefix), "=", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		labels[parts[0]] = parts[1]
	}
	return labels
}

func serverMatches(server Server, project, environment string) bool {
	labels := labelsFromTags(server.Tags)
	return labels[LabelManagedBy] == "ship" &&
		labels[LabelProject] == project &&
		labels[LabelEnvironment] == environment
}

func securityGroupMatches(sg SecurityGroup, project, environment string) bool {
	labels := labelsFromTags(sg.Tags)
	return labels[LabelManagedBy] == "ship" &&
		labels[LabelProject] == project &&
		labels[LabelEnvironment] == environment &&
		labels[LabelPool] == "security-group"
}

func resourceName(project, environment, kind string) string {
	name := "ship-" + project + "-" + environment + "-" + kind
	return safeName(name)
}

func safeName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	lastDash := false
	for _, r := range name {
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

func hostFromServer(server Server) provider.Host {
	labels := labelsFromTags(server.Tags)
	address := ""
	if server.PublicIP != nil {
		address = server.PublicIP.Address
	}
	if address == "" {
		for _, ip := range server.PublicIPs {
			if ip.Family == "" || ip.Family == "inet" {
				address = ip.Address
				break
			}
		}
	}
	host := provider.Host{
		ID:            server.ID,
		Name:          server.Name,
		Pool:          labels[LabelPool],
		PublicAddress: address,
		Labels:        labels,
	}
	return host
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

type setSecurityGroupRulesResponse struct {
	Rules []SecurityGroupRule `json:"rules"`
}
