package proxmox

import (
	"context"
	"crypto/tls"
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
	"time"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

const defaultAPIBase = "https://localhost:8006/api2/json"

var taskPollInterval = 2 * time.Second

type Client struct {
	Token   string
	DryRun  bool
	HTTP    *http.Client
	BaseURL string
	Config  config.ProxmoxConfig
}

type VM struct {
	VMID   int    `json:"vmid"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Tags   string `json:"tags"`
}

type TaskStatus struct {
	Status     string `json:"status"`
	ExitStatus string `json:"exitstatus"`
}

type apiResponse struct {
	Data json.RawMessage `json:"data"`
}

func NewFromEnv(dryRun bool, cfg config.ProxmoxConfig) Client {
	token := firstNonEmptyEnv("PROXMOX_API_TOKEN", "PVE_API_TOKEN", "PVEAPITOKEN")
	return Client{
		Token:   token,
		DryRun:  dryRun,
		HTTP:    defaultHTTPClient(cfg.InsecureSkipTLSVerify),
		BaseURL: cfg.APIURL,
		Config:  cfg,
	}
}

func (c Client) Name() string {
	return config.ProviderProxmox
}

func (c Client) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.Proxmox == nil {
		return nil, fmt.Errorf("environment %q must define provider.proxmox", environment)
	}
	cfg := *env.Provider.Proxmox
	return provider.HostPlans(project, environment, env, provider.HostPlanOptions{
		Location: cfg.Node,
		Size:     proxmoxSize(cfg),
		Image:    fmt.Sprintf("template:%d", cfg.TemplateID),
	}), nil
}

func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	if env.Provider.Proxmox == nil {
		return provider.ReconcileResult{}, fmt.Errorf("environment %q must define provider.proxmox", environment)
	}
	c.Config = *env.Provider.Proxmox
	plans, err := c.PlanHosts(project, environment, env)
	if err != nil {
		return provider.ReconcileResult{}, err
	}
	if c.DryRun {
		return provider.ReconcileResult{Desired: plans}, nil
	}
	return provider.ReconcileHosts(ctx, project, environment, plans, c)
}

func (c Client) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	cfg := c.Config
	if cfg.Node == "" {
		return nil, fmt.Errorf("provider.proxmox.node is required")
	}
	var vms []VM
	if err := c.get(ctx, fmt.Sprintf("/nodes/%s/qemu", pathEscape(cfg.Node)), url.Values{"full": {"1"}}, &vms); err != nil {
		return nil, err
	}
	required := requiredTags(project, environment)
	var hosts []provider.Host
	for _, vm := range vms {
		tags := parseTags(vm.Tags)
		if !hasTags(tags, required) {
			continue
		}
		pool := poolFromTags(tags)
		address := c.publicAddress(ctx, cfg.Node, vm)
		hosts = append(hosts, provider.Host{
			ID:            strconv.Itoa(vm.VMID),
			Name:          strings.TrimSpace(vm.Name),
			Pool:          pool,
			PublicAddress: firstNonEmpty(address, strings.TrimSpace(vm.Name)),
			Labels:        provider.ShipLabels(project, environment, pool),
		})
	}
	sortHosts(hosts)
	return hosts, nil
}

func (c Client) Create(ctx context.Context, plan provider.HostPlan) (provider.Host, error) {
	cfg := c.Config
	if c.DryRun {
		return provider.Host{ID: "dry-run", Name: plan.Name, Pool: plan.Pool, PublicAddress: "0.0.0.0", Labels: plan.Labels}, nil
	}
	vmid, err := c.NextID(ctx)
	if err != nil {
		return provider.Host{}, err
	}
	clone := url.Values{
		"newid": {strconv.Itoa(vmid)},
		"name":  {plan.Name},
	}
	if fullClone(cfg) {
		clone.Set("full", "1")
	}
	if cfg.Storage != "" {
		clone.Set("storage", cfg.Storage)
	}
	if cfg.Pool != "" {
		clone.Set("pool", cfg.Pool)
	}
	if description := proxmoxDescription(cfg, plan); description != "" {
		clone.Set("description", description)
	}
	upid, err := c.post(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/clone", pathEscape(cfg.Node), cfg.TemplateID), clone)
	if err != nil {
		return provider.Host{}, err
	}
	if err := c.WaitTask(ctx, cfg.Node, upid); err != nil {
		return provider.Host{}, err
	}
	update := c.configValues(plan)
	if len(update) > 0 {
		upid, err = c.post(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", pathEscape(cfg.Node), vmid), update)
		if err != nil {
			return provider.Host{}, err
		}
		if err := c.WaitTask(ctx, cfg.Node, upid); err != nil {
			return provider.Host{}, err
		}
	}
	if startVM(cfg) {
		upid, err = c.post(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/status/start", pathEscape(cfg.Node), vmid), nil)
		if err != nil {
			return provider.Host{}, err
		}
		if err := c.WaitTask(ctx, cfg.Node, upid); err != nil {
			return provider.Host{}, err
		}
	}
	vm := VM{VMID: vmid, Name: plan.Name}
	return provider.Host{
		ID:            strconv.Itoa(vmid),
		Name:          plan.Name,
		Pool:          plan.Pool,
		PublicAddress: firstNonEmpty(c.publicAddress(ctx, cfg.Node, vm), plan.Name),
		Labels:        plan.Labels,
	}, nil
}

func (c Client) Delete(ctx context.Context, host provider.Host) error {
	id, err := strconv.Atoi(host.ID)
	if err != nil {
		return fmt.Errorf("proxmox host %q has non-numeric VMID %q", host.Name, host.ID)
	}
	values := url.Values{
		"purge":                      {"1"},
		"destroy-unreferenced-disks": {"1"},
	}
	upid, err := c.delete(ctx, fmt.Sprintf("/nodes/%s/qemu/%d", pathEscape(c.Config.Node), id), values)
	if err != nil {
		return err
	}
	return c.WaitTask(ctx, c.Config.Node, upid)
}

func (c Client) CredentialChecks(lookupEnv func(string) (string, bool)) []provider.CredentialCheck {
	_, ok := lookupEnv("PROXMOX_API_TOKEN")
	if !ok {
		_, ok = lookupEnv("PVE_API_TOKEN")
	}
	if !ok {
		_, ok = lookupEnv("PVEAPITOKEN")
	}
	return []provider.CredentialCheck{{
		Name:           "proxmox api token",
		Present:        ok,
		Required:       true,
		PresentMessage: "Proxmox API token is set",
		MissingMessage: "set PROXMOX_API_TOKEN or PVE_API_TOKEN",
	}}
}

func (c Client) NextID(ctx context.Context) (int, error) {
	var id int
	if err := c.get(ctx, "/cluster/nextid", nil, &id); err != nil {
		return 0, err
	}
	return id, nil
}

func (c Client) WaitTask(ctx context.Context, node, upid string) error {
	if strings.TrimSpace(upid) == "" {
		return nil
	}
	for {
		var status TaskStatus
		err := c.get(ctx, fmt.Sprintf("/nodes/%s/tasks/%s/status", pathEscape(node), pathEscape(upid)), nil, &status)
		if err != nil {
			return err
		}
		if status.Status == "stopped" {
			if status.ExitStatus == "" || status.ExitStatus == "OK" {
				return nil
			}
			return fmt.Errorf("proxmox task %s failed: %s", upid, status.ExitStatus)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(taskPollInterval):
		}
	}
}

func (c Client) configValues(plan provider.HostPlan) url.Values {
	cfg := c.Config
	values := url.Values{}
	tags := proxmoxTags(plan.Project, plan.Environment, plan.Pool, cfg.Tags)
	if len(tags) > 0 {
		values.Set("tags", strings.Join(tags, ";"))
	}
	if user := firstNonEmpty(cfg.CIUser, plan.User); user != "" {
		values.Set("ciuser", user)
	}
	if len(cfg.SSHKeys) > 0 {
		values.Set("sshkeys", strings.Join(cfg.SSHKeys, "\n"))
	}
	if ipconfig := firstNonEmpty(cfg.IPConfig, "ip=dhcp"); ipconfig != "" {
		values.Set("ipconfig0", ipconfig)
	}
	if cfg.Nameserver != "" {
		values.Set("nameserver", cfg.Nameserver)
	}
	if cfg.SearchDomain != "" {
		values.Set("searchdomain", cfg.SearchDomain)
	}
	if cfg.MemoryMB > 0 {
		values.Set("memory", strconv.Itoa(cfg.MemoryMB))
	}
	if cfg.Cores > 0 {
		values.Set("cores", strconv.Itoa(cfg.Cores))
	}
	if cfg.Bridge != "" {
		net0 := "virtio,bridge=" + cfg.Bridge
		if cfg.VLAN > 0 {
			net0 += ",tag=" + strconv.Itoa(cfg.VLAN)
		}
		values.Set("net0", net0)
	}
	if cfg.Agent == nil || *cfg.Agent {
		values.Set("agent", "enabled=1")
	}
	if cfg.OnBoot != nil {
		values.Set("onboot", boolValue(*cfg.OnBoot))
	}
	if description := proxmoxDescription(cfg, plan); description != "" {
		values.Set("description", description)
	}
	return values
}

func (c Client) publicAddress(ctx context.Context, node string, vm VM) string {
	if vm.VMID == 0 {
		return ""
	}
	var payload struct {
		Result []struct {
			Name        string `json:"name"`
			IPAddresses []struct {
				Address string `json:"ip-address"`
				Type    string `json:"ip-address-type"`
			} `json:"ip-addresses"`
		} `json:"result"`
	}
	if err := c.get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/agent/network-get-interfaces", pathEscape(node), vm.VMID), nil, &payload); err != nil {
		return ""
	}
	var fallback string
	for _, iface := range payload.Result {
		for _, addr := range iface.IPAddresses {
			ip := net.ParseIP(addr.Address)
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if ip.To4() != nil {
				return addr.Address
			}
			if fallback == "" {
				fallback = addr.Address
			}
		}
	}
	return fallback
}

func (c Client) get(ctx context.Context, path string, query url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, query, nil, out)
}

func (c Client) post(ctx context.Context, path string, values url.Values) (string, error) {
	var upid string
	if err := c.do(ctx, http.MethodPost, path, nil, values, &upid); err != nil {
		return "", err
	}
	return upid, nil
}

func (c Client) delete(ctx context.Context, path string, values url.Values) (string, error) {
	var upid string
	if err := c.do(ctx, http.MethodDelete, path, values, nil, &upid); err != nil {
		return "", err
	}
	return upid, nil
}

func (c Client) do(ctx context.Context, method, path string, query url.Values, values url.Values, out any) error {
	base := strings.TrimRight(firstNonEmpty(c.BaseURL, c.Config.APIURL, defaultAPIBase), "/")
	if !strings.HasSuffix(base, "/api2/json") {
		base += "/api2/json"
	}
	reqURL := base + path
	if len(query) > 0 {
		reqURL += "?" + query.Encode()
	}
	var body io.Reader
	if len(values) > 0 {
		body = strings.NewReader(values.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return err
	}
	if len(values) > 0 {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if token := c.authToken(); token != "" {
		req.Header.Set("Authorization", token)
	}
	client := c.HTTP
	if client == nil {
		client = defaultHTTPClient(c.Config.InsecureSkipTLSVerify)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("proxmox %s %s failed: %s", method, path, strings.TrimSpace(string(data)))
	}
	var wrapped apiResponse
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return fmt.Errorf("decode proxmox response: %w", err)
	}
	if out == nil || len(wrapped.Data) == 0 || string(wrapped.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(wrapped.Data, out); err != nil {
		return fmt.Errorf("decode proxmox data: %w", err)
	}
	return nil
}

func (c Client) authToken() string {
	token := strings.TrimSpace(c.Token)
	if token == "" {
		return ""
	}
	if strings.HasPrefix(token, "PVEAPIToken=") {
		return token
	}
	return "PVEAPIToken=" + token
}

func defaultHTTPClient(insecure bool) *http.Client {
	if !insecure {
		return http.DefaultClient
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
}

func proxmoxSize(cfg config.ProxmoxConfig) string {
	var parts []string
	if cfg.Cores > 0 {
		parts = append(parts, fmt.Sprintf("%d cores", cfg.Cores))
	}
	if cfg.MemoryMB > 0 {
		parts = append(parts, fmt.Sprintf("%d MB", cfg.MemoryMB))
	}
	if len(parts) == 0 {
		return "template"
	}
	return strings.Join(parts, ", ")
}

func proxmoxDescription(cfg config.ProxmoxConfig, plan provider.HostPlan) string {
	if cfg.Description != "" {
		return cfg.Description
	}
	return fmt.Sprintf("Managed by Ship: project=%s environment=%s pool=%s", plan.Project, plan.Environment, plan.Pool)
}

func fullClone(cfg config.ProxmoxConfig) bool {
	if cfg.FullClone == nil {
		return true
	}
	return *cfg.FullClone
}

func startVM(cfg config.ProxmoxConfig) bool {
	if cfg.Start == nil {
		return true
	}
	return *cfg.Start
}

func proxmoxTags(project, environment, pool string, extra []string) []string {
	tags := []string{"ship", "ship-project-" + tagPart(project), "ship-env-" + tagPart(environment), "ship-pool-" + tagPart(pool)}
	tags = append(tags, extra...)
	return uniqueTags(tags)
}

func requiredTags(project, environment string) []string {
	return uniqueTags([]string{"ship", "ship-project-" + tagPart(project), "ship-env-" + tagPart(environment)})
}

func parseTags(tags string) map[string]bool {
	out := map[string]bool{}
	for _, tag := range strings.Split(tags, ";") {
		tag = strings.TrimSpace(tag)
		if tag != "" {
			out[tag] = true
		}
	}
	return out
}

func hasTags(tags map[string]bool, required []string) bool {
	for _, tag := range required {
		if !tags[tag] {
			return false
		}
	}
	return true
}

func poolFromTags(tags map[string]bool) string {
	for tag := range tags {
		if pool := strings.TrimPrefix(tag, "ship-pool-"); pool != tag {
			return pool
		}
	}
	return ""
}

func tagPart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unknown"
	}
	return out
}

func uniqueTags(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = tagPart(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func sortHosts(hosts []provider.Host) {
	sort.SliceStable(hosts, func(i, j int) bool {
		if hosts[i].Pool != hosts[j].Pool {
			return hosts[i].Pool < hosts[j].Pool
		}
		return hosts[i].Name < hosts[j].Name
	})
}

func boolValue(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func pathEscape(value string) string {
	return url.PathEscape(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstNonEmptyEnv(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
