package civo

import (
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
	defaultAPIBase = "https://api.civo.com/v2"

	LabelManagedBy   = provider.LabelManagedBy
	LabelProject     = provider.LabelProject
	LabelEnvironment = provider.LabelEnvironment
	LabelPool        = provider.LabelPool

	tagPrefix = "ship:"
)

type Client struct {
	Token   string
	Region  string
	DryRun  bool
	HTTP    *http.Client
	BaseURL string
}

type InstancePlan = provider.HostPlan

type Instance struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Hostname  string   `json:"hostname"`
	Size      string   `json:"size"`
	NetworkID string   `json:"network_id"`
	PublicIP  string   `json:"public_ip"`
	PrivateIP string   `json:"private_ip"`
	IPv6      string   `json:"ipv6"`
	Status    string   `json:"status"`
	Tags      []string `json:"tags"`
}

type Firewall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	NetworkID string `json:"network_id"`
}

type FirewallRule struct {
	ID        string   `json:"id"`
	Protocol  string   `json:"protocol"`
	StartPort string   `json:"start_port"`
	EndPort   string   `json:"end_port"`
	CIDR      []string `json:"cidr"`
	Direction string   `json:"direction"`
	Label     string   `json:"label"`
}

type ReconcileResult = provider.ReconcileResult

type listInstancesResponse struct {
	Page  int        `json:"page"`
	Pages int        `json:"pages"`
	Items []Instance `json:"items"`
}

func NewFromEnv(dryRun bool, configs ...config.CivoConfig) Client {
	cfg := config.CivoConfig{}
	if len(configs) > 0 {
		cfg = configs[0]
	}
	return Client{Token: os.Getenv("CIVO_TOKEN"), Region: cfg.Region, DryRun: dryRun, HTTP: http.DefaultClient}
}

func (c Client) Name() string {
	return config.ProviderCivo
}

func (c Client) TokenPresent() bool {
	return strings.TrimSpace(c.Token) != ""
}

func (c Client) CredentialChecks(lookupEnv func(string) (string, bool)) []provider.CredentialCheck {
	_, ok := lookupEnv("CIVO_TOKEN")
	return []provider.CredentialCheck{{
		Name:           "civo token",
		Present:        ok,
		Required:       true,
		PresentMessage: "CIVO_TOKEN is set",
		MissingMessage: "missing CIVO_TOKEN",
	}}
}

func DesiredInstances(env config.Environment) []provider.HostPlan {
	return DesiredInstancesFor("", "", env)
}

func DesiredInstancesFor(project, environment string, env config.Environment) []provider.HostPlan {
	civo := env.Provider.Civo
	if civo == nil {
		return nil
	}
	return provider.HostPlans(project, environment, env, provider.HostPlanOptions{
		Location: civo.Region,
		Size:     civo.Size,
		Image:    civo.Image,
		UserData: civo.UserData,
	})
}

func (c Client) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.Civo == nil {
		return nil, fmt.Errorf("environment %q must define provider.civo", environment)
	}
	return DesiredInstancesFor(project, environment, env), nil
}

func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	if env.Provider.Civo == nil {
		return provider.ReconcileResult{}, fmt.Errorf("environment %q must define provider.civo", environment)
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

	backend, err := c.reconcileBackendFor(ctx, project, environment, env)
	if err != nil {
		return provider.ReconcileResult{}, err
	}
	return provider.ReconcileHosts(ctx, project, environment, desired, backend)
}

// reconcileBackendFor resolves the Civo firewall and builds the reconcile
// backend shared by Reconcile and CreateHost so both create instances
// identically.
func (c Client) reconcileBackendFor(ctx context.Context, project, environment string, env config.Environment) (reconcileBackend, error) {
	civo := *env.Provider.Civo
	firewallID := civo.Firewall.ID
	if civo.Firewall.ManagedValue(true) {
		firewall, err := c.EnsureFirewall(ctx, project, environment, civo)
		if err != nil {
			return reconcileBackend{}, err
		}
		firewallID = firewall.ID
	}
	return reconcileBackend{
		client:     c,
		civo:       civo,
		firewallID: firewallID,
	}, nil
}

// CreateHost provisions a single instance using the backend Reconcile would
// build, so `ship migrate` can add a replacement alongside the existing one.
func (c Client) CreateHost(ctx context.Context, project, environment string, env config.Environment, plan provider.HostPlan) (provider.Host, error) {
	if env.Provider.Civo == nil {
		return provider.Host{}, fmt.Errorf("environment %q must define provider.civo", environment)
	}
	backend, err := c.reconcileBackendFor(ctx, project, environment, env)
	if err != nil {
		return provider.Host{}, err
	}
	return backend.Create(ctx, plan)
}

var _ provider.HostCreator = Client{}

type reconcileBackend struct {
	client     Client
	civo       config.CivoConfig
	firewallID string
}

func (b reconcileBackend) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	return b.client.List(ctx, project, environment)
}

func (b reconcileBackend) Create(ctx context.Context, plan provider.HostPlan) (provider.Host, error) {
	instance, err := b.client.CreateInstance(ctx, plan, b.civo, b.firewallID)
	if err != nil {
		return provider.Host{}, err
	}
	return hostFromInstance(instance), nil
}

func (c Client) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	instances, err := c.ListInstances(ctx, project, environment, c.Region)
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
		return fmt.Errorf("civo instance id is required")
	}
	region := ""
	if host.Labels != nil {
		region = host.Labels["region"]
	}
	if region == "" {
		region = c.Region
	}
	return c.DeleteInstance(ctx, host.ID, region)
}

func (c Client) ListInstances(ctx context.Context, project, environment, region string) ([]Instance, error) {
	if !c.TokenPresent() {
		return nil, fmt.Errorf("CIVO_TOKEN is required")
	}
	page := 1
	var filtered []Instance
	for {
		values := url.Values{}
		if region != "" {
			values.Set("region", region)
		}
		values.Set("page", strconv.Itoa(page))
		values.Set("per_page", "100")
		var out listInstancesResponse
		if err := c.request(ctx, http.MethodGet, "/instances?"+values.Encode(), nil, &out); err != nil {
			return nil, err
		}
		for _, instance := range out.Items {
			if instanceMatches(instance, project, environment) {
				filtered = append(filtered, instance)
			}
		}
		if out.Pages == 0 || out.Page >= out.Pages {
			break
		}
		page = out.Page + 1
	}
	return filtered, nil
}

func (c Client) CreateInstance(ctx context.Context, plan provider.HostPlan, civo config.CivoConfig, firewallID string) (Instance, error) {
	if !c.TokenPresent() {
		return Instance{}, fmt.Errorf("CIVO_TOKEN is required")
	}
	values := url.Values{
		"hostname":    []string{firstNonEmpty(civo.Hostname, plan.Name)},
		"size":        []string{plan.Size},
		"template_id": []string{plan.Image},
		"network_id":  []string{civo.NetworkID},
		"region":      []string{plan.Location},
		"tags":        []string{strings.Join(tagsForPlan(plan), " ")},
	}
	if civo.PublicIP != nil {
		if *civo.PublicIP {
			values.Set("public_ip", "create")
		} else {
			values.Set("public_ip", "none")
		}
	}
	if firewallID != "" {
		values.Set("firewall_id", firewallID)
	}
	if civo.ReverseDNS != "" {
		values.Set("reverse_dns", civo.ReverseDNS)
	}
	if civo.PrivateIPv4 != "" {
		values.Set("private_ipv4", civo.PrivateIPv4)
	}
	if civo.SSHKeyID != "" {
		values.Set("ssh_key_id", civo.SSHKeyID)
	}
	if civo.InitialUser != "" {
		values.Set("initial_user", civo.InitialUser)
	}
	if plan.UserData != "" {
		values.Set("script", plan.UserData)
	}
	for _, ip := range civo.AllowedIPs {
		values.Add("allowed_ips", ip)
	}
	if civo.NetworkBandwidthLimit > 0 {
		values.Set("network_bandwidth_limit", strconv.Itoa(civo.NetworkBandwidthLimit))
	}
	if c.DryRun {
		return Instance{ID: plan.Name, Hostname: plan.Name, Tags: tagsForPlan(plan)}, nil
	}
	var out Instance
	if err := c.request(ctx, http.MethodPost, "/instances", values, &out); err != nil {
		return Instance{}, err
	}
	return out, nil
}

func (c Client) DeleteInstance(ctx context.Context, id, region string) error {
	if !c.TokenPresent() {
		return fmt.Errorf("CIVO_TOKEN is required")
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("civo instance id is required")
	}
	values := url.Values{}
	if region != "" {
		values.Set("region", region)
	}
	if c.DryRun {
		return nil
	}
	return c.request(ctx, http.MethodDelete, "/instances/"+url.PathEscape(id)+"?"+values.Encode(), nil, nil)
}

func (c Client) EnsureFirewall(ctx context.Context, project, environment string, civo config.CivoConfig) (Firewall, error) {
	if !c.TokenPresent() {
		return Firewall{}, fmt.Errorf("CIVO_TOKEN is required")
	}
	name := civo.Firewall.Name
	if name == "" {
		name = resourceName(project, environment, "firewall")
	}
	firewall, ok, err := c.FindFirewall(ctx, name, civo.Region)
	if err != nil {
		return Firewall{}, err
	}
	if !ok {
		firewall, err = c.CreateFirewall(ctx, name, civo)
		if err != nil {
			return Firewall{}, err
		}
	}
	if err := c.EnsureFirewallRules(ctx, firewall.ID, civo); err != nil {
		return Firewall{}, err
	}
	return firewall, nil
}

func (c Client) FindFirewall(ctx context.Context, name, region string) (Firewall, bool, error) {
	firewalls, err := c.ListFirewalls(ctx, region)
	if err != nil {
		return Firewall{}, false, err
	}
	for _, firewall := range firewalls {
		if firewall.Name == name {
			return firewall, true, nil
		}
	}
	return Firewall{}, false, nil
}

func (c Client) ListFirewalls(ctx context.Context, region string) ([]Firewall, error) {
	values := url.Values{}
	if region != "" {
		values.Set("region", region)
	}
	var out []Firewall
	if err := c.request(ctx, http.MethodGet, "/firewalls?"+values.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c Client) CreateFirewall(ctx context.Context, name string, civo config.CivoConfig) (Firewall, error) {
	values := url.Values{
		"name":       []string{name},
		"network_id": []string{civo.NetworkID},
		"region":     []string{civo.Region},
	}
	var out Firewall
	if err := c.request(ctx, http.MethodPost, "/firewalls", values, &out); err != nil {
		return Firewall{}, err
	}
	return out, nil
}

func (c Client) EnsureFirewallRules(ctx context.Context, firewallID string, civo config.CivoConfig) error {
	existing, err := c.ListFirewallRules(ctx, firewallID, civo.Region)
	if err != nil {
		return err
	}
	for _, rule := range firewallRules(civo) {
		if containsFirewallRule(existing, rule) {
			continue
		}
		if _, err := c.CreateFirewallRule(ctx, firewallID, civo.Region, rule); err != nil {
			return err
		}
	}
	return nil
}

func (c Client) ListFirewallRules(ctx context.Context, firewallID, region string) ([]FirewallRule, error) {
	values := url.Values{}
	if region != "" {
		values.Set("region", region)
	}
	var out []FirewallRule
	if err := c.request(ctx, http.MethodGet, "/firewalls/"+url.PathEscape(firewallID)+"/rules?"+values.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c Client) CreateFirewallRule(ctx context.Context, firewallID, region string, rule FirewallRule) (FirewallRule, error) {
	values := url.Values{
		"region":     []string{region},
		"protocol":   []string{rule.Protocol},
		"start_port": []string{rule.StartPort},
		"direction":  []string{rule.Direction},
		"label":      []string{rule.Label},
	}
	if rule.EndPort != "" {
		values.Set("end_port", rule.EndPort)
	}
	for _, cidr := range rule.CIDR {
		values.Add("cidr", cidr)
	}
	var out FirewallRule
	if err := c.request(ctx, http.MethodPost, "/firewalls/"+url.PathEscape(firewallID)+"/rules", values, &out); err != nil {
		return FirewallRule{}, err
	}
	return out, nil
}

func (c Client) request(ctx context.Context, method, path string, values url.Values, out any) error {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = defaultAPIBase
	}
	var body io.Reader
	if values != nil && method != http.MethodGet && method != http.MethodDelete {
		body = strings.NewReader(values.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("civo api %s %s failed: %s: %s", method, path, res.Status, strings.TrimSpace(string(data)))
	}
	if out != nil && res.StatusCode != http.StatusNoContent && res.ContentLength != 0 {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil && err != io.EOF {
			return err
		}
	}
	return nil
}

func firewallRules(civo config.CivoConfig) []FirewallRule {
	var rules []FirewallRule
	if civo.EffectiveSSHFirewall() == config.SSHFirewallManaged {
		for _, cidr := range civo.SSHAllowedCIDRs {
			rules = append(rules, firewallRule("ship-ssh", "tcp", cidr, 22))
		}
	}
	for _, cidr := range []string{"0.0.0.0/0", "::/0"} {
		rules = append(rules, firewallRule("ship-http", "tcp", cidr, 80))
		rules = append(rules, firewallRule("ship-https", "tcp", cidr, 443))
		rules = append(rules, firewallRule("ship-http3", "udp", cidr, 443))
	}
	return rules
}

func firewallRule(label, protocol, cidr string, port int) FirewallRule {
	return FirewallRule{
		Protocol:  protocol,
		StartPort: strconv.Itoa(port),
		CIDR:      []string{cidr},
		Direction: "ingress",
		Label:     label,
	}
}

func containsFirewallRule(existing []FirewallRule, want FirewallRule) bool {
	for _, got := range existing {
		if strings.EqualFold(got.Protocol, want.Protocol) &&
			got.StartPort == want.StartPort &&
			firstNonEmpty(got.EndPort, got.StartPort) == firstNonEmpty(want.EndPort, want.StartPort) &&
			strings.EqualFold(firstNonEmpty(got.Direction, "ingress"), firstNonEmpty(want.Direction, "ingress")) &&
			containsAnyCIDR(got.CIDR, want.CIDR) {
			return true
		}
	}
	return false
}

func containsAnyCIDR(got, want []string) bool {
	if len(want) == 0 {
		want = []string{"0.0.0.0/0"}
	}
	gotSet := map[string]bool{}
	for _, cidr := range got {
		gotSet[cidr] = true
	}
	if len(gotSet) == 0 {
		gotSet["0.0.0.0/0"] = true
	}
	for _, cidr := range want {
		if !gotSet[cidr] {
			return false
		}
	}
	return true
}

func hostFromInstance(instance Instance) provider.Host {
	labels := labelsFromTags(instance.Tags)
	if instance.NetworkID != "" {
		labels["network_id"] = instance.NetworkID
	}
	return provider.Host{
		ID:            instance.ID,
		Name:          firstNonEmpty(instance.Hostname, instance.Name),
		Pool:          labels[LabelPool],
		PublicAddress: firstNonEmpty(instance.PublicIP, instance.IPv6, instance.PrivateIP),
		Labels:        labels,
	}
}

func instanceMatches(instance Instance, project, environment string) bool {
	labels := labelsFromTags(instance.Tags)
	return labels[LabelManagedBy] == "ship" &&
		labels[LabelProject] == project &&
		labels[LabelEnvironment] == environment
}

func tagsForPlan(plan provider.HostPlan) []string {
	labels := plan.Labels
	if labels == nil {
		labels = provider.ShipLabels(plan.Project, plan.Environment, plan.Pool)
	}
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

func labelsFromTags(tags []string) map[string]string {
	labels := map[string]string{}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if !strings.HasPrefix(tag, tagPrefix) {
			continue
		}
		parts := strings.SplitN(strings.TrimPrefix(tag, tagPrefix), ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		labels[parts[0]] = parts[1]
	}
	return labels
}

func tagForLabel(key, value string) string {
	return tagPrefix + key + ":" + safeTag(value)
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
