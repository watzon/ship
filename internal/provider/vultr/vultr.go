package vultr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
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
	defaultAPIBase = "https://api.vultr.com/v2"

	LabelManagedBy   = provider.LabelManagedBy
	LabelProject     = provider.LabelProject
	LabelEnvironment = provider.LabelEnvironment
	LabelPool        = provider.LabelPool

	tagPrefix = "ship:"
)

type Client struct {
	Token   string
	DryRun  bool
	HTTP    *http.Client
	BaseURL string
}

type InstancePlan = provider.HostPlan

type Instance struct {
	ID     string   `json:"id"`
	Label  string   `json:"label"`
	MainIP string   `json:"main_ip"`
	Tags   []string `json:"tags"`
}

type FirewallGroup struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

type FirewallRule struct {
	ID         int    `json:"id"`
	IPType     string `json:"ip_type"`
	Protocol   string `json:"protocol"`
	Port       string `json:"port"`
	Subnet     string `json:"subnet"`
	SubnetSize int    `json:"subnet_size"`
	Source     string `json:"source"`
	Notes      string `json:"notes"`
}

type ReconcileResult = provider.ReconcileResult

func NewFromEnv(dryRun bool) Client {
	return Client{Token: os.Getenv("VULTR_API_KEY"), DryRun: dryRun, HTTP: http.DefaultClient}
}

func (c Client) Name() string {
	return config.ProviderVultr
}

func (c Client) TokenPresent() bool {
	return strings.TrimSpace(c.Token) != ""
}

func (c Client) CredentialChecks(lookupEnv func(string) (string, bool)) []provider.CredentialCheck {
	_, ok := lookupEnv("VULTR_API_KEY")
	return []provider.CredentialCheck{{
		Name:           "vultr token",
		Present:        ok,
		Required:       true,
		PresentMessage: "VULTR_API_KEY is set",
		MissingMessage: "missing VULTR_API_KEY",
	}}
}

func DesiredInstances(env config.Environment) []provider.HostPlan {
	return DesiredInstancesFor("", "", env)
}

func DesiredInstancesFor(project, environment string, env config.Environment) []provider.HostPlan {
	vultr := env.Provider.Vultr
	if vultr == nil {
		return nil
	}
	return provider.HostPlans(project, environment, env, provider.HostPlanOptions{
		Location: vultr.Region,
		Size:     vultr.Plan,
		Image:    sourceDescription(*vultr),
		UserData: vultr.UserData,
	})
}

func (c Client) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.Vultr == nil {
		return nil, fmt.Errorf("environment %q must define provider.vultr", environment)
	}
	return DesiredInstancesFor(project, environment, env), nil
}

func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	if env.Provider.Vultr == nil {
		return provider.ReconcileResult{}, fmt.Errorf("environment %q must define provider.vultr", environment)
	}
	if strings.TrimSpace(project) == "" {
		return provider.ReconcileResult{}, fmt.Errorf("project is required")
	}
	if strings.TrimSpace(environment) == "" {
		return provider.ReconcileResult{}, fmt.Errorf("environment is required")
	}

	desired := DesiredInstancesFor(project, environment, env)
	result := provider.ReconcileResult{Desired: desired}
	if c.DryRun {
		return result, nil
	}

	vultr := *env.Provider.Vultr
	if vultr.FirewallGroupID == "" && vultr.Firewall.EnabledValue(true) {
		firewall, err := c.EnsureFirewall(ctx, project, environment, vultr)
		if err != nil {
			return provider.ReconcileResult{}, err
		}
		vultr.FirewallGroupID = firewall.ID
	}

	return provider.ReconcileHosts(ctx, project, environment, desired, reconcileBackend{client: c, vultr: vultr})
}

type reconcileBackend struct {
	client Client
	vultr  config.VultrConfig
}

func (b reconcileBackend) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	return b.client.List(ctx, project, environment)
}

func (b reconcileBackend) Create(ctx context.Context, plan provider.HostPlan) (provider.Host, error) {
	instance, err := b.client.CreateInstance(ctx, plan, b.vultr)
	if err != nil {
		return provider.Host{}, err
	}
	return hostFromInstance(instance), nil
}

func (c Client) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	instances, err := c.ListInstances(ctx, project, environment)
	if err != nil {
		return nil, err
	}
	hosts := make([]provider.Host, 0, len(instances))
	for _, instance := range instances {
		hosts = append(hosts, hostFromInstance(instance))
	}
	return hosts, nil
}

func (c Client) Delete(ctx context.Context, host provider.Host) error {
	if strings.TrimSpace(host.ID) == "" {
		return fmt.Errorf("instance id is required")
	}
	return c.DeleteInstance(ctx, host.ID)
}

func (c Client) ListInstances(ctx context.Context, project, environment string) ([]Instance, error) {
	if !c.TokenPresent() {
		return nil, fmt.Errorf("VULTR_API_KEY is required")
	}
	var instances []Instance
	cursor := ""
	for {
		values := url.Values{}
		values.Set("per_page", "500")
		if cursor != "" {
			values.Set("cursor", cursor)
		}
		var out listInstancesResponse
		if err := c.request(ctx, http.MethodGet, "/instances?"+values.Encode(), nil, &out); err != nil {
			return nil, err
		}
		for _, instance := range out.Instances {
			if instanceMatches(instance, project, environment) {
				instances = append(instances, instance)
			}
		}
		if out.Meta.Links.Next == "" {
			break
		}
		cursor = nextCursor(out.Meta.Links.Next)
	}
	return instances, nil
}

func (c Client) CreateInstance(ctx context.Context, plan provider.HostPlan, vultr config.VultrConfig) (Instance, error) {
	if !c.TokenPresent() {
		return Instance{}, fmt.Errorf("VULTR_API_KEY is required")
	}
	tags := tagsForPlan(plan)
	body := map[string]any{
		"region": plan.Location,
		"plan":   plan.Size,
		"label":  plan.Name,
		"tags":   tags,
	}
	if vultr.Hostname != "" {
		body["hostname"] = vultr.Hostname
	}
	if len(vultr.SSHKeyIDs) > 0 {
		body["sshkey_id"] = vultr.SSHKeyIDs
	}
	if vultr.FirewallGroupID != "" {
		body["firewall_group_id"] = vultr.FirewallGroupID
	}
	if vultr.Backups != nil {
		body["backups"] = vultrBackupMode(*vultr.Backups)
	}
	if vultr.IPv6 != nil {
		body["enable_ipv6"] = *vultr.IPv6
	}
	if vultr.DDoSProtection != nil {
		body["ddos_protection"] = *vultr.DDoSProtection
	}
	if vultr.ActivationEmail != nil {
		body["activation_email"] = *vultr.ActivationEmail
	}
	if vultr.EnableVPC != nil {
		body["enable_vpc"] = *vultr.EnableVPC
	}
	if len(vultr.VPCIDs) > 0 {
		body["attach_vpc"] = vultr.VPCIDs
	}
	if vultr.VPCOnly != nil {
		body["vpc_only"] = *vultr.VPCOnly
	}
	if vultr.DisablePublicIPv4 != nil {
		body["disable_public_ipv4"] = *vultr.DisablePublicIPv4
	}
	if vultr.ReservedIPv4 != "" {
		body["reserved_ipv4"] = vultr.ReservedIPv4
	}
	if vultr.UserScheme != "" {
		body["user_scheme"] = vultr.UserScheme
	}
	if vultr.ScriptID != "" {
		body["script_id"] = vultr.ScriptID
	}
	if len(vultr.AppVariables) > 0 {
		body["app_variables"] = vultr.AppVariables
	}
	if plan.UserData != "" {
		body["user_data"] = base64.StdEncoding.EncodeToString([]byte(plan.UserData))
	}
	addPlanSource(body, plan.Image, vultr)
	if c.DryRun {
		return Instance{Label: plan.Name, Tags: tags}, nil
	}
	var out createInstanceResponse
	if err := c.request(ctx, http.MethodPost, "/instances", body, &out); err != nil {
		return Instance{}, err
	}
	return out.Instance, nil
}

func vultrBackupMode(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func (c Client) DeleteInstance(ctx context.Context, id string) error {
	if !c.TokenPresent() {
		return fmt.Errorf("VULTR_API_KEY is required")
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("instance id is required")
	}
	if c.DryRun {
		return nil
	}
	return c.request(ctx, http.MethodDelete, "/instances/"+url.PathEscape(id), nil, nil)
}

func (c Client) EnsureFirewall(ctx context.Context, project, environment string, cfg config.VultrConfig) (FirewallGroup, error) {
	description := cfg.Firewall.Description
	if strings.TrimSpace(description) == "" {
		description = resourceName(project, environment, "firewall")
	}
	existing, err := c.ListFirewalls(ctx)
	if err != nil {
		return FirewallGroup{}, err
	}
	for _, firewall := range existing {
		if firewall.Description == description {
			if err := c.EnsureFirewallRules(ctx, firewall.ID, cfg); err != nil {
				return FirewallGroup{}, err
			}
			return firewall, nil
		}
	}
	var out createFirewallResponse
	if err := c.request(ctx, http.MethodPost, "/firewalls", map[string]string{"description": description}, &out); err != nil {
		return FirewallGroup{}, err
	}
	if err := c.EnsureFirewallRules(ctx, out.FirewallGroup.ID, cfg); err != nil {
		return FirewallGroup{}, err
	}
	return out.FirewallGroup, nil
}

func (c Client) ListFirewalls(ctx context.Context) ([]FirewallGroup, error) {
	if !c.TokenPresent() {
		return nil, fmt.Errorf("VULTR_API_KEY is required")
	}
	var firewalls []FirewallGroup
	cursor := ""
	for {
		values := url.Values{}
		values.Set("per_page", "500")
		if cursor != "" {
			values.Set("cursor", cursor)
		}
		var out listFirewallsResponse
		if err := c.request(ctx, http.MethodGet, "/firewalls?"+values.Encode(), nil, &out); err != nil {
			return nil, err
		}
		firewalls = append(firewalls, out.FirewallGroups...)
		if out.Meta.Links.Next == "" {
			break
		}
		cursor = nextCursor(out.Meta.Links.Next)
	}
	return firewalls, nil
}

func (c Client) EnsureFirewallRules(ctx context.Context, firewallID string, cfg config.VultrConfig) error {
	existing, err := c.ListFirewallRules(ctx, firewallID)
	if err != nil {
		return err
	}
	for _, rule := range firewallRules(cfg) {
		if hasFirewallRule(existing, rule) {
			continue
		}
		if err := c.CreateFirewallRule(ctx, firewallID, rule); err != nil {
			return err
		}
	}
	return nil
}

func (c Client) ListFirewallRules(ctx context.Context, firewallID string) ([]FirewallRule, error) {
	if !c.TokenPresent() {
		return nil, fmt.Errorf("VULTR_API_KEY is required")
	}
	if strings.TrimSpace(firewallID) == "" {
		return nil, fmt.Errorf("firewall id is required")
	}
	var rules []FirewallRule
	cursor := ""
	for {
		values := url.Values{}
		values.Set("per_page", "500")
		if cursor != "" {
			values.Set("cursor", cursor)
		}
		var out listFirewallRulesResponse
		if err := c.request(ctx, http.MethodGet, "/firewalls/"+url.PathEscape(firewallID)+"/rules?"+values.Encode(), nil, &out); err != nil {
			return nil, err
		}
		rules = append(rules, out.FirewallRules...)
		if out.Meta.Links.Next == "" {
			break
		}
		cursor = nextCursor(out.Meta.Links.Next)
	}
	return rules, nil
}

func (c Client) CreateFirewallRule(ctx context.Context, firewallID string, rule map[string]any) error {
	if !c.TokenPresent() {
		return fmt.Errorf("VULTR_API_KEY is required")
	}
	if strings.TrimSpace(firewallID) == "" {
		return fmt.Errorf("firewall id is required")
	}
	if c.DryRun {
		return nil
	}
	return c.request(ctx, http.MethodPost, "/firewalls/"+url.PathEscape(firewallID)+"/rules", rule, nil)
}

func (c Client) request(ctx context.Context, method, path string, body any, out any) error {
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
	req.Header.Set("Authorization", "Bearer "+c.Token)
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
		return fmt.Errorf("vultr %s %s failed: %s", method, path, strings.TrimSpace(string(data)))
	}
	if out == nil || len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c Client) apiBase() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return defaultAPIBase
}

func nextCursor(next string) string {
	parsed, err := url.Parse(next)
	if err != nil || parsed.Query().Get("cursor") == "" {
		return next
	}
	return parsed.Query().Get("cursor")
}

func sourceDescription(vultr config.VultrConfig) string {
	switch {
	case vultr.OSID != 0:
		return "os_id:" + strconv.Itoa(vultr.OSID)
	case vultr.ImageID != "":
		return "image_id:" + vultr.ImageID
	case vultr.SnapshotID != "":
		return "snapshot_id:" + vultr.SnapshotID
	case vultr.AppID != 0:
		return "app_id:" + strconv.Itoa(vultr.AppID)
	default:
		return ""
	}
}

func addSource(body map[string]any, vultr config.VultrConfig) {
	switch {
	case vultr.OSID != 0:
		body["os_id"] = vultr.OSID
	case vultr.ImageID != "":
		body["image_id"] = vultr.ImageID
	case vultr.SnapshotID != "":
		body["snapshot_id"] = vultr.SnapshotID
	case vultr.AppID != 0:
		body["app_id"] = vultr.AppID
	}
}

func addPlanSource(body map[string]any, source string, fallback config.VultrConfig) {
	if key, value, ok := strings.Cut(source, ":"); ok {
		switch key {
		case "os_id", "app_id":
			id, err := strconv.Atoi(value)
			if err == nil {
				body[key] = id
				return
			}
		case "image_id", "snapshot_id":
			if value != "" {
				body[key] = value
				return
			}
		}
	}
	addSource(body, fallback)
}

func firewallRules(cfg config.VultrConfig) []map[string]any {
	rules := []map[string]any{}
	if cfg.EffectiveSSHFirewall() == config.SSHFirewallManaged {
		rules = append(rules, firewallCIDRRules("ship-ssh", "tcp", "22", cfg.SSHAllowedCIDRs)...)
	}
	rules = append(rules, firewallCIDRRules("ship-http", "tcp", "80", []string{"0.0.0.0/0", "::/0"})...)
	rules = append(rules, firewallCIDRRules("ship-https", "tcp", "443", []string{"0.0.0.0/0", "::/0"})...)
	rules = append(rules, firewallCIDRRules("ship-http3", "udp", "443", []string{"0.0.0.0/0", "::/0"})...)
	return rules
}

func firewallCIDRRules(notes, protocol, port string, cidrs []string) []map[string]any {
	rules := []map[string]any{}
	for _, cidr := range cidrs {
		subnet, size, ipType, ok := splitFirewallCIDR(cidr)
		if !ok {
			continue
		}
		rules = append(rules, map[string]any{
			"ip_type":     ipType,
			"protocol":    protocol,
			"port":        port,
			"subnet":      subnet,
			"subnet_size": size,
			"notes":       notes,
		})
	}
	return rules
}

func splitFirewallCIDR(value string) (string, int, string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", 0, "", false
	}
	if !strings.Contains(value, "/") {
		ip := net.ParseIP(value)
		if ip == nil {
			return "", 0, "", false
		}
		if ip.To4() != nil {
			return ip.String(), 32, "v4", true
		}
		return ip.String(), 128, "v6", true
	}
	ip, network, err := net.ParseCIDR(value)
	if err != nil {
		return "", 0, "", false
	}
	ones, _ := network.Mask.Size()
	if ip.To4() != nil {
		return ip.String(), ones, "v4", true
	}
	return ip.String(), ones, "v6", true
}

func hasFirewallRule(existing []FirewallRule, want map[string]any) bool {
	for _, rule := range existing {
		if rule.IPType == want["ip_type"] &&
			rule.Protocol == want["protocol"] &&
			rule.Port == want["port"] &&
			rule.Subnet == want["subnet"] &&
			rule.SubnetSize == want["subnet_size"] &&
			rule.Notes == want["notes"] {
			return true
		}
	}
	return false
}

func instanceMatches(instance Instance, project, environment string) bool {
	labels := labelsFromTags(instance.Tags)
	return labels[LabelManagedBy] == "ship" &&
		labels[LabelProject] == project &&
		labels[LabelEnvironment] == environment
}

func tagsForPlan(plan provider.HostPlan) []string {
	labels := plan.Labels
	if len(labels) == 0 {
		labels = provider.ShipLabels(plan.Project, plan.Environment, plan.Pool)
	}
	return tagsFromLabels(labels)
}

func tagsFromLabels(labels map[string]string) []string {
	tags := make([]string, 0, len(labels))
	for key, value := range labels {
		if value == "" {
			continue
		}
		tags = append(tags, tagPrefix+key+"="+value)
	}
	sort.Strings(tags)
	return tags
}

func labelsFromTags(tags []string) map[string]string {
	labels := map[string]string{}
	for _, tag := range tags {
		trimmed := strings.TrimPrefix(tag, tagPrefix)
		if trimmed == tag {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok || key == "" {
			continue
		}
		labels[key] = value
	}
	return labels
}

func hostFromInstance(instance Instance) provider.Host {
	labels := labelsFromTags(instance.Tags)
	return provider.Host{
		ID:            instance.ID,
		Name:          instance.Label,
		Pool:          labels[LabelPool],
		PublicAddress: instance.MainIP,
		Labels:        labels,
	}
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

type listInstancesResponse struct {
	Instances []Instance `json:"instances"`
	Meta      struct {
		Links struct {
			Next string `json:"next"`
		} `json:"links"`
	} `json:"meta"`
}

type createInstanceResponse struct {
	Instance Instance `json:"instance"`
	JobIDs   []string `json:"job_ids"`
}

type listFirewallsResponse struct {
	FirewallGroups []FirewallGroup `json:"firewall_groups"`
	Meta           struct {
		Links struct {
			Next string `json:"next"`
		} `json:"links"`
	} `json:"meta"`
}

type createFirewallResponse struct {
	FirewallGroup FirewallGroup `json:"firewall_group"`
}

type listFirewallRulesResponse struct {
	FirewallRules []FirewallRule `json:"firewall_rules"`
	Meta          struct {
		Links struct {
			Next string `json:"next"`
		} `json:"links"`
	} `json:"meta"`
}
