package hetzner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

const (
	defaultAPIBase = "https://api.hetzner.cloud/v1"

	LabelManagedBy   = provider.LabelManagedBy
	LabelProject     = provider.LabelProject
	LabelEnvironment = provider.LabelEnvironment
	LabelPool        = provider.LabelPool

	DefaultActionTimeout = 10 * time.Minute
)

type Client struct {
	Token         string
	DryRun        bool
	HTTP          *http.Client
	BaseURL       string
	PollInterval  time.Duration
	ActionTimeout time.Duration
}

type ServerPlan = provider.HostPlan

type Server struct {
	ID         int64             `json:"id"`
	Name       string            `json:"name"`
	Labels     map[string]string `json:"labels"`
	PublicNet  PublicNet         `json:"public_net"`
	PrivateNet []PrivateNet      `json:"private_net"`
}

type Network struct {
	ID     int64             `json:"id"`
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
}

type Firewall struct {
	ID     int64             `json:"id"`
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
}

type PublicNet struct {
	IPv4      PublicIPv4       `json:"ipv4"`
	Firewalls []ServerFirewall `json:"firewalls"`
}

type PublicIPv4 struct {
	IP string `json:"ip"`
}

type PrivateNet struct {
	Network int64 `json:"network"`
}

type ServerFirewall struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

type Action struct {
	ID     int64        `json:"id"`
	Status string       `json:"status"`
	Error  *ActionError `json:"error"`
}

type ActionError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ReconcileResult = provider.ReconcileResult

type DecommissionResult struct {
	Deleted []provider.Host
}

func NewFromEnv(dryRun bool) Client {
	return Client{Token: os.Getenv("HCLOUD_TOKEN"), DryRun: dryRun, HTTP: http.DefaultClient}
}

func (c Client) Name() string {
	return config.ProviderHetzner
}

func (c Client) TokenPresent() bool {
	return strings.TrimSpace(c.Token) != ""
}

func (c Client) CredentialChecks(lookupEnv func(string) (string, bool)) []provider.CredentialCheck {
	_, ok := lookupEnv("HCLOUD_TOKEN")
	return []provider.CredentialCheck{{
		Name:           "hetzner token",
		Present:        ok,
		Required:       true,
		PresentMessage: "HCLOUD_TOKEN is set",
		MissingMessage: "missing HCLOUD_TOKEN",
	}}
}

func DesiredServers(env config.Environment) []provider.HostPlan {
	return DesiredServersFor("", "", env)
}

func DesiredServersFor(project, environment string, env config.Environment) []provider.HostPlan {
	hetzner := env.Provider.Hetzner
	if hetzner == nil {
		return nil
	}
	return provider.HostPlans(project, environment, env, provider.HostPlanOptions{
		Location: hetzner.Location,
		Size:     hetzner.ServerType,
		Image:    hetzner.Image,
		UserData: hetzner.UserData,
	})
}

func (s Server) IPv4() string {
	return s.PublicNet.IPv4.IP
}

func (c Client) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.Hetzner == nil {
		return nil, fmt.Errorf("environment %q must define provider.hetzner", environment)
	}
	return DesiredServersFor(project, environment, env), nil
}

func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	if env.Provider.Hetzner == nil {
		return provider.ReconcileResult{}, fmt.Errorf("environment %q must define provider.hetzner", environment)
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
	result, err = provider.ReconcileHosts(ctx, project, environment, desired, backend)
	if err != nil {
		return provider.ReconcileResult{}, err
	}
	if err := c.EnsureHostAttachments(ctx, result.Existing, backend.networkID, backend.firewallID); err != nil {
		return provider.ReconcileResult{}, err
	}
	return result, nil
}

// reconcileBackendFor resolves the per-environment network and firewall and
// builds the reconcile backend. Reconcile and CreateHost share it so the two
// construct servers identically.
func (c Client) reconcileBackendFor(ctx context.Context, project, environment string, env config.Environment) (reconcileBackend, error) {
	var networkID int64
	if env.Provider.Hetzner.Network.EnabledValue(true) {
		network, err := c.EnsureNetwork(ctx, project, environment, *env.Provider.Hetzner)
		if err != nil {
			return reconcileBackend{}, err
		}
		networkID = network.ID
	}
	var firewallID int64
	if env.Provider.Hetzner.Firewall.EnabledValue(true) {
		firewall, err := c.EnsureFirewall(ctx, project, environment, *env.Provider.Hetzner)
		if err != nil {
			return reconcileBackend{}, err
		}
		firewallID = firewall.ID
	}
	return reconcileBackend{
		client:     c,
		sshKeys:    env.Provider.Hetzner.SSHKeys,
		networkID:  networkID,
		firewallID: firewallID,
	}, nil
}

// CreateHost provisions a single server using the same backend Reconcile would
// build, letting `ship migrate` add a replacement while the old server remains.
func (c Client) CreateHost(ctx context.Context, project, environment string, env config.Environment, plan provider.HostPlan) (provider.Host, error) {
	if env.Provider.Hetzner == nil {
		return provider.Host{}, fmt.Errorf("environment %q must define provider.hetzner", environment)
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
	sshKeys    []string
	networkID  int64
	firewallID int64
}

func (b reconcileBackend) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	return b.client.List(ctx, project, environment)
}

func (b reconcileBackend) Create(ctx context.Context, plan provider.HostPlan) (provider.Host, error) {
	server, err := b.client.CreateServer(ctx, plan, createServerOptions{
		SSHKeys:    b.sshKeys,
		NetworkID:  b.networkID,
		FirewallID: b.firewallID,
	})
	if err != nil {
		return provider.Host{}, err
	}
	return hostFromServer(server), nil
}

func (c Client) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	existing, err := c.ListServers(ctx, project, environment)
	if err != nil {
		return nil, err
	}
	hosts := make([]provider.Host, 0, len(existing))
	for _, server := range existing {
		hosts = append(hosts, hostFromServer(server))
	}
	return hosts, nil
}

func (c Client) Delete(ctx context.Context, host provider.Host) error {
	if strings.TrimSpace(host.ID) == "" {
		return fmt.Errorf("server id is required")
	}
	id, err := strconv.ParseInt(host.ID, 10, 64)
	if err != nil {
		return fmt.Errorf("server id must be numeric: %w", err)
	}
	return c.DeleteServer(ctx, id)
}

func (c Client) ListServers(ctx context.Context, project, environment string) ([]Server, error) {
	if !c.TokenPresent() {
		return nil, fmt.Errorf("HCLOUD_TOKEN is required")
	}
	selector := labelSelector(project, environment)
	page := 1
	var servers []Server
	for {
		values := url.Values{}
		values.Set("label_selector", selector)
		values.Set("page", strconv.Itoa(page))
		var out listServersResponse
		if err := c.request(ctx, http.MethodGet, "/servers?"+values.Encode(), nil, &out); err != nil {
			return nil, err
		}
		servers = append(servers, out.Servers...)
		if out.Meta.Pagination.NextPage == nil {
			break
		}
		page = *out.Meta.Pagination.NextPage
	}
	return servers, nil
}

type createServerOptions struct {
	SSHKeys    []string
	NetworkID  int64
	FirewallID int64
}

func (c Client) CreateServer(ctx context.Context, plan provider.HostPlan, opts createServerOptions) (Server, error) {
	if !c.TokenPresent() {
		return Server{}, fmt.Errorf("HCLOUD_TOKEN is required")
	}
	labels := labelsForPlan(plan)
	body := map[string]any{
		"name":        plan.Name,
		"server_type": plan.Size,
		"image":       plan.Image,
		"location":    plan.Location,
		"labels":      labels,
	}
	if plan.UserData != "" {
		body["user_data"] = plan.UserData
	}
	if len(opts.SSHKeys) > 0 {
		body["ssh_keys"] = opts.SSHKeys
	}
	if opts.NetworkID != 0 {
		body["networks"] = []int64{opts.NetworkID}
	}
	if opts.FirewallID != 0 {
		body["firewalls"] = []map[string]int64{{"firewall": opts.FirewallID}}
	}
	if c.DryRun {
		return Server{Name: plan.Name, Labels: labels}, nil
	}
	var out createServerResponse
	if err := c.request(ctx, http.MethodPost, "/servers", body, &out); err != nil {
		return Server{}, err
	}
	if out.Action.ID != 0 {
		if err := c.WaitAction(ctx, out.Action.ID); err != nil {
			return Server{}, err
		}
	}
	return out.Server, nil
}

func (c Client) WaitAction(ctx context.Context, id int64) error {
	if id == 0 {
		return nil
	}
	ctx, cancel := c.actionContext(ctx)
	defer cancel()
	interval := c.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	for {
		action, err := c.GetAction(ctx, id)
		if err != nil {
			return err
		}
		switch action.Status {
		case "success":
			return nil
		case "error":
			if action.Error != nil {
				return fmt.Errorf("hetzner action %d failed: %s: %s", id, action.Error.Code, action.Error.Message)
			}
			return fmt.Errorf("hetzner action %d failed", id)
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c Client) actionContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	timeout := c.ActionTimeout
	if timeout <= 0 {
		timeout = DefaultActionTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func (c Client) GetAction(ctx context.Context, id int64) (Action, error) {
	var out getActionResponse
	if err := c.request(ctx, http.MethodGet, "/actions/"+strconv.FormatInt(id, 10), nil, &out); err != nil {
		return Action{}, err
	}
	return out.Action, nil
}

func (c Client) Decommission(ctx context.Context, project, environment string) (DecommissionResult, error) {
	if strings.TrimSpace(project) == "" {
		return DecommissionResult{}, fmt.Errorf("project is required")
	}
	if strings.TrimSpace(environment) == "" {
		return DecommissionResult{}, fmt.Errorf("environment is required")
	}
	hosts, err := c.List(ctx, project, environment)
	if err != nil {
		return DecommissionResult{}, err
	}
	result := DecommissionResult{Deleted: make([]provider.Host, 0, len(hosts))}
	for _, host := range hosts {
		if err := c.Delete(ctx, host); err != nil {
			return DecommissionResult{}, err
		}
		result.Deleted = append(result.Deleted, host)
	}
	return result, nil
}

func (c Client) DeleteServer(ctx context.Context, id int64) error {
	if !c.TokenPresent() {
		return fmt.Errorf("HCLOUD_TOKEN is required")
	}
	if id == 0 {
		return fmt.Errorf("server id is required")
	}
	if c.DryRun {
		return nil
	}
	var out deleteServerResponse
	if err := c.request(ctx, http.MethodDelete, "/servers/"+strconv.FormatInt(id, 10), nil, &out); err != nil {
		return err
	}
	if out.Action.ID != 0 {
		return c.WaitAction(ctx, out.Action.ID)
	}
	return nil
}

func (c Client) EnsureHostAttachments(ctx context.Context, hosts []provider.Host, networkID, firewallID int64) error {
	for _, host := range hosts {
		serverID, err := strconv.ParseInt(host.ID, 10, 64)
		if err != nil {
			return fmt.Errorf("server id for %s must be numeric: %w", host.Name, err)
		}
		if networkID != 0 && !containsInt64(host.NetworkIDs, networkID) {
			if err := c.AttachServerToNetwork(ctx, serverID, networkID); err != nil {
				return fmt.Errorf("attach %s to network %d: %w", host.Name, networkID, err)
			}
		}
		if firewallID != 0 && !containsInt64(host.FirewallIDs, firewallID) {
			if err := c.ApplyFirewallToServer(ctx, firewallID, serverID); err != nil {
				return fmt.Errorf("apply firewall %d to %s: %w", firewallID, host.Name, err)
			}
		}
	}
	return nil
}

func (c Client) AttachServerToNetwork(ctx context.Context, serverID, networkID int64) error {
	if !c.TokenPresent() {
		return fmt.Errorf("HCLOUD_TOKEN is required")
	}
	if serverID == 0 {
		return fmt.Errorf("server id is required")
	}
	if networkID == 0 {
		return fmt.Errorf("network id is required")
	}
	if c.DryRun {
		return nil
	}
	body := map[string]any{"network": networkID}
	var out serverActionResponse
	path := fmt.Sprintf("/servers/%d/actions/attach_to_network", serverID)
	if err := c.request(ctx, http.MethodPost, path, body, &out); err != nil {
		return err
	}
	return c.WaitAction(ctx, out.Action.ID)
}

func (c Client) ApplyFirewallToServer(ctx context.Context, firewallID, serverID int64) error {
	if !c.TokenPresent() {
		return fmt.Errorf("HCLOUD_TOKEN is required")
	}
	if firewallID == 0 {
		return fmt.Errorf("firewall id is required")
	}
	if serverID == 0 {
		return fmt.Errorf("server id is required")
	}
	if c.DryRun {
		return nil
	}
	body := map[string]any{
		"apply_to": []map[string]any{{
			"type":   "server",
			"server": map[string]int64{"id": serverID},
		}},
	}
	var out firewallActionsResponse
	path := fmt.Sprintf("/firewalls/%d/actions/apply_to_resources", firewallID)
	if err := c.request(ctx, http.MethodPost, path, body, &out); err != nil {
		return err
	}
	for _, action := range out.Actions {
		if err := c.WaitAction(ctx, action.ID); err != nil {
			return err
		}
	}
	return nil
}

func (c Client) EnsureNetwork(ctx context.Context, project, environment string, cfg config.HetznerConfig) (Network, error) {
	name := cfg.Network.Name
	if strings.TrimSpace(name) == "" {
		name = resourceName(project, environment, "network")
	}
	existing, err := c.ListNetworks(ctx, project, environment)
	if err != nil {
		return Network{}, err
	}
	for _, network := range existing {
		if network.Name == name {
			return network, nil
		}
	}
	ipRange := cfg.Network.IPRange
	if strings.TrimSpace(ipRange) == "" {
		ipRange = "10.98.0.0/16"
	}
	body := map[string]any{
		"name":     name,
		"ip_range": ipRange,
		"labels":   resourceLabels(project, environment, "network"),
	}
	var out createNetworkResponse
	if err := c.request(ctx, http.MethodPost, "/networks", body, &out); err != nil {
		return Network{}, err
	}
	return out.Network, nil
}

func (c Client) ListNetworks(ctx context.Context, project, environment string) ([]Network, error) {
	if !c.TokenPresent() {
		return nil, fmt.Errorf("HCLOUD_TOKEN is required")
	}
	values := url.Values{}
	values.Set("label_selector", labelSelector(project, environment))
	var out listNetworksResponse
	if err := c.request(ctx, http.MethodGet, "/networks?"+values.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Networks, nil
}

func (c Client) EnsureFirewall(ctx context.Context, project, environment string, cfg config.HetznerConfig) (Firewall, error) {
	name := cfg.Firewall.Name
	if strings.TrimSpace(name) == "" {
		name = resourceName(project, environment, "firewall")
	}
	existing, err := c.ListFirewalls(ctx, project, environment)
	if err != nil {
		return Firewall{}, err
	}
	for _, firewall := range existing {
		if firewall.Name == name {
			return firewall, nil
		}
	}
	body := map[string]any{
		"name":   name,
		"labels": resourceLabels(project, environment, "firewall"),
		"rules":  firewallRules(cfg),
	}
	var out createFirewallResponse
	if err := c.request(ctx, http.MethodPost, "/firewalls", body, &out); err != nil {
		return Firewall{}, err
	}
	return out.Firewall, nil
}

func (c Client) ListFirewalls(ctx context.Context, project, environment string) ([]Firewall, error) {
	if !c.TokenPresent() {
		return nil, fmt.Errorf("HCLOUD_TOKEN is required")
	}
	values := url.Values{}
	values.Set("label_selector", labelSelector(project, environment))
	var out listFirewallsResponse
	if err := c.request(ctx, http.MethodGet, "/firewalls?"+values.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Firewalls, nil
}

func firewallRules(cfg config.HetznerConfig) []map[string]any {
	rules := []map[string]any{}
	if cfg.EffectiveSSHFirewall() == config.SSHFirewallManaged {
		rules = append(rules, map[string]any{
			"direction":  "in",
			"protocol":   "tcp",
			"port":       "22",
			"source_ips": append([]string(nil), cfg.SSHAllowedCIDRs...),
		})
	}
	rules = append(rules,
		map[string]any{"direction": "in", "protocol": "tcp", "port": "80", "source_ips": []string{"0.0.0.0/0", "::/0"}},
		map[string]any{"direction": "in", "protocol": "tcp", "port": "443", "source_ips": []string{"0.0.0.0/0", "::/0"}},
		map[string]any{"direction": "in", "protocol": "udp", "port": "443", "source_ips": []string{"0.0.0.0/0", "::/0"}},
	)
	return rules
}

func resourceName(project, environment, kind string) string {
	return strings.Join([]string{"ship", safeName(project), safeName(environment), kind}, "-")
}

func resourceLabels(project, environment, kind string) map[string]string {
	labels := provider.ShipLabels(project, environment, kind)
	labels[LabelPool] = kind
	return labels
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
		return fmt.Errorf("hetzner %s %s failed: %s", method, path, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c Client) apiBase() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return defaultAPIBase
}

func labelSelector(project, environment string) string {
	return strings.Join([]string{
		LabelManagedBy + "=ship",
		LabelProject + "=" + project,
		LabelEnvironment + "=" + environment,
	}, ",")
}

func labelsForPlan(plan provider.HostPlan) map[string]string {
	if len(plan.Labels) > 0 {
		return plan.Labels
	}
	return provider.ShipLabels(plan.Project, plan.Environment, plan.Pool)
}

func hostFromServer(server Server) provider.Host {
	return provider.Host{
		ID:            strconv.FormatInt(server.ID, 10),
		Name:          server.Name,
		Pool:          server.Labels[LabelPool],
		PublicAddress: server.IPv4(),
		Labels:        server.Labels,
		NetworkIDs:    networkIDs(server),
		FirewallIDs:   firewallIDs(server),
	}
}

func networkIDs(server Server) []int64 {
	ids := make([]int64, 0, len(server.PrivateNet))
	for _, network := range server.PrivateNet {
		if network.Network != 0 {
			ids = append(ids, network.Network)
		}
	}
	return ids
}

func firewallIDs(server Server) []int64 {
	ids := make([]int64, 0, len(server.PublicNet.Firewalls))
	for _, firewall := range server.PublicNet.Firewalls {
		if firewall.ID != 0 {
			ids = append(ids, firewall.ID)
		}
	}
	return ids
}

func containsInt64(values []int64, want int64) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type listServersResponse struct {
	Servers []Server `json:"servers"`
	Meta    struct {
		Pagination struct {
			NextPage *int `json:"next_page"`
		} `json:"pagination"`
	} `json:"meta"`
}

type createServerResponse struct {
	Server Server `json:"server"`
	Action Action `json:"action"`
}

type getActionResponse struct {
	Action Action `json:"action"`
}

type deleteServerResponse struct {
	Action Action `json:"action"`
}

type serverActionResponse struct {
	Action Action `json:"action"`
}

type firewallActionsResponse struct {
	Actions []Action `json:"actions"`
}

type listNetworksResponse struct {
	Networks []Network `json:"networks"`
}

type createNetworkResponse struct {
	Network Network `json:"network"`
}

type listFirewallsResponse struct {
	Firewalls []Firewall `json:"firewalls"`
}

type createFirewallResponse struct {
	Firewall Firewall `json:"firewall"`
}
