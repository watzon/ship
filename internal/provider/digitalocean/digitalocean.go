package digitalocean

import (
	"bytes"
	"context"
	"encoding/base32"
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
	defaultAPIBase = "https://api.digitalocean.com/v2"

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

type DropletPlan = provider.HostPlan

type Droplet struct {
	ID       int64    `json:"id"`
	Name     string   `json:"name"`
	Tags     []string `json:"tags"`
	Networks Networks `json:"networks"`
}

type Networks struct {
	V4 []NetworkV4 `json:"v4"`
}

type NetworkV4 struct {
	IPAddress string `json:"ip_address"`
	Type      string `json:"type"`
}

type Firewall struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

type ReconcileResult = provider.ReconcileResult

func NewFromEnv(dryRun bool) Client {
	return Client{Token: os.Getenv("DIGITALOCEAN_TOKEN"), DryRun: dryRun, HTTP: http.DefaultClient}
}

func (c Client) Name() string {
	return config.ProviderDigitalOcean
}

func (c Client) TokenPresent() bool {
	return strings.TrimSpace(c.Token) != ""
}

func (c Client) CredentialChecks(lookupEnv func(string) (string, bool)) []provider.CredentialCheck {
	_, ok := lookupEnv("DIGITALOCEAN_TOKEN")
	return []provider.CredentialCheck{{
		Name:           "digitalocean token",
		Present:        ok,
		Required:       true,
		PresentMessage: "DIGITALOCEAN_TOKEN is set",
		MissingMessage: "missing DIGITALOCEAN_TOKEN",
	}}
}

func DesiredDroplets(env config.Environment) []provider.HostPlan {
	return DesiredDropletsFor("", "", env)
}

func DesiredDropletsFor(project, environment string, env config.Environment) []provider.HostPlan {
	digitalocean := env.Provider.DigitalOcean
	if digitalocean == nil {
		return nil
	}
	return provider.HostPlans(project, environment, env, provider.HostPlanOptions{
		Location: digitalocean.Region,
		Size:     digitalocean.Size,
		Image:    digitalocean.Image,
		UserData: digitalocean.UserData,
	})
}

func (c Client) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.DigitalOcean == nil {
		return nil, fmt.Errorf("environment %q must define provider.digitalocean", environment)
	}
	return DesiredDropletsFor(project, environment, env), nil
}

func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	if env.Provider.DigitalOcean == nil {
		return provider.ReconcileResult{}, fmt.Errorf("environment %q must define provider.digitalocean", environment)
	}
	if strings.TrimSpace(project) == "" {
		return provider.ReconcileResult{}, fmt.Errorf("project is required")
	}
	if strings.TrimSpace(environment) == "" {
		return provider.ReconcileResult{}, fmt.Errorf("environment is required")
	}

	desired := DesiredDropletsFor(project, environment, env)
	result := provider.ReconcileResult{Desired: desired}
	if c.DryRun {
		return result, nil
	}

	digitalocean := *env.Provider.DigitalOcean
	environmentTags := tagsFromLabels(provider.ShipLabels(project, environment, ""))
	for _, tag := range environmentTags {
		if err := c.EnsureTag(ctx, tag); err != nil {
			return provider.ReconcileResult{}, err
		}
	}
	if digitalocean.Firewall.EnabledValue(true) {
		if _, err := c.EnsureFirewall(ctx, project, environment, digitalocean); err != nil {
			return provider.ReconcileResult{}, err
		}
	}

	return provider.ReconcileHosts(ctx, project, environment, desired, reconcileBackend{client: c, digitalocean: digitalocean})
}

type reconcileBackend struct {
	client       Client
	digitalocean config.DigitalOceanConfig
}

func (b reconcileBackend) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	return b.client.List(ctx, project, environment)
}

func (b reconcileBackend) Create(ctx context.Context, plan provider.HostPlan) (provider.Host, error) {
	droplet, err := b.client.CreateDroplet(ctx, plan, b.digitalocean)
	if err != nil {
		return provider.Host{}, err
	}
	return hostFromDroplet(droplet), nil
}

func (c Client) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	droplets, err := c.ListDroplets(ctx, project, environment)
	if err != nil {
		return nil, err
	}
	hosts := make([]provider.Host, 0, len(droplets))
	for _, droplet := range droplets {
		hosts = append(hosts, hostFromDroplet(droplet))
	}
	return hosts, nil
}

func (c Client) Delete(ctx context.Context, host provider.Host) error {
	if strings.TrimSpace(host.ID) == "" {
		return fmt.Errorf("droplet id is required")
	}
	id, err := strconv.ParseInt(host.ID, 10, 64)
	if err != nil {
		return fmt.Errorf("droplet id must be numeric: %w", err)
	}
	return c.DeleteDroplet(ctx, id)
}

func (c Client) ListDroplets(ctx context.Context, project, environment string) ([]Droplet, error) {
	if !c.TokenPresent() {
		return nil, fmt.Errorf("DIGITALOCEAN_TOKEN is required")
	}
	envTag := tagForLabel(LabelEnvironment, environment)
	page := 1
	var droplets []Droplet
	for {
		values := url.Values{}
		values.Set("tag_name", envTag)
		values.Set("per_page", "200")
		values.Set("page", strconv.Itoa(page))
		var out listDropletsResponse
		if err := c.request(ctx, http.MethodGet, "/droplets?"+values.Encode(), nil, &out); err != nil {
			return nil, err
		}
		for _, droplet := range out.Droplets {
			if dropletMatches(droplet, project, environment) {
				droplets = append(droplets, droplet)
			}
		}
		if out.Links.Pages.Next == "" {
			break
		}
		page++
	}
	return droplets, nil
}

func (c Client) CreateDroplet(ctx context.Context, plan provider.HostPlan, digitalocean config.DigitalOceanConfig) (Droplet, error) {
	if !c.TokenPresent() {
		return Droplet{}, fmt.Errorf("DIGITALOCEAN_TOKEN is required")
	}
	tags := tagsForPlan(plan)
	for _, tag := range tags {
		if err := c.EnsureTag(ctx, tag); err != nil {
			return Droplet{}, err
		}
	}
	body := map[string]any{
		"name":   plan.Name,
		"region": plan.Location,
		"size":   plan.Size,
		"image":  plan.Image,
		"tags":   tags,
	}
	if plan.UserData != "" {
		body["user_data"] = plan.UserData
	}
	if len(digitalocean.SSHKeys) > 0 {
		body["ssh_keys"] = digitalocean.SSHKeys
	}
	if digitalocean.VPCUUID != "" {
		body["vpc_uuid"] = digitalocean.VPCUUID
	}
	if digitalocean.Monitoring != nil {
		body["monitoring"] = *digitalocean.Monitoring
	}
	if digitalocean.Backups != nil {
		body["backups"] = *digitalocean.Backups
	}
	if digitalocean.IPv6 != nil {
		body["ipv6"] = *digitalocean.IPv6
	}
	if c.DryRun {
		return Droplet{Name: plan.Name, Tags: tags}, nil
	}
	var out createDropletResponse
	if err := c.request(ctx, http.MethodPost, "/droplets", body, &out); err != nil {
		return Droplet{}, err
	}
	return out.Droplet, nil
}

func (c Client) DeleteDroplet(ctx context.Context, id int64) error {
	if !c.TokenPresent() {
		return fmt.Errorf("DIGITALOCEAN_TOKEN is required")
	}
	if id == 0 {
		return fmt.Errorf("droplet id is required")
	}
	if c.DryRun {
		return nil
	}
	return c.request(ctx, http.MethodDelete, "/droplets/"+strconv.FormatInt(id, 10), nil, nil)
}

func (c Client) EnsureTag(ctx context.Context, name string) error {
	if !c.TokenPresent() {
		return fmt.Errorf("DIGITALOCEAN_TOKEN is required")
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("tag name is required")
	}
	if c.DryRun {
		return nil
	}
	err := c.request(ctx, http.MethodPost, "/tags", map[string]string{"name": name}, nil)
	if err == nil || isAlreadyExists(err) {
		return nil
	}
	return err
}

func (c Client) EnsureFirewall(ctx context.Context, project, environment string, cfg config.DigitalOceanConfig) (Firewall, error) {
	name := cfg.Firewall.Name
	if strings.TrimSpace(name) == "" {
		name = resourceName(project, environment, "firewall")
	}
	existing, err := c.ListFirewalls(ctx)
	if err != nil {
		return Firewall{}, err
	}
	envTag := tagForLabel(LabelEnvironment, environment)
	if err := c.EnsureTag(ctx, envTag); err != nil {
		return Firewall{}, err
	}
	for _, firewall := range existing {
		if firewall.Name == name {
			if !contains(firewall.Tags, envTag) {
				if err := c.AddTagsToFirewall(ctx, firewall.ID, []string{envTag}); err != nil {
					return Firewall{}, err
				}
			}
			return firewall, nil
		}
	}
	body := map[string]any{
		"name":           name,
		"inbound_rules":  firewallInboundRules(cfg),
		"outbound_rules": firewallOutboundRules(),
		"tags":           []string{envTag},
	}
	var out createFirewallResponse
	if err := c.request(ctx, http.MethodPost, "/firewalls", body, &out); err != nil {
		return Firewall{}, err
	}
	return out.Firewall, nil
}

func (c Client) ListFirewalls(ctx context.Context) ([]Firewall, error) {
	if !c.TokenPresent() {
		return nil, fmt.Errorf("DIGITALOCEAN_TOKEN is required")
	}
	page := 1
	var firewalls []Firewall
	for {
		values := url.Values{}
		values.Set("per_page", "200")
		values.Set("page", strconv.Itoa(page))
		var out listFirewallsResponse
		if err := c.request(ctx, http.MethodGet, "/firewalls?"+values.Encode(), nil, &out); err != nil {
			return nil, err
		}
		firewalls = append(firewalls, out.Firewalls...)
		if out.Links.Pages.Next == "" {
			break
		}
		page++
	}
	return firewalls, nil
}

func (c Client) AddTagsToFirewall(ctx context.Context, firewallID string, tags []string) error {
	if !c.TokenPresent() {
		return fmt.Errorf("DIGITALOCEAN_TOKEN is required")
	}
	if strings.TrimSpace(firewallID) == "" {
		return fmt.Errorf("firewall id is required")
	}
	if len(tags) == 0 || c.DryRun {
		return nil
	}
	body := map[string]any{"tags": tags}
	return c.request(ctx, http.MethodPost, "/firewalls/"+url.PathEscape(firewallID)+"/tags", body, nil)
}

func firewallInboundRules(cfg config.DigitalOceanConfig) []map[string]any {
	rules := []map[string]any{}
	if cfg.EffectiveSSHFirewall() == config.SSHFirewallManaged {
		rules = append(rules, map[string]any{
			"protocol": "tcp",
			"ports":    "22",
			"sources":  map[string]any{"addresses": append([]string(nil), cfg.SSHAllowedCIDRs...)},
		})
	}
	rules = append(rules,
		map[string]any{"protocol": "tcp", "ports": "80", "sources": map[string]any{"addresses": []string{"0.0.0.0/0", "::/0"}}},
		map[string]any{"protocol": "tcp", "ports": "443", "sources": map[string]any{"addresses": []string{"0.0.0.0/0", "::/0"}}},
		map[string]any{"protocol": "udp", "ports": "443", "sources": map[string]any{"addresses": []string{"0.0.0.0/0", "::/0"}}},
	)
	return rules
}

func firewallOutboundRules() []map[string]any {
	addresses := []string{"0.0.0.0/0", "::/0"}
	return []map[string]any{
		{"protocol": "tcp", "ports": "0", "destinations": map[string]any{"addresses": addresses}},
		{"protocol": "udp", "ports": "0", "destinations": map[string]any{"addresses": addresses}},
		{"protocol": "icmp", "ports": "0", "destinations": map[string]any{"addresses": addresses}},
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
		return apiError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(data))}
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

type apiError struct {
	StatusCode int
	Body       string
}

func (e apiError) Error() string {
	return fmt.Sprintf("digitalocean request failed status=%d: %s", e.StatusCode, e.Body)
}

func isAlreadyExists(err error) bool {
	apiErr, ok := err.(apiError)
	if !ok {
		return false
	}
	body := strings.ToLower(apiErr.Body)
	return apiErr.StatusCode == http.StatusUnprocessableEntity && (strings.Contains(body, "already") || strings.Contains(body, "exists"))
}

func dropletMatches(droplet Droplet, project, environment string) bool {
	tags := labelsFromTags(droplet.Tags)
	return tags[LabelManagedBy] == "ship" &&
		tags[LabelProject] == project &&
		tags[LabelEnvironment] == environment
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
		tags = append(tags, tagForLabel(key, value))
	}
	sort.Strings(tags)
	return tags
}

func tagForLabel(key, value string) string {
	return tagPrefix + safeTagPart(key) + ":" + encodeTagValue(value)
}

func labelsFromTags(tags []string) map[string]string {
	labels := map[string]string{}
	for _, tag := range tags {
		trimmed := strings.TrimPrefix(tag, tagPrefix)
		if trimmed == tag {
			continue
		}
		key, encoded, ok := strings.Cut(trimmed, ":")
		if !ok || key == "" || encoded == "" {
			continue
		}
		value, err := decodeTagValue(encoded)
		if err != nil {
			continue
		}
		labels[key] = value
	}
	return labels
}

func encodeTagValue(value string) string {
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(value))
	return strings.ToLower(encoded)
}

func decodeTagValue(value string) (string, error) {
	data, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(value))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func safeTagPart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		allowed := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-'
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
		return "value"
	}
	return out
}

func hostFromDroplet(droplet Droplet) provider.Host {
	labels := labelsFromTags(droplet.Tags)
	return provider.Host{
		ID:            strconv.FormatInt(droplet.ID, 10),
		Name:          droplet.Name,
		Pool:          labels[LabelPool],
		PublicAddress: dropletPublicIPv4(droplet),
		Labels:        labels,
	}
}

func dropletPublicIPv4(droplet Droplet) string {
	for _, network := range droplet.Networks.V4 {
		if network.Type == "public" {
			return network.IPAddress
		}
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

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type listDropletsResponse struct {
	Droplets []Droplet `json:"droplets"`
	Links    links     `json:"links"`
}

type createDropletResponse struct {
	Droplet Droplet `json:"droplet"`
}

type listFirewallsResponse struct {
	Firewalls []Firewall `json:"firewalls"`
	Links     links      `json:"links"`
}

type createFirewallResponse struct {
	Firewall Firewall `json:"firewall"`
}

type links struct {
	Pages struct {
		Next string `json:"next"`
	} `json:"pages"`
}
