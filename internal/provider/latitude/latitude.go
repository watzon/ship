package latitude

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

const (
	defaultAPIBase = "https://api.latitude.sh"

	LabelManagedBy   = provider.LabelManagedBy
	LabelProject     = provider.LabelProject
	LabelEnvironment = provider.LabelEnvironment
	LabelPool        = provider.LabelPool
)

var nonHostnameChars = regexp.MustCompile(`[^a-z0-9-]+`)

type Client struct {
	Token        string
	DeleteReason string
	DryRun       bool
	HTTP         *http.Client
	BaseURL      string
}

type ServerPlan = provider.HostPlan

type Server struct {
	ID         string           `json:"id"`
	Type       string           `json:"type"`
	Attributes ServerAttributes `json:"attributes"`
}

type ServerAttributes struct {
	Hostname    string        `json:"hostname"`
	Label       string        `json:"label"`
	Status      string        `json:"status"`
	PrimaryIPv4 string        `json:"primary_ipv4"`
	PrimaryIPv6 string        `json:"primary_ipv6"`
	Tags        []LatitudeTag `json:"tags"`
	Project     ProjectRef    `json:"project"`
	Plan        PlanRef       `json:"plan"`
	Site        SiteRef       `json:"site"`
}

type LatitudeTag struct {
	ID         string        `json:"id"`
	Type       string        `json:"type"`
	Attributes TagAttributes `json:"attributes"`
}

type TagAttributes struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
	Color       string `json:"color"`
}

type ProjectRef struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type PlanRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type SiteRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type UserData struct {
	ID         string             `json:"id"`
	Type       string             `json:"type"`
	Attributes UserDataAttributes `json:"attributes"`
}

type UserDataAttributes struct {
	Description    string     `json:"description"`
	Content        string     `json:"content"`
	DecodedContent string     `json:"decoded_content"`
	Project        ProjectRef `json:"project"`
}

type Firewall struct {
	ID         string             `json:"id"`
	Type       string             `json:"type"`
	Attributes FirewallAttributes `json:"attributes"`
}

type FirewallAttributes struct {
	Name    string         `json:"name"`
	Project ProjectRef     `json:"project"`
	Rules   []FirewallRule `json:"rules"`
}

type FirewallRule struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Port        string `json:"port"`
	Protocol    string `json:"protocol"`
	Description string `json:"description,omitempty"`
	Default     bool   `json:"default,omitempty"`
}

type FirewallAssignment struct {
	ID         string                       `json:"id"`
	Type       string                       `json:"type"`
	Attributes FirewallAssignmentAttributes `json:"attributes"`
}

type FirewallAssignmentAttributes struct {
	Server     FirewallAssignmentServer `json:"server"`
	FirewallID string                   `json:"firewall_id"`
}

type FirewallAssignmentServer struct {
	ID          string `json:"id"`
	Hostname    string `json:"hostname"`
	PrimaryIPv4 string `json:"primary_ipv4"`
}

type ReconcileResult = provider.ReconcileResult

func NewFromEnv(dryRun bool, configs ...config.LatitudeConfig) Client {
	cfg := config.LatitudeConfig{}
	if len(configs) > 0 {
		cfg = configs[0]
	}
	token := os.Getenv("LATITUDE_API_TOKEN")
	if token == "" {
		token = os.Getenv("LATITUDESH_BEARER")
	}
	return Client{Token: token, DeleteReason: cfg.DeleteReason, DryRun: dryRun, HTTP: http.DefaultClient}
}

func (c Client) Name() string {
	return config.ProviderLatitude
}

func (c Client) TokenPresent() bool {
	return strings.TrimSpace(c.Token) != ""
}

func (c Client) CredentialChecks(lookupEnv func(string) (string, bool)) []provider.CredentialCheck {
	_, tokenOK := lookupEnv("LATITUDE_API_TOKEN")
	_, bearerOK := lookupEnv("LATITUDESH_BEARER")
	return []provider.CredentialCheck{{
		Name:           "latitude token",
		Present:        tokenOK || bearerOK,
		Required:       true,
		PresentMessage: "LATITUDE_API_TOKEN or LATITUDESH_BEARER is set",
		MissingMessage: "missing LATITUDE_API_TOKEN or LATITUDESH_BEARER",
	}}
}

func DesiredServers(env config.Environment) []provider.HostPlan {
	return DesiredServersFor("", "", env)
}

func DesiredServersFor(project, environment string, env config.Environment) []provider.HostPlan {
	latitude := env.Provider.Latitude
	if latitude == nil {
		return nil
	}
	return provider.HostPlans(project, environment, env, provider.HostPlanOptions{
		Location: latitude.Site,
		Size:     latitude.Plan,
		Image:    latitude.OperatingSystem,
		UserData: latitude.UserData,
	})
}

func (c Client) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.Latitude == nil {
		return nil, fmt.Errorf("environment %q must define provider.latitude", environment)
	}
	return DesiredServersFor(project, environment, env), nil
}

func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	if env.Provider.Latitude == nil {
		return provider.ReconcileResult{}, fmt.Errorf("environment %q must define provider.latitude", environment)
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
	latitude := *env.Provider.Latitude
	firewallID := latitude.Firewall.ID
	if latitude.Firewall.ManagedValue(true) {
		firewall, err := c.EnsureFirewall(ctx, project, environment, latitude)
		if err != nil {
			return provider.ReconcileResult{}, err
		}
		firewallID = firewall.ID
	}
	result, err = provider.ReconcileHosts(ctx, project, environment, desired, backend)
	if err != nil {
		return provider.ReconcileResult{}, err
	}
	if firewallID != "" {
		hosts := append(append([]provider.Host{}, result.Existing...), result.Created...)
		if err := c.EnsureFirewallAssignments(ctx, firewallID, hosts); err != nil {
			return provider.ReconcileResult{}, err
		}
	}
	return result, nil
}

// reconcileBackendFor resolves the ownership tags and user-data and builds the
// reconcile backend shared by Reconcile and CreateHost so both create servers
// identically.
func (c Client) reconcileBackendFor(ctx context.Context, project, environment string, env config.Environment) (reconcileBackend, error) {
	latitude := *env.Provider.Latitude
	ownershipTagIDs, err := c.EnsureTags(ctx, provider.ShipLabels(project, environment, ""))
	if err != nil {
		return reconcileBackend{}, err
	}
	userDataID := latitude.UserDataID
	if userDataID == "" && latitude.UserData != "" {
		userData, err := c.EnsureUserData(ctx, project, environment, latitude.Project, latitude.UserData)
		if err != nil {
			return reconcileBackend{}, err
		}
		userDataID = userData.ID
	}
	return reconcileBackend{
		client:          c,
		latitude:        latitude,
		userDataID:      userDataID,
		ownershipTagIDs: ownershipTagIDs,
	}, nil
}

// CreateHost provisions a single server using the backend Reconcile would
// build, so `ship migrate` can add a replacement alongside the existing one.
// It also applies the managed firewall assignment Reconcile would perform.
func (c Client) CreateHost(ctx context.Context, project, environment string, env config.Environment, plan provider.HostPlan) (provider.Host, error) {
	if env.Provider.Latitude == nil {
		return provider.Host{}, fmt.Errorf("environment %q must define provider.latitude", environment)
	}
	backend, err := c.reconcileBackendFor(ctx, project, environment, env)
	if err != nil {
		return provider.Host{}, err
	}
	host, err := backend.Create(ctx, plan)
	if err != nil {
		return provider.Host{}, err
	}
	latitude := *env.Provider.Latitude
	firewallID := latitude.Firewall.ID
	if latitude.Firewall.ManagedValue(true) {
		firewall, err := c.EnsureFirewall(ctx, project, environment, latitude)
		if err != nil {
			return provider.Host{}, err
		}
		firewallID = firewall.ID
	}
	if firewallID != "" {
		if err := c.EnsureFirewallAssignments(ctx, firewallID, []provider.Host{host}); err != nil {
			return provider.Host{}, err
		}
	}
	return host, nil
}

var _ provider.HostCreator = Client{}

type reconcileBackend struct {
	client          Client
	latitude        config.LatitudeConfig
	userDataID      string
	ownershipTagIDs []string
}

func (b reconcileBackend) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	servers, err := b.client.ListServers(ctx, project, environment, b.latitude.Project, b.ownershipTagIDs)
	if err != nil {
		return nil, err
	}
	hosts := make([]provider.Host, 0, len(servers))
	for _, server := range servers {
		hosts = append(hosts, hostFromServer(server, project, environment))
	}
	return hosts, nil
}

func (b reconcileBackend) Create(ctx context.Context, plan provider.HostPlan) (provider.Host, error) {
	server, err := b.client.CreateServer(ctx, plan, b.latitude, b.userDataID)
	if err != nil {
		return provider.Host{}, err
	}
	tagIDs, err := b.client.EnsureTags(ctx, plan.Labels)
	if err != nil {
		return provider.Host{}, err
	}
	if len(tagIDs) > 0 {
		server, err = b.client.UpdateServerTags(ctx, server.ID, mergeTagIDs(tagIDs, tagIDsForServer(server)))
		if err != nil {
			return provider.Host{}, err
		}
	}
	return hostFromServer(server, plan.Project, plan.Environment), nil
}

func (c Client) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	tagIDs, err := c.FindTagIDs(ctx, provider.ShipLabels(project, environment, ""))
	if err != nil {
		return nil, err
	}
	servers, err := c.ListServers(ctx, project, environment, "", tagIDs)
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
		return fmt.Errorf("latitude server id is required")
	}
	return c.DeleteServer(ctx, host.ID, c.DeleteReason)
}

func (c Client) ListServers(ctx context.Context, project, environment, latitudeProject string, tagIDs []string) ([]Server, error) {
	if !c.TokenPresent() {
		return nil, fmt.Errorf("LATITUDE_API_TOKEN or LATITUDESH_BEARER is required")
	}
	prefix := ownershipPrefix(project, environment)
	page := 1
	var servers []Server
	for {
		values := url.Values{}
		values.Set("page[size]", "100")
		values.Set("page[number]", strconv.Itoa(page))
		if latitudeProject != "" {
			values.Set("filter[project]", latitudeProject)
		}
		if len(tagIDs) > 0 {
			values.Set("filter[tags]", strings.Join(tagIDs, ","))
		}
		var out serversResponse
		if err := c.request(ctx, http.MethodGet, "/servers?"+values.Encode(), nil, &out); err != nil {
			return nil, err
		}
		for _, server := range out.Data {
			if len(tagIDs) > 0 {
				if serverHasAllTagIDs(server, tagIDs) || strings.HasPrefix(server.Attributes.Hostname, prefix) {
					servers = append(servers, server)
				}
				continue
			}
			if strings.HasPrefix(server.Attributes.Hostname, prefix) {
				servers = append(servers, server)
			}
		}
		if out.Meta.CurrentPage == 0 || out.Meta.TotalPages == 0 || out.Meta.CurrentPage >= out.Meta.TotalPages {
			break
		}
		page = out.Meta.CurrentPage + 1
	}
	return servers, nil
}

func (c Client) CreateServer(ctx context.Context, plan provider.HostPlan, latitude config.LatitudeConfig, userDataID string) (Server, error) {
	if !c.TokenPresent() {
		return Server{}, fmt.Errorf("LATITUDE_API_TOKEN or LATITUDESH_BEARER is required")
	}
	latitude = withPlanDefaults(plan, latitude)
	body := createServerBody(plan, latitude, userDataID)
	if c.DryRun {
		return Server{ID: "dry-run", Attributes: ServerAttributes{Hostname: cloudHostname(plan), PrimaryIPv4: "0.0.0.0"}}, nil
	}
	var out serverResponse
	if err := c.request(ctx, http.MethodPost, "/servers", body, &out); err != nil {
		return Server{}, err
	}
	return out.Data, nil
}

func (c Client) DeleteServer(ctx context.Context, id, reason string) error {
	if !c.TokenPresent() {
		return fmt.Errorf("LATITUDE_API_TOKEN or LATITUDESH_BEARER is required")
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("latitude server id is required")
	}
	path := "/servers/" + url.PathEscape(id)
	if reason != "" {
		values := url.Values{}
		values.Set("reason", reason)
		path += "?" + values.Encode()
	}
	if c.DryRun {
		return nil
	}
	return c.request(ctx, http.MethodDelete, path, nil, nil)
}

func (c Client) UpdateServerTags(ctx context.Context, id string, tagIDs []string) (Server, error) {
	if strings.TrimSpace(id) == "" {
		return Server{}, fmt.Errorf("latitude server id is required")
	}
	attrs := map[string]any{"tags": tagIDs}
	body := map[string]any{
		"data": map[string]any{
			"id":         id,
			"type":       "servers",
			"attributes": attrs,
		},
	}
	var out serverResponse
	if err := c.request(ctx, http.MethodPatch, "/servers/"+url.PathEscape(id), body, &out); err != nil {
		return Server{}, err
	}
	return out.Data, nil
}

func (c Client) EnsureTags(ctx context.Context, labels map[string]string) ([]string, error) {
	existing, err := c.ListTags(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]LatitudeTag{}
	for _, tag := range existing {
		byName[tag.Attributes.Name] = tag
	}
	names := tagNamesForLabels(labels)
	ids := make([]string, 0, len(names))
	for _, name := range names {
		if tag, ok := byName[name]; ok {
			ids = append(ids, tag.ID)
			continue
		}
		tag, err := c.CreateTag(ctx, name, "Managed by Ship")
		if err != nil {
			return nil, err
		}
		ids = append(ids, tag.ID)
	}
	return ids, nil
}

func (c Client) FindTagIDs(ctx context.Context, labels map[string]string) ([]string, error) {
	existing, err := c.ListTags(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]string{}
	for _, tag := range existing {
		byName[tag.Attributes.Name] = tag.ID
	}
	names := tagNamesForLabels(labels)
	ids := make([]string, 0, len(names))
	for _, name := range names {
		id := byName[name]
		if id == "" {
			return nil, nil
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (c Client) ListTags(ctx context.Context) ([]LatitudeTag, error) {
	if !c.TokenPresent() {
		return nil, fmt.Errorf("LATITUDE_API_TOKEN or LATITUDESH_BEARER is required")
	}
	var out tagsResponse
	if err := c.request(ctx, http.MethodGet, "/tags", nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

func (c Client) CreateTag(ctx context.Context, name, description string) (LatitudeTag, error) {
	attrs := map[string]any{
		"name":  name,
		"color": "#2f6fed",
	}
	if description != "" {
		attrs["description"] = description
	}
	body := jsonAPIRequest("tags", attrs)
	var out tagResponse
	if err := c.request(ctx, http.MethodPost, "/tags", body, &out); err != nil {
		return LatitudeTag{}, err
	}
	return out.Data, nil
}

func (c Client) EnsureUserData(ctx context.Context, project, environment, latitudeProject, content string) (UserData, error) {
	description := userDataDescription(project, environment, content)
	userData, err := c.ListUserData(ctx, latitudeProject)
	if err != nil {
		return UserData{}, err
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	for _, item := range userData {
		if item.Attributes.Description == description && item.Attributes.Content == encoded {
			return item, nil
		}
	}
	return c.CreateUserData(ctx, latitudeProject, description, content)
}

func (c Client) ListUserData(ctx context.Context, latitudeProject string) ([]UserData, error) {
	values := url.Values{}
	values.Set("page[size]", "100")
	if latitudeProject != "" {
		values.Set("filter[project]", latitudeProject)
	}
	var out userDataListResponse
	if err := c.request(ctx, http.MethodGet, "/user_data?"+values.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

func (c Client) CreateUserData(ctx context.Context, latitudeProject, description, content string) (UserData, error) {
	attrs := map[string]any{
		"description": description,
		"content":     base64.StdEncoding.EncodeToString([]byte(content)),
	}
	if latitudeProject != "" {
		attrs["project"] = latitudeProject
	}
	body := jsonAPIRequest("user_data", attrs)
	var out userDataResponse
	if err := c.request(ctx, http.MethodPost, "/user_data", body, &out); err != nil {
		return UserData{}, err
	}
	return out.Data, nil
}

func (c Client) EnsureFirewall(ctx context.Context, project, environment string, latitude config.LatitudeConfig) (Firewall, error) {
	if !c.TokenPresent() {
		return Firewall{}, fmt.Errorf("LATITUDE_API_TOKEN or LATITUDESH_BEARER is required")
	}
	name := latitude.Firewall.Name
	if name == "" {
		name = firewallName(project, environment)
	}
	if latitude.Firewall.ID != "" && !latitude.Firewall.ManagedValue(true) {
		return Firewall{ID: latitude.Firewall.ID, Attributes: FirewallAttributes{Name: name}}, nil
	}
	rules := firewallRules(latitude)
	firewalls, err := c.ListFirewalls(ctx, latitude.Project)
	if err != nil {
		return Firewall{}, err
	}
	for _, firewall := range firewalls {
		if firewall.Attributes.Name != name {
			continue
		}
		if firewallHasRules(firewall, rules) {
			return firewall, nil
		}
		return c.UpdateFirewall(ctx, firewall.ID, name, rules)
	}
	return c.CreateFirewall(ctx, latitude.Project, name, rules)
}

func (c Client) ListFirewalls(ctx context.Context, latitudeProject string) ([]Firewall, error) {
	values := url.Values{}
	values.Set("page[size]", "100")
	if latitudeProject != "" {
		values.Set("filter[project]", latitudeProject)
	}
	page := 1
	var firewalls []Firewall
	for {
		values.Set("page[number]", strconv.Itoa(page))
		var out firewallsResponse
		if err := c.request(ctx, http.MethodGet, "/firewalls?"+values.Encode(), nil, &out); err != nil {
			return nil, err
		}
		firewalls = append(firewalls, out.Data...)
		if out.Meta.CurrentPage == 0 || out.Meta.TotalPages == 0 || out.Meta.CurrentPage >= out.Meta.TotalPages {
			break
		}
		page = out.Meta.CurrentPage + 1
	}
	return firewalls, nil
}

func (c Client) CreateFirewall(ctx context.Context, latitudeProject, name string, rules []FirewallRule) (Firewall, error) {
	attrs := map[string]any{
		"name":  name,
		"rules": rules,
	}
	if latitudeProject != "" {
		attrs["project"] = latitudeProject
	}
	body := jsonAPIRequest("firewalls", attrs)
	var out firewallResponse
	if err := c.request(ctx, http.MethodPost, "/firewalls", body, &out); err != nil {
		return Firewall{}, err
	}
	return out.Data, nil
}

func (c Client) UpdateFirewall(ctx context.Context, id, name string, rules []FirewallRule) (Firewall, error) {
	if strings.TrimSpace(id) == "" {
		return Firewall{}, fmt.Errorf("latitude firewall id is required")
	}
	attrs := map[string]any{
		"name":  name,
		"rules": rules,
	}
	body := map[string]any{
		"data": map[string]any{
			"id":         id,
			"type":       "firewalls",
			"attributes": attrs,
		},
	}
	var out firewallResponse
	if err := c.request(ctx, http.MethodPatch, "/firewalls/"+url.PathEscape(id), body, &out); err != nil {
		return Firewall{}, err
	}
	return out.Data, nil
}

func (c Client) EnsureFirewallAssignments(ctx context.Context, firewallID string, hosts []provider.Host) error {
	if strings.TrimSpace(firewallID) == "" || len(hosts) == 0 {
		return nil
	}
	assignments, err := c.ListFirewallAssignments(ctx, firewallID)
	if err != nil {
		return err
	}
	assigned := map[string]bool{}
	for _, assignment := range assignments {
		if assignment.Attributes.Server.ID != "" {
			assigned[assignment.Attributes.Server.ID] = true
		}
	}
	for _, host := range hosts {
		if host.ID == "" || assigned[host.ID] {
			continue
		}
		if _, err := c.CreateFirewallAssignment(ctx, firewallID, host.ID); err != nil {
			return err
		}
		assigned[host.ID] = true
	}
	return nil
}

func (c Client) ListFirewallAssignments(ctx context.Context, firewallID string) ([]FirewallAssignment, error) {
	if strings.TrimSpace(firewallID) == "" {
		return nil, fmt.Errorf("latitude firewall id is required")
	}
	values := url.Values{}
	values.Set("page[size]", "100")
	page := 1
	var assignments []FirewallAssignment
	for {
		values.Set("page[number]", strconv.Itoa(page))
		var out firewallAssignmentsResponse
		path := "/firewalls/" + url.PathEscape(firewallID) + "/assignments?" + values.Encode()
		if err := c.request(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		assignments = append(assignments, out.Data...)
		if out.Meta.CurrentPage == 0 || out.Meta.TotalPages == 0 || out.Meta.CurrentPage >= out.Meta.TotalPages {
			break
		}
		page = out.Meta.CurrentPage + 1
	}
	return assignments, nil
}

func (c Client) CreateFirewallAssignment(ctx context.Context, firewallID, serverID string) (FirewallAssignment, error) {
	if strings.TrimSpace(firewallID) == "" {
		return FirewallAssignment{}, fmt.Errorf("latitude firewall id is required")
	}
	if strings.TrimSpace(serverID) == "" {
		return FirewallAssignment{}, fmt.Errorf("latitude server id is required")
	}
	body := jsonAPIRequest("firewall_assignments", map[string]any{"server_id": serverID})
	var out firewallAssignmentResponse
	path := "/firewalls/" + url.PathEscape(firewallID) + "/assignments"
	if err := c.request(ctx, http.MethodPost, path, body, &out); err != nil {
		return FirewallAssignment{}, err
	}
	return out.Data, nil
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
	req.Header.Set("Accept", "application/vnd.api+json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/vnd.api+json")
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
		return fmt.Errorf("latitude %s %s failed: %s", method, req.URL.Path, message)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode latitude response: %w", err)
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

func createServerBody(plan provider.HostPlan, latitude config.LatitudeConfig, userDataID string) map[string]any {
	attrs := map[string]any{
		"project":          latitude.Project,
		"plan":             plan.Size,
		"site":             plan.Location,
		"operating_system": plan.Image,
		"hostname":         cloudHostname(plan),
	}
	if len(latitude.SSHKeys) > 0 {
		attrs["ssh_keys"] = latitude.SSHKeys
	}
	if userDataID != "" {
		attrs["user_data"] = userDataID
	}
	if latitude.RAID != "" {
		attrs["raid"] = latitude.RAID
	}
	if len(latitude.DiskLayout) > 0 {
		attrs["disk_layout"] = latitude.DiskLayout
	}
	if latitude.IPXE != "" {
		attrs["ipxe"] = latitude.IPXE
	}
	if latitude.Billing != "" {
		attrs["billing"] = latitude.Billing
	}
	return jsonAPIRequest("servers", attrs)
}

func jsonAPIRequest(kind string, attrs map[string]any) map[string]any {
	return map[string]any{
		"data": map[string]any{
			"type":       kind,
			"attributes": attrs,
		},
	}
}

func withPlanDefaults(plan provider.HostPlan, latitude config.LatitudeConfig) config.LatitudeConfig {
	if plan.Location != "" {
		latitude.Site = plan.Location
	}
	if plan.Size != "" {
		latitude.Plan = plan.Size
	}
	if plan.Image != "" {
		latitude.OperatingSystem = plan.Image
	}
	return latitude
}

func hostFromServer(server Server, project, environment string) provider.Host {
	name := stripOwnershipPrefix(server.Attributes.Hostname)
	labels := provider.ShipLabels(project, environment, poolFromName(name))
	return provider.Host{
		ID:            server.ID,
		Name:          name,
		Pool:          labels[LabelPool],
		PublicAddress: firstNonEmpty(server.Attributes.PrimaryIPv4, server.Attributes.PrimaryIPv6),
		Labels:        labels,
	}
}

func cloudHostname(plan provider.HostPlan) string {
	prefix := ownershipPrefix(plan.Project, plan.Environment)
	name := sanitizeHostname(plan.Name)
	if name == "" {
		name = "host"
	}
	hostname := prefix + name
	if len(hostname) <= 63 {
		return hostname
	}
	return strings.Trim(hostname[:63], "-")
}

func ownershipPrefix(project, environment string) string {
	prefix := "ship-" + sanitizeHostname(project) + "-" + sanitizeHostname(environment) + "-"
	prefix = strings.ReplaceAll(prefix, "--", "-")
	if len(prefix) > 48 {
		prefix = strings.Trim(prefix[:48], "-") + "-"
	}
	return prefix
}

func stripOwnershipPrefix(name string) string {
	parts := strings.Split(name, "-")
	if len(parts) < 4 || parts[0] != "ship" {
		return name
	}
	return strings.Join(parts[3:], "-")
}

func sanitizeHostname(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = nonHostnameChars.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	for strings.Contains(value, "--") {
		value = strings.ReplaceAll(value, "--", "-")
	}
	return value
}

func poolFromName(name string) string {
	if before, _, ok := strings.Cut(name, "-"); ok && before != "" {
		return before
	}
	return name
}

func userDataDescription(project, environment, content string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	if len(encoded) > 16 {
		encoded = encoded[:16]
	}
	return "ship-" + sanitizeHostname(project) + "-" + sanitizeHostname(environment) + "-" + encoded
}

func firewallName(project, environment string) string {
	name := "ship-" + sanitizeHostname(project) + "-" + sanitizeHostname(environment) + "-firewall"
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	name = strings.Trim(name, "-")
	if len(name) <= 63 {
		return name
	}
	return strings.Trim(name[:63], "-")
}

func firewallRules(latitude config.LatitudeConfig) []FirewallRule {
	var rules []FirewallRule
	if latitude.EffectiveSSHFirewall() == config.SSHFirewallManaged {
		for _, cidr := range latitude.SSHAllowedCIDRs {
			rules = append(rules, firewallRule(cidr, "ANY", "22", "TCP", "Allow SSH from Ship-approved CIDR"))
		}
	}
	rules = append(rules,
		firewallRule("ANY", "ANY", "80", "TCP", "Allow HTTP"),
		firewallRule("ANY", "ANY", "443", "TCP", "Allow HTTPS"),
		firewallRule("ANY", "ANY", "443", "UDP", "Allow HTTP/3"),
	)
	return rules
}

func firewallRule(from, to, port, protocol, description string) FirewallRule {
	return FirewallRule{
		From:        from,
		To:          to,
		Port:        port,
		Protocol:    protocol,
		Description: description,
	}
}

func firewallHasRules(firewall Firewall, want []FirewallRule) bool {
	for _, rule := range want {
		if !containsFirewallRule(firewall.Attributes.Rules, rule) {
			return false
		}
	}
	return true
}

func containsFirewallRule(existing []FirewallRule, want FirewallRule) bool {
	for _, got := range existing {
		if strings.EqualFold(got.Protocol, want.Protocol) &&
			strings.EqualFold(firstNonEmpty(got.From, "ANY"), firstNonEmpty(want.From, "ANY")) &&
			strings.EqualFold(firstNonEmpty(got.To, "ANY"), firstNonEmpty(want.To, "ANY")) &&
			got.Port == want.Port {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func tagNamesForLabels(labels map[string]string) []string {
	names := make([]string, 0, len(labels))
	for key, value := range labels {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			continue
		}
		names = append(names, tagNameForLabel(key, value))
	}
	sort.Strings(names)
	return names
}

func tagNameForLabel(key, value string) string {
	name := "ship-" + sanitizeHostname(key) + "-" + sanitizeHostname(value)
	if len(name) <= 40 {
		return name
	}
	sum := sha1.Sum([]byte(name))
	suffix := hex.EncodeToString(sum[:])[:8]
	prefix := strings.Trim(name[:31], "-")
	return prefix + "-" + suffix
}

func tagIDsForServer(server Server) []string {
	ids := make([]string, 0, len(server.Attributes.Tags))
	for _, tag := range server.Attributes.Tags {
		if tag.ID != "" {
			ids = append(ids, tag.ID)
		}
	}
	return ids
}

func mergeTagIDs(groups ...[]string) []string {
	seen := map[string]bool{}
	var merged []string
	for _, group := range groups {
		for _, id := range group {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			merged = append(merged, id)
		}
	}
	sort.Strings(merged)
	return merged
}

func serverHasAllTagIDs(server Server, ids []string) bool {
	serverIDs := map[string]bool{}
	for _, tag := range server.Attributes.Tags {
		serverIDs[tag.ID] = true
	}
	for _, id := range ids {
		if !serverIDs[id] {
			return false
		}
	}
	return true
}

type serverResponse struct {
	Data Server `json:"data"`
}

type serversResponse struct {
	Data []Server `json:"data"`
	Meta pageMeta `json:"meta"`
}

type userDataResponse struct {
	Data UserData `json:"data"`
}

type userDataListResponse struct {
	Data []UserData `json:"data"`
}

type firewallResponse struct {
	Data Firewall `json:"data"`
}

type firewallsResponse struct {
	Data []Firewall `json:"data"`
	Meta pageMeta   `json:"meta"`
}

type firewallAssignmentResponse struct {
	Data FirewallAssignment `json:"data"`
}

type firewallAssignmentsResponse struct {
	Data []FirewallAssignment `json:"data"`
	Meta pageMeta             `json:"meta"`
}

type tagResponse struct {
	Data LatitudeTag `json:"data"`
}

type tagsResponse struct {
	Data []LatitudeTag `json:"data"`
}

type pageMeta struct {
	CurrentPage int `json:"current_page"`
	TotalPages  int `json:"total_pages"`
}
