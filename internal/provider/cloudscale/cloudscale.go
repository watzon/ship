package cloudscale

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

const (
	defaultAPIBase    = "https://api.cloudscale.ch/v1"
	defaultVolumeSize = 10

	LabelManagedBy   = provider.LabelManagedBy
	LabelProject     = provider.LabelProject
	LabelEnvironment = provider.LabelEnvironment
	LabelPool        = provider.LabelPool
)

type Client struct {
	Token   string
	DryRun  bool
	HTTP    *http.Client
	BaseURL string
}

type ServerPlan = provider.HostPlan

type Server struct {
	HREF       string            `json:"href"`
	UUID       string            `json:"uuid"`
	Name       string            `json:"name"`
	Status     string            `json:"status"`
	Tags       map[string]string `json:"tags"`
	Zone       SlugRef           `json:"zone"`
	Flavor     SlugRef           `json:"flavor"`
	Image      SlugRef           `json:"image"`
	Interfaces []Interface       `json:"interfaces"`
}

type ServerGroup struct {
	HREF    string            `json:"href"`
	UUID    string            `json:"uuid"`
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	Zone    SlugRef           `json:"zone"`
	Servers []ServerRef       `json:"servers"`
	Tags    map[string]string `json:"tags"`
}

type ServerRef struct {
	HREF string `json:"href"`
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

type SlugRef struct {
	Slug string `json:"slug,omitempty"`
	Name string `json:"name,omitempty"`
}

type Interface struct {
	Type      string         `json:"type,omitempty"`
	Network   NetworkRef     `json:"network,omitempty"`
	Addresses []Address      `json:"addresses,omitempty"`
	NoAddress bool           `json:"-"`
	Raw       map[string]any `json:"-"`
}

type NetworkRef struct {
	UUID string `json:"uuid,omitempty"`
	Name string `json:"name,omitempty"`
}

type Address struct {
	Version int    `json:"version,omitempty"`
	Address string `json:"address,omitempty"`
	Subnet  string `json:"subnet,omitempty"`
}

type ReconcileResult = provider.ReconcileResult

func NewFromEnv(dryRun bool, configs ...config.CloudscaleConfig) Client {
	return Client{
		Token:  os.Getenv("CLOUDSCALE_API_TOKEN"),
		DryRun: dryRun,
		HTTP:   http.DefaultClient,
	}
}

func (c Client) Name() string {
	return config.ProviderCloudscale
}

func (c Client) TokenPresent() bool {
	return strings.TrimSpace(c.Token) != ""
}

func (c Client) CredentialChecks(lookupEnv func(string) (string, bool)) []provider.CredentialCheck {
	_, ok := lookupEnv("CLOUDSCALE_API_TOKEN")
	return []provider.CredentialCheck{{
		Name:           "cloudscale token",
		Present:        ok,
		Required:       true,
		PresentMessage: "CLOUDSCALE_API_TOKEN is set",
		MissingMessage: "missing CLOUDSCALE_API_TOKEN",
	}}
}

func DesiredServers(env config.Environment) []provider.HostPlan {
	return DesiredServersFor("", "", env)
}

func DesiredServersFor(project, environment string, env config.Environment) []provider.HostPlan {
	cloudscale := env.Provider.Cloudscale
	if cloudscale == nil {
		return nil
	}
	return provider.HostPlans(project, environment, env, provider.HostPlanOptions{
		Location: cloudscale.Zone,
		Size:     cloudscale.Flavor,
		Image:    cloudscale.Image,
		UserData: cloudscale.UserData,
	})
}

func (c Client) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.Cloudscale == nil {
		return nil, fmt.Errorf("environment %q must define provider.cloudscale", environment)
	}
	return DesiredServersFor(project, environment, env), nil
}

func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	if env.Provider.Cloudscale == nil {
		return provider.ReconcileResult{}, fmt.Errorf("environment %q must define provider.cloudscale", environment)
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

// reconcileBackendFor resolves the cloudscale server groups and builds the
// reconcile backend shared by Reconcile and CreateHost so both create servers
// identically.
func (c Client) reconcileBackendFor(ctx context.Context, project, environment string, env config.Environment) (reconcileBackend, error) {
	cloudscale := *env.Provider.Cloudscale
	serverGroups := append([]string{}, cloudscale.ServerGroups...)
	if cloudscale.ServerGroup.ManagedValue(false) {
		group, err := c.EnsureServerGroup(ctx, project, environment, cloudscale)
		if err != nil {
			return reconcileBackend{}, err
		}
		serverGroups = append(serverGroups, group.UUID)
	} else if cloudscale.ServerGroup.UUID != "" {
		serverGroups = append(serverGroups, cloudscale.ServerGroup.UUID)
	}
	return reconcileBackend{
		client:       c,
		cloudscale:   cloudscale,
		serverGroups: serverGroups,
	}, nil
}

// CreateHost provisions a single server using the backend Reconcile would
// build, so `ship migrate` can add a replacement alongside the existing one.
func (c Client) CreateHost(ctx context.Context, project, environment string, env config.Environment, plan provider.HostPlan) (provider.Host, error) {
	if env.Provider.Cloudscale == nil {
		return provider.Host{}, fmt.Errorf("environment %q must define provider.cloudscale", environment)
	}
	backend, err := c.reconcileBackendFor(ctx, project, environment, env)
	if err != nil {
		return provider.Host{}, err
	}
	return backend.Create(ctx, plan)
}

var _ provider.HostCreator = Client{}

type reconcileBackend struct {
	client       Client
	cloudscale   config.CloudscaleConfig
	serverGroups []string
}

func (b reconcileBackend) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	return b.client.List(ctx, project, environment)
}

func (b reconcileBackend) Create(ctx context.Context, plan provider.HostPlan) (provider.Host, error) {
	server, err := b.client.CreateServer(ctx, plan, b.cloudscale, b.serverGroups)
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
		return fmt.Errorf("cloudscale server uuid is required")
	}
	return c.DeleteServer(ctx, host.ID)
}

func (c Client) ListServers(ctx context.Context, project, environment string) ([]Server, error) {
	if !c.TokenPresent() {
		return nil, fmt.Errorf("CLOUDSCALE_API_TOKEN is required")
	}
	values := url.Values{}
	values.Set("tag:"+LabelManagedBy, "ship")
	values.Set("tag:"+LabelProject, project)
	values.Set("tag:"+LabelEnvironment, environment)
	var servers []Server
	if err := c.request(ctx, http.MethodGet, "/servers?"+values.Encode(), nil, &servers); err != nil {
		return nil, err
	}
	filtered := servers[:0]
	for _, server := range servers {
		if serverMatches(server, project, environment) {
			filtered = append(filtered, server)
		}
	}
	return filtered, nil
}

func (c Client) CreateServer(ctx context.Context, plan provider.HostPlan, cloudscale config.CloudscaleConfig, serverGroups []string) (Server, error) {
	if !c.TokenPresent() {
		return Server{}, fmt.Errorf("CLOUDSCALE_API_TOKEN is required")
	}
	cloudscale = withPlanDefaults(plan, cloudscale)
	body := createServerBody(plan, cloudscale, serverGroups)
	if c.DryRun {
		return Server{Name: plan.Name, Tags: plan.Labels}, nil
	}
	var server Server
	if err := c.request(ctx, http.MethodPost, "/servers", body, &server); err != nil {
		return Server{}, err
	}
	return server, nil
}

func (c Client) DeleteServer(ctx context.Context, uuid string) error {
	if !c.TokenPresent() {
		return fmt.Errorf("CLOUDSCALE_API_TOKEN is required")
	}
	if strings.TrimSpace(uuid) == "" {
		return fmt.Errorf("cloudscale server uuid is required")
	}
	if c.DryRun {
		return nil
	}
	return c.request(ctx, http.MethodDelete, "/servers/"+url.PathEscape(uuid), nil, nil)
}

func (c Client) EnsureServerGroup(ctx context.Context, project, environment string, cloudscale config.CloudscaleConfig) (ServerGroup, error) {
	if !c.TokenPresent() {
		return ServerGroup{}, fmt.Errorf("CLOUDSCALE_API_TOKEN is required")
	}
	name := cloudscale.ServerGroup.Name
	if name == "" {
		name = "ship-" + project + "-" + environment
	}
	groups, err := c.ListServerGroups(ctx, project, environment)
	if err != nil {
		return ServerGroup{}, err
	}
	for _, group := range groups {
		if group.Name == name {
			return group, nil
		}
	}
	return c.CreateServerGroup(ctx, name, cloudscale.Zone, provider.ShipLabels(project, environment, "server-group"))
}

func (c Client) ListServerGroups(ctx context.Context, project, environment string) ([]ServerGroup, error) {
	values := url.Values{}
	values.Set("tag:"+LabelManagedBy, "ship")
	values.Set("tag:"+LabelProject, project)
	values.Set("tag:"+LabelEnvironment, environment)
	var groups []ServerGroup
	if err := c.request(ctx, http.MethodGet, "/server-groups?"+values.Encode(), nil, &groups); err != nil {
		return nil, err
	}
	filtered := groups[:0]
	for _, group := range groups {
		if group.Tags[LabelManagedBy] == "ship" &&
			group.Tags[LabelProject] == project &&
			group.Tags[LabelEnvironment] == environment {
			filtered = append(filtered, group)
		}
	}
	return filtered, nil
}

func (c Client) CreateServerGroup(ctx context.Context, name, zone string, tags map[string]string) (ServerGroup, error) {
	body := map[string]any{
		"name": name,
		"type": "anti-affinity",
		"tags": tags,
	}
	if zone != "" {
		body["zone"] = zone
	}
	var group ServerGroup
	if err := c.request(ctx, http.MethodPost, "/server-groups", body, &group); err != nil {
		return ServerGroup{}, err
	}
	return group, nil
}

func (c Client) request(ctx context.Context, method, path string, body any, out any) error {
	bodyBytes, err := jsonBody(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase()+path, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
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
		message := strings.TrimSpace(string(data))
		if message == "" {
			message = resp.Status
		}
		return fmt.Errorf("cloudscale %s %s failed: %s", method, req.URL.Path, message)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode cloudscale response: %w", err)
	}
	return nil
}

func (c Client) apiBase() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return defaultAPIBase
}

func jsonBody(body any) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func createServerBody(plan provider.HostPlan, cloudscale config.CloudscaleConfig, serverGroups []string) map[string]any {
	body := map[string]any{
		"name":           plan.Name,
		"zone":           cloudscale.Zone,
		"flavor":         plan.Size,
		"image":          plan.Image,
		"ssh_keys":       cloudscale.SSHKeys,
		"volume_size_gb": cloudscale.EffectiveVolumeSizeGB(defaultVolumeSize),
		"tags":           plan.Labels,
	}
	if plan.UserData != "" {
		body["user_data"] = plan.UserData
	}
	if cloudscale.UserDataHandling != "" {
		body["user_data_handling"] = cloudscale.UserDataHandling
	}
	if cloudscale.Password != "" {
		body["password"] = cloudscale.Password
	}
	if cloudscale.UsePublicNetwork != nil {
		body["use_public_network"] = *cloudscale.UsePublicNetwork
	}
	if cloudscale.UsePrivateNetwork != nil {
		body["use_private_network"] = *cloudscale.UsePrivateNetwork
	}
	if cloudscale.UseIPv6 != nil {
		body["use_ipv6"] = *cloudscale.UseIPv6
	}
	if cloudscale.BulkVolumeSizeGB > 0 {
		body["bulk_volume_size_gb"] = cloudscale.BulkVolumeSizeGB
	}
	if len(cloudscale.Volumes) > 0 {
		body["volumes"] = cloudscale.Volumes
	}
	if len(cloudscale.Interfaces) > 0 {
		body["interfaces"] = cloudscale.Interfaces
	}
	if len(serverGroups) > 0 {
		body["server_groups"] = serverGroups
	}
	if cloudscale.AntiAffinityWith != "" {
		body["anti_affinity_with"] = cloudscale.AntiAffinityWith
	}
	return body
}

func withPlanDefaults(plan provider.HostPlan, cloudscale config.CloudscaleConfig) config.CloudscaleConfig {
	if plan.Location != "" {
		cloudscale.Zone = plan.Location
	}
	if plan.Size != "" {
		cloudscale.Flavor = plan.Size
	}
	if plan.Image != "" {
		cloudscale.Image = plan.Image
	}
	return cloudscale
}

func serverMatches(server Server, project, environment string) bool {
	return server.Tags[LabelManagedBy] == "ship" &&
		server.Tags[LabelProject] == project &&
		server.Tags[LabelEnvironment] == environment
}

func hostFromServer(server Server) provider.Host {
	return provider.Host{
		ID:            server.UUID,
		Name:          server.Name,
		Pool:          server.Tags[LabelPool],
		PublicAddress: publicAddress(server.Interfaces),
		Labels:        server.Tags,
	}
}

func publicAddress(interfaces []Interface) string {
	var ipv6 string
	for _, iface := range interfaces {
		if iface.Type != "public" {
			continue
		}
		for _, address := range iface.Addresses {
			if address.Address == "" {
				continue
			}
			if address.Version == 4 {
				return address.Address
			}
			if address.Version == 6 && ipv6 == "" {
				ipv6 = address.Address
			}
		}
	}
	return ipv6
}
