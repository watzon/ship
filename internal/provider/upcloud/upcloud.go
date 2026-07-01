package upcloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

const (
	defaultAPIBase = "https://api.upcloud.com/1.3"

	LabelManagedBy   = provider.LabelManagedBy
	LabelProject     = provider.LabelProject
	LabelEnvironment = provider.LabelEnvironment
	LabelPool        = provider.LabelPool
)

type Client struct {
	Username string
	Password string
	Zone     string
	DryRun   bool
	HTTP     *http.Client
	BaseURL  string
}

type ServerPlan = provider.HostPlan

type Server struct {
	UUID        string      `json:"uuid"`
	Title       string      `json:"title"`
	Hostname    string      `json:"hostname"`
	Zone        string      `json:"zone"`
	State       string      `json:"state"`
	Plan        string      `json:"plan"`
	Firewall    string      `json:"firewall"`
	Labels      labelsValue `json:"labels"`
	Tags        tagsValue   `json:"tags"`
	IPAddresses struct {
		IPAddress []IPAddress `json:"ip_address"`
	} `json:"ip_addresses"`
	Networking struct {
		Interfaces struct {
			Interface []NetworkInterface `json:"interface"`
		} `json:"interfaces"`
	} `json:"networking"`
}

type IPAddress struct {
	Access  string `json:"access"`
	Address string `json:"address"`
	Family  string `json:"family"`
}

type NetworkInterface struct {
	Type        string `json:"type"`
	Network     string `json:"network,omitempty"`
	IPAddresses struct {
		IPAddress []InterfaceIPAddress `json:"ip_address"`
	} `json:"ip_addresses,omitempty"`
}

type InterfaceIPAddress struct {
	Address      string `json:"address,omitempty"`
	Family       string `json:"family"`
	DHCPProvided string `json:"dhcp_provided,omitempty"`
}

type FirewallRule struct {
	Action                  string `json:"action"`
	Comment                 string `json:"comment,omitempty"`
	Direction               string `json:"direction"`
	Family                  string `json:"family"`
	Protocol                string `json:"protocol"`
	Position                string `json:"position,omitempty"`
	SourceAddressStart      string `json:"source_address_start,omitempty"`
	SourceAddressEnd        string `json:"source_address_end,omitempty"`
	DestinationAddressStart string `json:"destination_address_start,omitempty"`
	DestinationAddressEnd   string `json:"destination_address_end,omitempty"`
	DestinationPortStart    string `json:"destination_port_start,omitempty"`
	DestinationPortEnd      string `json:"destination_port_end,omitempty"`
	SourcePortStart         string `json:"source_port_start,omitempty"`
	SourcePortEnd           string `json:"source_port_end,omitempty"`
	ICMPType                string `json:"icmp_type,omitempty"`
}

type ReconcileResult = provider.ReconcileResult

func NewFromEnv(dryRun bool, configs ...config.UpCloudConfig) Client {
	cfg := config.UpCloudConfig{}
	if len(configs) > 0 {
		cfg = configs[0]
	}
	return Client{
		Username: os.Getenv("UPCLOUD_USERNAME"),
		Password: os.Getenv("UPCLOUD_PASSWORD"),
		Zone:     cfg.Zone,
		DryRun:   dryRun,
		HTTP:     http.DefaultClient,
	}
}

func (c Client) Name() string {
	return config.ProviderUpCloud
}

func (c Client) CredentialsPresent() bool {
	return strings.TrimSpace(c.Username) != "" && strings.TrimSpace(c.Password) != ""
}

func (c Client) CredentialChecks(lookupEnv func(string) (string, bool)) []provider.CredentialCheck {
	_, userOK := lookupEnv("UPCLOUD_USERNAME")
	_, passwordOK := lookupEnv("UPCLOUD_PASSWORD")
	return []provider.CredentialCheck{{
		Name:           "upcloud credentials",
		Present:        userOK && passwordOK,
		Required:       true,
		PresentMessage: "UPCLOUD_USERNAME/UPCLOUD_PASSWORD are set",
		MissingMessage: "missing UPCLOUD_USERNAME/UPCLOUD_PASSWORD",
	}}
}

func DesiredServers(env config.Environment) []provider.HostPlan {
	return DesiredServersFor("", "", env)
}

func DesiredServersFor(project, environment string, env config.Environment) []provider.HostPlan {
	upcloud := env.Provider.UpCloud
	if upcloud == nil {
		return nil
	}
	return provider.HostPlans(project, environment, env, provider.HostPlanOptions{
		Location: upcloud.Zone,
		Size:     upcloud.Plan,
		Image:    upcloud.Template,
		UserData: upcloud.UserData,
	})
}

func (c Client) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.UpCloud == nil {
		return nil, fmt.Errorf("environment %q must define provider.upcloud", environment)
	}
	return DesiredServersFor(project, environment, env), nil
}

func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	if env.Provider.UpCloud == nil {
		return provider.ReconcileResult{}, fmt.Errorf("environment %q must define provider.upcloud", environment)
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
	upcloud := *env.Provider.UpCloud
	return provider.ReconcileHosts(ctx, project, environment, desired, reconcileBackend{client: c, upcloud: upcloud})
}

type reconcileBackend struct {
	client  Client
	upcloud config.UpCloudConfig
}

func (b reconcileBackend) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	return b.client.List(ctx, project, environment)
}

func (b reconcileBackend) Create(ctx context.Context, plan provider.HostPlan) (provider.Host, error) {
	server, err := b.client.CreateServer(ctx, plan, b.upcloud)
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
		return fmt.Errorf("upcloud server uuid is required")
	}
	if err := c.StopServer(ctx, host.ID); err != nil {
		return err
	}
	return c.DeleteServer(ctx, host.ID)
}

func (c Client) ListServers(ctx context.Context, project, environment string) ([]Server, error) {
	if !c.CredentialsPresent() {
		return nil, fmt.Errorf("UPCLOUD_USERNAME/UPCLOUD_PASSWORD are required")
	}
	values := url.Values{}
	values.Add("label", LabelManagedBy+"=ship")
	values.Add("label", LabelProject+"="+project)
	values.Add("label", LabelEnvironment+"="+environment)
	if c.Zone != "" {
		values.Set("search", c.Zone)
	}
	var out listServersResponse
	if err := c.request(ctx, http.MethodGet, "/server?"+values.Encode(), nil, &out); err != nil {
		return nil, err
	}
	var servers []Server
	for _, summary := range out.Servers.Server {
		if !serverMatches(summary, project, environment) {
			continue
		}
		detail, err := c.GetServer(ctx, summary.UUID)
		if err != nil {
			return nil, err
		}
		servers = append(servers, detail)
	}
	return servers, nil
}

func (c Client) GetServer(ctx context.Context, uuid string) (Server, error) {
	var out serverResponse
	if err := c.request(ctx, http.MethodGet, "/server/"+url.PathEscape(uuid), nil, &out); err != nil {
		return Server{}, err
	}
	return out.Server, nil
}

func (c Client) CreateServer(ctx context.Context, plan provider.HostPlan, upcloud config.UpCloudConfig) (Server, error) {
	if !c.CredentialsPresent() {
		return Server{}, fmt.Errorf("UPCLOUD_USERNAME/UPCLOUD_PASSWORD are required")
	}
	upcloud = withPlanDefaults(plan, upcloud)
	body := map[string]any{"server": createServerBody(plan, upcloud)}
	if c.DryRun {
		return Server{UUID: plan.Name, Title: plan.Name, Hostname: firstNonEmpty(upcloud.Hostname, plan.Name), Labels: labelsFromMap(labelsForPlan(plan))}, nil
	}
	var out serverResponse
	if err := c.request(ctx, http.MethodPost, "/server", body, &out); err != nil {
		return Server{}, err
	}
	server := out.Server
	if upcloud.Firewall.ManagedValue(true) {
		if err := c.EnsureFirewallRules(ctx, server.UUID, upcloud); err != nil {
			return Server{}, err
		}
	}
	return server, nil
}

func (c Client) StopServer(ctx context.Context, uuid string) error {
	body := map[string]any{"stop_server": map[string]string{"stop_type": "soft", "timeout": "60"}}
	return c.request(ctx, http.MethodPost, "/server/"+url.PathEscape(uuid)+"/stop", body, nil)
}

func (c Client) DeleteServer(ctx context.Context, uuid string) error {
	values := url.Values{}
	values.Set("storages", "1")
	values.Set("backups", "delete")
	return c.request(ctx, http.MethodDelete, "/server/"+url.PathEscape(uuid)+"?"+values.Encode(), nil, nil)
}

func (c Client) EnsureFirewallRules(ctx context.Context, uuid string, upcloud config.UpCloudConfig) error {
	existing, err := c.ListFirewallRules(ctx, uuid)
	if err != nil {
		return err
	}
	position := 1
	for _, rule := range firewallRules(upcloud) {
		if containsFirewallRule(existing, rule) {
			continue
		}
		rule.Position = fmt.Sprintf("%d", position)
		if err := c.CreateFirewallRule(ctx, uuid, rule); err != nil {
			return err
		}
		position++
	}
	return nil
}

func (c Client) ListFirewallRules(ctx context.Context, uuid string) ([]FirewallRule, error) {
	var out listFirewallRulesResponse
	if err := c.request(ctx, http.MethodGet, "/server/"+url.PathEscape(uuid)+"/firewall_rule", nil, &out); err != nil {
		return nil, err
	}
	return out.FirewallRules.FirewallRule, nil
}

func (c Client) CreateFirewallRule(ctx context.Context, uuid string, rule FirewallRule) error {
	body := map[string]any{"firewall_rule": rule}
	var out firewallRuleResponse
	return c.request(ctx, http.MethodPost, "/server/"+url.PathEscape(uuid)+"/firewall_rule", body, &out)
}

func (c Client) request(ctx context.Context, method, path string, payload any, out any) error {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = defaultAPIBase
	}
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(c.Username, c.Password)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
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
		return fmt.Errorf("upcloud api %s %s failed: %s: %s", method, path, res.Status, strings.TrimSpace(string(data)))
	}
	if out != nil && res.StatusCode != http.StatusNoContent && res.ContentLength != 0 {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil && err != io.EOF {
			return err
		}
	}
	return nil
}

func createServerBody(plan provider.HostPlan, upcloud config.UpCloudConfig) map[string]any {
	body := map[string]any{
		"zone":     plan.Location,
		"title":    plan.Name,
		"hostname": firstNonEmpty(upcloud.Hostname, plan.Name),
		"plan":     plan.Size,
		"labels":   labelsFromMap(labelsForPlan(plan)),
		"tags":     tagsFromLabels(labelsForPlan(plan)),
		"firewall": boolOnOff(upcloud.Firewall.ManagedValue(true)),
		"storage_devices": map[string]any{"storage_device": []map[string]any{{
			"action":  "clone",
			"storage": plan.Image,
			"title":   "Storage for " + plan.Name,
		}}},
		"networking": map[string]any{"interfaces": map[string]any{"interface": networkInterfaces(upcloud)}},
		"login_user": map[string]any{
			"username": firstNonEmpty(upcloud.Username, "root"),
			"ssh_keys": map[string]any{"ssh_key": upcloud.SSHKeys},
		},
	}
	if upcloud.StorageSizeGB > 0 || upcloud.StorageTier != "" {
		storage := body["storage_devices"].(map[string]any)["storage_device"].([]map[string]any)[0]
		if upcloud.StorageSizeGB > 0 {
			storage["size"] = upcloud.StorageSizeGB
		}
		if upcloud.StorageTier != "" {
			storage["tier"] = upcloud.StorageTier
		}
	}
	if upcloud.Metadata != nil {
		body["metadata"] = boolYesNo(*upcloud.Metadata)
	} else if strings.TrimSpace(plan.UserData) != "" {
		body["metadata"] = "yes"
	}
	if strings.TrimSpace(plan.UserData) != "" {
		body["user_data"] = plan.UserData
	}
	if upcloud.SimpleBackup != "" {
		body["simple_backup"] = upcloud.SimpleBackup
	}
	if upcloud.ServerGroup != "" {
		body["server_group"] = upcloud.ServerGroup
	}
	if upcloud.Timezone != "" {
		body["timezone"] = upcloud.Timezone
	}
	return body
}

func networkInterfaces(upcloud config.UpCloudConfig) []map[string]any {
	interfaces := []map[string]any{networkInterface("public", "", "IPv4")}
	if upcloud.UtilityNetwork == nil || *upcloud.UtilityNetwork {
		interfaces = append(interfaces, networkInterface("utility", "", "IPv4"))
	}
	if upcloud.IPv6 != nil && *upcloud.IPv6 {
		interfaces = append(interfaces, networkInterface("public", "", "IPv6"))
	}
	if upcloud.PrivateNetworkID != "" {
		interfaces = append(interfaces, networkInterface("private", upcloud.PrivateNetworkID, "IPv4"))
	}
	return interfaces
}

func networkInterface(kind, network, family string) map[string]any {
	item := map[string]any{
		"type": kind,
		"ip_addresses": map[string]any{"ip_address": []map[string]string{{
			"family": family,
		}}},
	}
	if network != "" {
		item["network"] = network
	}
	return item
}

func firewallRules(upcloud config.UpCloudConfig) []FirewallRule {
	var rules []FirewallRule
	if upcloud.EffectiveSSHFirewall() == config.SSHFirewallManaged {
		for _, cidr := range upcloud.SSHAllowedCIDRs {
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

func firewallRule(comment, protocol, cidr string, port int) FirewallRule {
	sourceStart, sourceEnd := addressRange(cidr)
	return FirewallRule{
		Action:                  "accept",
		Comment:                 comment,
		Direction:               "in",
		Family:                  ipFamily(cidr),
		Protocol:                protocol,
		SourceAddressStart:      sourceStart,
		SourceAddressEnd:        sourceEnd,
		DestinationPortStart:    fmt.Sprintf("%d", port),
		DestinationPortEnd:      fmt.Sprintf("%d", port),
		DestinationAddressStart: "",
		DestinationAddressEnd:   "",
	}
}

func addressRange(value string) (string, string) {
	ip, network, err := net.ParseCIDR(strings.TrimSpace(value))
	if err != nil {
		return value, value
	}
	ones, bits := network.Mask.Size()
	first := ip.Mask(network.Mask)
	if bits == 32 {
		return ipv4String(first), ipv4String(lastIP(first, bits-ones))
	}
	return first.String(), lastIP(first, bits-ones).String()
}

func lastIP(first net.IP, hostBits int) net.IP {
	ip := first.To16()
	if ip == nil {
		return first
	}
	value := new(big.Int).SetBytes(ip)
	size := new(big.Int).Lsh(big.NewInt(1), uint(hostBits))
	value.Add(value, size.Sub(size, big.NewInt(1)))
	return net.IP(value.FillBytes(make([]byte, len(ip))))
}

func ipv4String(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}

func containsFirewallRule(existing []FirewallRule, want FirewallRule) bool {
	for _, got := range existing {
		if got.Action == want.Action &&
			got.Direction == want.Direction &&
			got.Family == want.Family &&
			got.Protocol == want.Protocol &&
			got.SourceAddressStart == want.SourceAddressStart &&
			got.SourceAddressEnd == want.SourceAddressEnd &&
			got.DestinationPortStart == want.DestinationPortStart &&
			got.DestinationPortEnd == want.DestinationPortEnd {
			return true
		}
	}
	return false
}

func withPlanDefaults(plan provider.HostPlan, upcloud config.UpCloudConfig) config.UpCloudConfig {
	if plan.Location != "" {
		upcloud.Zone = plan.Location
	}
	if plan.Size != "" {
		upcloud.Plan = plan.Size
	}
	if plan.Image != "" {
		upcloud.Template = plan.Image
	}
	if plan.UserData != "" {
		upcloud.UserData = plan.UserData
	}
	return upcloud
}

func hostFromServer(server Server) provider.Host {
	labels := labelsToMap(server.Labels)
	return provider.Host{
		ID:            server.UUID,
		Name:          firstNonEmpty(server.Title, server.Hostname),
		Pool:          labels[LabelPool],
		PublicAddress: publicAddress(server),
		Labels:        labels,
	}
}

func publicAddress(server Server) string {
	for _, ip := range server.IPAddresses.IPAddress {
		if strings.EqualFold(ip.Access, "public") && strings.EqualFold(ip.Family, "IPv4") {
			return ip.Address
		}
	}
	for _, iface := range server.Networking.Interfaces.Interface {
		if !strings.EqualFold(iface.Type, "public") {
			continue
		}
		for _, ip := range iface.IPAddresses.IPAddress {
			if strings.EqualFold(ip.Family, "IPv4") {
				return ip.Address
			}
		}
	}
	for _, ip := range server.IPAddresses.IPAddress {
		if strings.EqualFold(ip.Access, "public") {
			return ip.Address
		}
	}
	return ""
}

func serverMatches(server Server, project, environment string) bool {
	labels := labelsToMap(server.Labels)
	return labels[LabelManagedBy] == "ship" &&
		labels[LabelProject] == project &&
		labels[LabelEnvironment] == environment
}

func labelsForPlan(plan provider.HostPlan) map[string]string {
	labels := plan.Labels
	if labels == nil {
		labels = provider.ShipLabels(plan.Project, plan.Environment, plan.Pool)
	}
	return labels
}

func labelsFromMap(labels map[string]string) labelsValue {
	items := make([]label, 0, len(labels))
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if key == "" || labels[key] == "" {
			continue
		}
		items = append(items, label{Key: key, Value: labels[key]})
	}
	return labelsValue{Label: items}
}

func labelsToMap(labels labelsValue) map[string]string {
	out := map[string]string{}
	for _, label := range labels.Label {
		if label.Key != "" && label.Value != "" {
			out[label.Key] = label.Value
		}
	}
	return out
}

func tagsFromLabels(labels map[string]string) tagsValue {
	tags := make([]string, 0, len(labels))
	for key, value := range labels {
		if key == "" || value == "" {
			continue
		}
		tags = append(tags, safeTag(key+"-"+value))
	}
	sort.Strings(tags)
	return tagsValue{Tag: tags}
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

func boolOnOff(value bool) string {
	if value {
		return "on"
	}
	return "off"
}

func boolYesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func ipFamily(cidr string) string {
	if strings.Contains(cidr, ":") {
		return "IPv6"
	}
	return "IPv4"
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
	Servers struct {
		Server []Server `json:"server"`
	} `json:"servers"`
}

type serverResponse struct {
	Server Server `json:"server"`
}

type listFirewallRulesResponse struct {
	FirewallRules struct {
		FirewallRule []FirewallRule `json:"firewall_rule"`
	} `json:"firewall_rules"`
}

type firewallRuleResponse struct {
	FirewallRule FirewallRule `json:"firewall_rule"`
}

type labelsValue struct {
	Label []label `json:"label"`
}

type label struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type tagsValue struct {
	Tag []string `json:"tag"`
}
