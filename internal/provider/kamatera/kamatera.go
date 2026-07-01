package kamatera

import (
	"bytes"
	"context"
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

const (
	defaultAPIBase             = "https://console.kamatera.com/service"
	defaultBilling             = "monthly"
	defaultTraffic             = "t5000"
	defaultNetwork             = "wan"
	defaultPasswordEnv         = "KAMATERA_SERVER_PASSWORD"
	defaultWaitTimeout         = 10 * time.Minute
	defaultPollInterval        = 5 * time.Second
	defaultShipServerNameLimit = 80
)

type Client struct {
	ClientID       string
	Secret         string
	ServerPassword string
	PasswordEnv    string
	DryRun         bool
	HTTP           *http.Client
	BaseURL        string
	Sleep          func(context.Context, time.Duration) error
}

type ServerPlan = provider.HostPlan

type ServerSummary struct {
	ID         string `json:"id"`
	Datacenter string `json:"datacenter"`
	Name       string `json:"name"`
	Power      string `json:"power"`
}

type Server struct {
	ID         string    `json:"id"`
	Datacenter string    `json:"datacenter"`
	Name       string    `json:"name"`
	CPU        string    `json:"cpu"`
	RAM        int       `json:"ram"`
	Power      string    `json:"power"`
	DiskSizes  []int     `json:"diskSizes"`
	Networks   []Network `json:"networks"`
	Billing    string    `json:"billing"`
	Traffic    string    `json:"traffic"`
}

type Network struct {
	Network string   `json:"network"`
	IPs     []string `json:"ips"`
}

type ReconcileResult = provider.ReconcileResult

func NewFromEnv(dryRun bool, configs ...config.KamateraConfig) Client {
	var cfg config.KamateraConfig
	if len(configs) > 0 {
		cfg = configs[0]
	}
	passwordEnv := strings.TrimSpace(cfg.PasswordEnv)
	if passwordEnv == "" {
		passwordEnv = defaultPasswordEnv
	}
	secret := os.Getenv("KAMATERA_SECRET")
	if secret == "" {
		secret = os.Getenv("KAMATERA_API_SECRET")
	}
	return Client{
		ClientID:       os.Getenv("KAMATERA_CLIENT_ID"),
		Secret:         secret,
		ServerPassword: firstNonEmpty(cfg.Password, os.Getenv(passwordEnv)),
		PasswordEnv:    passwordEnv,
		DryRun:         dryRun,
		HTTP:           http.DefaultClient,
	}
}

func (c Client) Name() string {
	return config.ProviderKamatera
}

func (c Client) CredentialsPresent() bool {
	return strings.TrimSpace(c.ClientID) != "" && strings.TrimSpace(c.Secret) != ""
}

func (c Client) CredentialChecks(lookupEnv func(string) (string, bool)) []provider.CredentialCheck {
	_, clientIDOK := lookupEnv("KAMATERA_CLIENT_ID")
	_, secretOK := lookupEnv("KAMATERA_SECRET")
	_, apiSecretOK := lookupEnv("KAMATERA_API_SECRET")
	_, passwordOK := lookupEnv(firstNonEmpty(c.PasswordEnv, defaultPasswordEnv))
	passwordPresent := passwordOK || strings.TrimSpace(c.ServerPassword) != ""
	return []provider.CredentialCheck{
		{
			Name:           "kamatera client id",
			Present:        clientIDOK,
			Required:       true,
			PresentMessage: "KAMATERA_CLIENT_ID is set",
			MissingMessage: "missing KAMATERA_CLIENT_ID",
		},
		{
			Name:           "kamatera secret",
			Present:        secretOK || apiSecretOK,
			Required:       true,
			PresentMessage: "KAMATERA_SECRET is set",
			MissingMessage: "missing KAMATERA_SECRET or KAMATERA_API_SECRET",
		},
		{
			Name:           "kamatera server password",
			Present:        passwordPresent,
			Required:       true,
			PresentMessage: firstNonEmpty(c.PasswordEnv, defaultPasswordEnv) + " is set",
			MissingMessage: "missing " + firstNonEmpty(c.PasswordEnv, defaultPasswordEnv),
		},
	}
}

func DesiredServers(env config.Environment) []provider.HostPlan {
	return DesiredServersFor("", "", env)
}

func DesiredServersFor(project, environment string, env config.Environment) []provider.HostPlan {
	kamatera := env.Provider.Kamatera
	if kamatera == nil {
		return nil
	}
	return provider.HostPlans(project, environment, env, provider.HostPlanOptions{
		Location: kamatera.Datacenter,
		Size:     kamatera.CPU,
		Image:    kamatera.Image,
	})
}

func (c Client) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.Kamatera == nil {
		return nil, fmt.Errorf("environment %q must define provider.kamatera", environment)
	}
	return DesiredServersFor(project, environment, env), nil
}

func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	if env.Provider.Kamatera == nil {
		return provider.ReconcileResult{}, fmt.Errorf("environment %q must define provider.kamatera", environment)
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
	return provider.ReconcileHosts(ctx, project, environment, desired, reconcileBackend{client: c, kamatera: *env.Provider.Kamatera})
}

type reconcileBackend struct {
	client   Client
	kamatera config.KamateraConfig
}

func (b reconcileBackend) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	return b.client.List(ctx, project, environment)
}

func (b reconcileBackend) Create(ctx context.Context, plan provider.HostPlan) (provider.Host, error) {
	server, err := b.client.CreateServer(ctx, plan, b.kamatera)
	if err != nil {
		return provider.Host{}, err
	}
	return hostFromServer(server, plan.Project, plan.Environment), nil
}

func (c Client) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	servers, err := c.ListServers(ctx, project, environment)
	if err != nil {
		return nil, err
	}
	hosts := make([]provider.Host, 0, len(servers))
	for _, server := range servers {
		hosts = append(hosts, hostFromServer(server, project, environment))
	}
	return hosts, nil
}

func (c Client) Delete(ctx context.Context, host provider.Host) error {
	if strings.TrimSpace(host.ID) == "" {
		return fmt.Errorf("kamatera server id is required")
	}
	return c.TerminateServer(ctx, host.ID)
}

func (c Client) ListServers(ctx context.Context, project, environment string) ([]Server, error) {
	if !c.CredentialsPresent() {
		return nil, fmt.Errorf("KAMATERA_CLIENT_ID and KAMATERA_SECRET are required")
	}
	var summaries []ServerSummary
	if err := c.request(ctx, http.MethodGet, "/servers", nil, &summaries); err != nil {
		return nil, err
	}
	prefix := serverNamePrefix(project, environment)
	var servers []Server
	for _, summary := range summaries {
		if !strings.HasPrefix(summary.Name, prefix) {
			continue
		}
		server, err := c.GetServer(ctx, summary.ID)
		if err != nil {
			return nil, err
		}
		servers = append(servers, server)
	}
	sort.SliceStable(servers, func(i, j int) bool {
		return logicalName(servers[i].Name, project, environment) < logicalName(servers[j].Name, project, environment)
	})
	return servers, nil
}

func (c Client) GetServer(ctx context.Context, id string) (Server, error) {
	if !c.CredentialsPresent() {
		return Server{}, fmt.Errorf("KAMATERA_CLIENT_ID and KAMATERA_SECRET are required")
	}
	if strings.TrimSpace(id) == "" {
		return Server{}, fmt.Errorf("kamatera server id is required")
	}
	var server Server
	if err := c.request(ctx, http.MethodGet, "/server/"+url.PathEscape(id), nil, &server); err != nil {
		return Server{}, err
	}
	return server, nil
}

func (c Client) CreateServer(ctx context.Context, plan provider.HostPlan, kamatera config.KamateraConfig) (Server, error) {
	if !c.CredentialsPresent() {
		return Server{}, fmt.Errorf("KAMATERA_CLIENT_ID and KAMATERA_SECRET are required")
	}
	if strings.TrimSpace(c.ServerPassword) == "" {
		return Server{}, fmt.Errorf("%s is required", firstNonEmpty(c.PasswordEnv, defaultPasswordEnv))
	}
	kamatera = withPlanDefaults(plan, kamatera)
	body := createServerBody(plan, kamatera, c.ServerPassword)
	externalName := body["name"].(string)
	if c.DryRun {
		return Server{ID: externalName, Name: externalName, Datacenter: plan.Location, CPU: plan.Size}, nil
	}
	if err := c.request(ctx, http.MethodPost, "/server", body, nil); err != nil {
		return Server{}, err
	}
	return c.WaitForServerName(ctx, externalName, kamatera)
}

func (c Client) WaitForServerName(ctx context.Context, name string, kamatera config.KamateraConfig) (Server, error) {
	deadline := time.Now().Add(waitTimeout(kamatera))
	var lastErr error
	for {
		server, ok, err := c.findServerByExternalName(ctx, name)
		if err != nil {
			lastErr = err
		} else if ok {
			return server, nil
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return Server{}, fmt.Errorf("wait for Kamatera server %q: %w", name, lastErr)
			}
			return Server{}, fmt.Errorf("timed out waiting for Kamatera server %q", name)
		}
		if err := c.sleep(ctx, pollInterval(kamatera)); err != nil {
			return Server{}, err
		}
	}
}

func (c Client) findServerByExternalName(ctx context.Context, name string) (Server, bool, error) {
	var summaries []ServerSummary
	if err := c.request(ctx, http.MethodGet, "/servers", nil, &summaries); err != nil {
		return Server{}, false, err
	}
	for _, summary := range summaries {
		if summary.Name != name {
			continue
		}
		server, err := c.GetServer(ctx, summary.ID)
		if err != nil {
			return Server{}, false, err
		}
		return server, true, nil
	}
	return Server{}, false, nil
}

func (c Client) TerminateServer(ctx context.Context, id string) error {
	if !c.CredentialsPresent() {
		return fmt.Errorf("KAMATERA_CLIENT_ID and KAMATERA_SECRET are required")
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("kamatera server id is required")
	}
	if c.DryRun {
		return nil
	}
	body := map[string]string{"confirm": "1", "force": "1"}
	return c.request(ctx, http.MethodDelete, "/server/"+url.PathEscape(id)+"/terminate", body, nil)
}

func (c Client) request(ctx context.Context, method, path string, body any, out any) error {
	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = defaultAPIBase
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("clientId", c.ClientID)
	req.Header.Set("secret", c.Secret)
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
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
		return fmt.Errorf("kamatera API %s %s failed: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out == nil || len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode Kamatera response: %w", err)
	}
	return nil
}

func (c Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func createServerBody(plan provider.HostPlan, kamatera config.KamateraConfig, password string) map[string]any {
	body := map[string]any{
		"disk_src_0":     plan.Image,
		"datacenter":     plan.Location,
		"name":           externalServerName(plan),
		"cpu":            plan.Size,
		"ram":            kamatera.RAMMB,
		"password":       password,
		"billing":        firstNonEmpty(kamatera.Billing, defaultBilling),
		"traffic":        firstNonEmpty(kamatera.Traffic, defaultTraffic),
		"disk_size_0":    kamatera.DiskGB,
		"network_name_0": firstNonEmpty(kamatera.Network, defaultNetwork),
	}
	if kamatera.Managed != nil {
		body["managed"] = *kamatera.Managed
	}
	if kamatera.Backup != nil {
		body["backup"] = *kamatera.Backup
	}
	if kamatera.Power != nil {
		body["power"] = *kamatera.Power
	} else {
		body["power"] = true
	}
	if kamatera.NetworkIP != "" {
		body["network_ip_0"] = kamatera.NetworkIP
	}
	if kamatera.NetworkBits > 0 {
		body["network_bits_0"] = kamatera.NetworkBits
	}
	if kamatera.SSHPublicKey != "" {
		body["selectedSSHKeyValue"] = kamatera.SSHPublicKey
	}
	return body
}

func withPlanDefaults(plan provider.HostPlan, kamatera config.KamateraConfig) config.KamateraConfig {
	if plan.Location != "" {
		kamatera.Datacenter = plan.Location
	}
	if plan.Size != "" {
		kamatera.CPU = plan.Size
	}
	if plan.Image != "" {
		kamatera.Image = plan.Image
	}
	return kamatera
}

func hostFromServer(server Server, project, environment string) provider.Host {
	name := logicalName(server.Name, project, environment)
	return provider.Host{
		ID:            server.ID,
		Name:          name,
		Pool:          poolFromName(name),
		PublicAddress: publicAddress(server.Networks),
		Labels:        provider.ShipLabels(project, environment, poolFromName(name)),
	}
}

func publicAddress(networks []Network) string {
	for _, network := range networks {
		if !strings.HasPrefix(strings.ToLower(network.Network), "wan") {
			continue
		}
		if ip := firstIPv4(network.IPs); ip != "" {
			return ip
		}
	}
	for _, network := range networks {
		if ip := firstIPv4(network.IPs); ip != "" {
			return ip
		}
	}
	for _, network := range networks {
		for _, ip := range network.IPs {
			if strings.TrimSpace(ip) != "" {
				return strings.TrimSpace(ip)
			}
		}
	}
	return ""
}

func firstIPv4(values []string) string {
	for _, value := range values {
		ip := net.ParseIP(strings.TrimSpace(value))
		if ip != nil && ip.To4() != nil {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func externalServerName(plan provider.HostPlan) string {
	return truncateName(serverNamePrefix(plan.Project, plan.Environment)+sanitizeHostName(plan.Name), defaultShipServerNameLimit)
}

func serverNamePrefix(project, environment string) string {
	return "ship-" + sanitizeName(project) + "-" + sanitizeName(environment) + "-"
}

func logicalName(external, project, environment string) string {
	prefix := serverNamePrefix(project, environment)
	return strings.TrimPrefix(external, prefix)
}

func sanitizeName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	dash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "default"
	}
	return out
}

func sanitizeHostName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	dash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.'
		if ok {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "host"
	}
	return out
}

func truncateName(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return strings.TrimRight(value[:limit], "-")
}

func poolFromName(name string) string {
	if i := strings.LastIndex(name, "-"); i > 0 {
		if _, err := strconv.Atoi(name[i+1:]); err == nil {
			return name[:i]
		}
	}
	return name
}

func waitTimeout(kamatera config.KamateraConfig) time.Duration {
	if kamatera.WaitTimeoutSeconds > 0 {
		return time.Duration(kamatera.WaitTimeoutSeconds) * time.Second
	}
	return defaultWaitTimeout
}

func pollInterval(kamatera config.KamateraConfig) time.Duration {
	if kamatera.PollIntervalSeconds > 0 {
		return time.Duration(kamatera.PollIntervalSeconds) * time.Second
	}
	return defaultPollInterval
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
