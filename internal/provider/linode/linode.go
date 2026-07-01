package linode

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
	"strconv"
	"strings"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

const (
	defaultAPIBase = "https://api.linode.com/v4"

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
	ID     int64    `json:"id"`
	Label  string   `json:"label"`
	IPv4   []string `json:"ipv4"`
	Tags   []string `json:"tags"`
	Region string   `json:"region"`
	Type   string   `json:"type"`
	Image  string   `json:"image"`
}

type Firewall struct {
	ID    int64  `json:"id"`
	Label string `json:"label"`
}

type ReconcileResult = provider.ReconcileResult

func NewFromEnv(dryRun bool) Client {
	return Client{Token: os.Getenv("LINODE_TOKEN"), DryRun: dryRun, HTTP: http.DefaultClient}
}

func (c Client) Name() string {
	return config.ProviderLinode
}

func (c Client) TokenPresent() bool {
	return strings.TrimSpace(c.Token) != ""
}

func (c Client) CredentialChecks(lookupEnv func(string) (string, bool)) []provider.CredentialCheck {
	_, ok := lookupEnv("LINODE_TOKEN")
	return []provider.CredentialCheck{{
		Name:           "linode token",
		Present:        ok,
		Required:       true,
		PresentMessage: "LINODE_TOKEN is set",
		MissingMessage: "missing LINODE_TOKEN",
	}}
}

func DesiredInstances(env config.Environment) []provider.HostPlan {
	return DesiredInstancesFor("", "", env)
}

func DesiredInstancesFor(project, environment string, env config.Environment) []provider.HostPlan {
	linode := env.Provider.Linode
	if linode == nil {
		return nil
	}
	return provider.HostPlans(project, environment, env, provider.HostPlanOptions{
		Location: linode.Region,
		Size:     linode.Type,
		Image:    linode.Image,
		UserData: linode.UserData,
	})
}

func (c Client) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.Linode == nil {
		return nil, fmt.Errorf("environment %q must define provider.linode", environment)
	}
	return DesiredInstancesFor(project, environment, env), nil
}

func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	if env.Provider.Linode == nil {
		return provider.ReconcileResult{}, fmt.Errorf("environment %q must define provider.linode", environment)
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

	linode := *env.Provider.Linode
	var firewallID int64
	if linode.Firewall.EnabledValue(true) {
		firewall, err := c.EnsureFirewall(ctx, project, environment, linode)
		if err != nil {
			return provider.ReconcileResult{}, err
		}
		firewallID = firewall.ID
	}

	return provider.ReconcileHosts(ctx, project, environment, desired, reconcileBackend{
		client:     c,
		linode:     linode,
		firewallID: firewallID,
	})
}

type reconcileBackend struct {
	client     Client
	linode     config.LinodeConfig
	firewallID int64
}

func (b reconcileBackend) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	return b.client.List(ctx, project, environment)
}

func (b reconcileBackend) Create(ctx context.Context, plan provider.HostPlan) (provider.Host, error) {
	instance, err := b.client.CreateInstance(ctx, plan, b.linode, b.firewallID)
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
		return fmt.Errorf("linode id is required")
	}
	id, err := strconv.ParseInt(host.ID, 10, 64)
	if err != nil {
		return fmt.Errorf("linode id must be numeric: %w", err)
	}
	return c.DeleteInstance(ctx, id)
}

func (c Client) ListInstances(ctx context.Context, project, environment string) ([]Instance, error) {
	if !c.TokenPresent() {
		return nil, fmt.Errorf("LINODE_TOKEN is required")
	}
	page := 1
	var instances []Instance
	for {
		values := url.Values{}
		values.Set("page", strconv.Itoa(page))
		values.Set("page_size", "500")
		var out listInstancesResponse
		if err := c.request(ctx, http.MethodGet, "/linode/instances?"+values.Encode(), nil, &out); err != nil {
			return nil, err
		}
		for _, instance := range out.Data {
			if instanceMatches(instance, project, environment) {
				instances = append(instances, instance)
			}
		}
		if out.Pages == 0 || out.Page >= out.Pages {
			break
		}
		page = out.Page + 1
	}
	return instances, nil
}

func (c Client) CreateInstance(ctx context.Context, plan provider.HostPlan, linode config.LinodeConfig, firewallID int64) (Instance, error) {
	if !c.TokenPresent() {
		return Instance{}, fmt.Errorf("LINODE_TOKEN is required")
	}
	body := map[string]any{
		"label":  plan.Name,
		"region": plan.Location,
		"type":   plan.Size,
		"image":  plan.Image,
		"tags":   tagsForPlan(plan),
	}
	if len(linode.AuthorizedKeys) > 0 {
		body["authorized_keys"] = linode.AuthorizedKeys
	}
	if len(linode.AuthorizedUsers) > 0 {
		body["authorized_users"] = linode.AuthorizedUsers
	}
	if linode.PrivateIP != nil {
		body["private_ip"] = *linode.PrivateIP
	}
	if linode.Backups != nil {
		body["backups_enabled"] = *linode.Backups
	}
	if firewallID != 0 {
		body["firewall_id"] = firewallID
	}
	if plan.UserData != "" {
		body["metadata"] = map[string]any{
			"user_data": base64.StdEncoding.EncodeToString([]byte(plan.UserData)),
		}
	}
	if c.DryRun {
		return Instance{Label: plan.Name, Tags: tagsForPlan(plan)}, nil
	}
	var out Instance
	if err := c.request(ctx, http.MethodPost, "/linode/instances", body, &out); err != nil {
		return Instance{}, err
	}
	return out, nil
}

func (c Client) DeleteInstance(ctx context.Context, id int64) error {
	if !c.TokenPresent() {
		return fmt.Errorf("LINODE_TOKEN is required")
	}
	if id == 0 {
		return fmt.Errorf("linode id is required")
	}
	if c.DryRun {
		return nil
	}
	return c.request(ctx, http.MethodDelete, "/linode/instances/"+strconv.FormatInt(id, 10), nil, nil)
}

func (c Client) EnsureFirewall(ctx context.Context, project, environment string, cfg config.LinodeConfig) (Firewall, error) {
	label := cfg.Firewall.Label
	if strings.TrimSpace(label) == "" {
		label = resourceName(project, environment, "firewall")
	}
	existing, err := c.ListFirewalls(ctx)
	if err != nil {
		return Firewall{}, err
	}
	for _, firewall := range existing {
		if firewall.Label == label {
			return firewall, nil
		}
	}
	body := map[string]any{
		"label": label,
		"rules": firewallRules(cfg),
	}
	var out Firewall
	if err := c.request(ctx, http.MethodPost, "/networking/firewalls", body, &out); err != nil {
		return Firewall{}, err
	}
	return out, nil
}

func (c Client) ListFirewalls(ctx context.Context) ([]Firewall, error) {
	if !c.TokenPresent() {
		return nil, fmt.Errorf("LINODE_TOKEN is required")
	}
	page := 1
	var firewalls []Firewall
	for {
		values := url.Values{}
		values.Set("page", strconv.Itoa(page))
		values.Set("page_size", "500")
		var out listFirewallsResponse
		if err := c.request(ctx, http.MethodGet, "/networking/firewalls?"+values.Encode(), nil, &out); err != nil {
			return nil, err
		}
		firewalls = append(firewalls, out.Data...)
		if out.Pages == 0 || out.Page >= out.Pages {
			break
		}
		page = out.Page + 1
	}
	return firewalls, nil
}

func firewallRules(cfg config.LinodeConfig) map[string]any {
	inbound := []map[string]any{}
	if cfg.EffectiveSSHFirewall() == config.SSHFirewallManaged {
		inbound = append(inbound, firewallRule("ship-ssh", "tcp", "22", cfg.SSHAllowedCIDRs))
	}
	inbound = append(inbound,
		firewallRule("ship-http", "tcp", "80", []string{"0.0.0.0/0", "::/0"}),
		firewallRule("ship-https", "tcp", "443", []string{"0.0.0.0/0", "::/0"}),
		firewallRule("ship-http3", "udp", "443", []string{"0.0.0.0/0", "::/0"}),
	)
	return map[string]any{
		"inbound_policy":  "DROP",
		"outbound_policy": "ACCEPT",
		"inbound":         inbound,
		"outbound":        []map[string]any{},
	}
}

func firewallRule(label, protocol, ports string, cidrs []string) map[string]any {
	ipv4 := []string{}
	ipv6 := []string{}
	for _, cidr := range cidrs {
		if strings.Contains(cidr, ":") {
			ipv6 = append(ipv6, cidr)
			continue
		}
		ipv4 = append(ipv4, cidr)
	}
	return map[string]any{
		"label":    label,
		"action":   "ACCEPT",
		"protocol": protocol,
		"ports":    ports,
		"addresses": map[string]any{
			"ipv4": ipv4,
			"ipv6": ipv6,
		},
	}
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
		return fmt.Errorf("linode %s %s failed: %s", method, path, strings.TrimSpace(string(data)))
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
		ID:            strconv.FormatInt(instance.ID, 10),
		Name:          instance.Label,
		Pool:          labels[LabelPool],
		PublicAddress: publicIPv4(instance),
		Labels:        labels,
	}
}

func publicIPv4(instance Instance) string {
	for _, ip := range instance.IPv4 {
		if strings.Contains(ip, ":") {
			continue
		}
		return ip
	}
	return ""
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
	Data  []Instance `json:"data"`
	Page  int        `json:"page"`
	Pages int        `json:"pages"`
}

type listFirewallsResponse struct {
	Data  []Firewall `json:"data"`
	Page  int        `json:"page"`
	Pages int        `json:"pages"`
}
