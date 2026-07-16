package gcp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
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
	defaultComputeBase = "https://compute.googleapis.com/compute/v1"
	defaultTokenURL    = "https://oauth2.googleapis.com/token"
	computeScope       = "https://www.googleapis.com/auth/compute"

	LabelManagedBy   = provider.LabelManagedBy
	LabelProject     = provider.LabelProject
	LabelEnvironment = provider.LabelEnvironment
	LabelPool        = provider.LabelPool
)

type Client struct {
	AccessToken     string
	CredentialsFile string
	ProjectID       string
	Zone            string
	DryRun          bool
	HTTP            *http.Client
	BaseURL         string
	TokenURL        string
	Now             func() time.Time
}

type InstancePlan = provider.HostPlan

type Instance struct {
	ID                string             `json:"id"`
	Name              string             `json:"name"`
	Status            string             `json:"status"`
	Labels            map[string]string  `json:"labels"`
	NetworkInterfaces []NetworkInterface `json:"networkInterfaces"`
}

type NetworkInterface struct {
	NetworkIP     string         `json:"networkIP"`
	AccessConfigs []AccessConfig `json:"accessConfigs"`
}

type AccessConfig struct {
	NatIP string `json:"natIP"`
}

type Firewall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ReconcileResult = provider.ReconcileResult

func NewFromEnv(dryRun bool, configs ...config.GCPConfig) Client {
	var cfg config.GCPConfig
	if len(configs) > 0 {
		cfg = configs[0]
	}
	return Client{
		AccessToken:     os.Getenv("GCP_ACCESS_TOKEN"),
		CredentialsFile: os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"),
		ProjectID:       cfg.ProjectID,
		Zone:            cfg.Zone,
		DryRun:          dryRun,
		HTTP:            http.DefaultClient,
	}
}

func (c Client) Name() string {
	return config.ProviderGCP
}

func (c Client) CredentialChecks(lookupEnv func(string) (string, bool)) []provider.CredentialCheck {
	_, tokenOK := lookupEnv("GCP_ACCESS_TOKEN")
	_, credentialsOK := lookupEnv("GOOGLE_APPLICATION_CREDENTIALS")
	return []provider.CredentialCheck{{
		Name:           "gcp credentials",
		Present:        tokenOK || credentialsOK,
		Required:       true,
		PresentMessage: "GCP_ACCESS_TOKEN or GOOGLE_APPLICATION_CREDENTIALS is set",
		MissingMessage: "missing GCP_ACCESS_TOKEN or GOOGLE_APPLICATION_CREDENTIALS",
	}}
}

func DesiredInstances(env config.Environment) []provider.HostPlan {
	return DesiredInstancesFor("", "", env)
}

func DesiredInstancesFor(project, environment string, env config.Environment) []provider.HostPlan {
	gcp := env.Provider.GCP
	if gcp == nil {
		return nil
	}
	return provider.HostPlans(project, environment, env, provider.HostPlanOptions{
		Location: gcp.Zone,
		Size:     gcp.MachineType,
		Image:    gcp.Image,
		UserData: gcp.UserData,
	})
}

func (c Client) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.GCP == nil {
		return nil, fmt.Errorf("environment %q must define provider.gcp", environment)
	}
	return DesiredInstancesFor(project, environment, env), nil
}

func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	if env.Provider.GCP == nil {
		return provider.ReconcileResult{}, fmt.Errorf("environment %q must define provider.gcp", environment)
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

// reconcileBackendFor ensures the GCP firewall and builds the reconcile backend
// shared by Reconcile and CreateHost so both create instances identically.
func (c Client) reconcileBackendFor(ctx context.Context, project, environment string, env config.Environment) (reconcileBackend, error) {
	gcp := *env.Provider.GCP
	if gcp.Firewall.ManagedValue(true) {
		if _, err := c.EnsureFirewall(ctx, project, environment, gcp); err != nil {
			return reconcileBackend{}, err
		}
	}
	return reconcileBackend{client: c, gcp: gcp}, nil
}

// CreateHost provisions a single instance using the backend Reconcile would
// build, so `ship migrate` can add a replacement alongside the existing one.
func (c Client) CreateHost(ctx context.Context, project, environment string, env config.Environment, plan provider.HostPlan) (provider.Host, error) {
	if env.Provider.GCP == nil {
		return provider.Host{}, fmt.Errorf("environment %q must define provider.gcp", environment)
	}
	backend, err := c.reconcileBackendFor(ctx, project, environment, env)
	if err != nil {
		return provider.Host{}, err
	}
	return backend.Create(ctx, plan)
}

var _ provider.HostCreator = Client{}

type reconcileBackend struct {
	client Client
	gcp    config.GCPConfig
}

func (b reconcileBackend) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	return b.client.List(ctx, project, environment)
}

func (b reconcileBackend) Create(ctx context.Context, plan provider.HostPlan) (provider.Host, error) {
	instance, err := b.client.CreateInstance(ctx, plan, b.gcp)
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

func (c Client) ListInstances(ctx context.Context, project, environment string) ([]Instance, error) {
	if strings.TrimSpace(c.ProjectID) == "" || strings.TrimSpace(c.Zone) == "" {
		return nil, fmt.Errorf("gcp project and zone are required")
	}
	var instances []Instance
	pageToken := ""
	for {
		path := fmt.Sprintf("/projects/%s/zones/%s/instances", url.PathEscape(c.ProjectID), url.PathEscape(c.Zone))
		values := url.Values{}
		if pageToken != "" {
			values.Set("pageToken", pageToken)
		}
		if encoded := values.Encode(); encoded != "" {
			path += "?" + encoded
		}
		var out listInstancesResponse
		if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		for _, instance := range out.Items {
			if instanceMatches(instance, project, environment) {
				instances = append(instances, instance)
			}
		}
		if out.NextPageToken == "" {
			break
		}
		pageToken = out.NextPageToken
	}
	sort.SliceStable(instances, func(i, j int) bool {
		if instances[i].Labels[LabelPool] != instances[j].Labels[LabelPool] {
			return instances[i].Labels[LabelPool] < instances[j].Labels[LabelPool]
		}
		return instances[i].Name < instances[j].Name
	})
	return instances, nil
}

func (c Client) Delete(ctx context.Context, host provider.Host) error {
	if strings.TrimSpace(host.ID) == "" {
		return fmt.Errorf("instance id is required")
	}
	if strings.TrimSpace(c.ProjectID) == "" || strings.TrimSpace(c.Zone) == "" {
		return fmt.Errorf("gcp project and zone are required")
	}
	return c.DeleteInstance(ctx, config.GCPConfig{ProjectID: c.ProjectID, Zone: c.Zone}, host.ID)
}

func (c Client) DeleteInstance(ctx context.Context, gcp config.GCPConfig, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("instance name is required")
	}
	if c.DryRun {
		return nil
	}
	path := fmt.Sprintf("/projects/%s/zones/%s/instances/%s", url.PathEscape(gcp.ProjectID), url.PathEscape(gcp.Zone), url.PathEscape(name))
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

func (c Client) CreateInstance(ctx context.Context, plan provider.HostPlan, gcp config.GCPConfig) (Instance, error) {
	body := map[string]any{
		"name":        plan.Name,
		"machineType": machineTypeURL(gcp.ProjectID, plan.Location, plan.Size),
		"labels":      gcpLabels(plan.Labels),
		"tags":        map[string]any{"items": instanceNetworkTags(plan, gcp)},
		"disks": []map[string]any{{
			"boot":             true,
			"autoDelete":       true,
			"initializeParams": initializeParams(gcp.ProjectID, plan.Location, plan.Image, gcp),
		}},
		"networkInterfaces": []map[string]any{networkInterface(gcp.ProjectID, plan.Location, gcp)},
	}
	if metadata := metadataItems(plan, gcp); len(metadata) > 0 {
		body["metadata"] = map[string]any{"items": metadata}
	}
	if len(gcp.Scopes) > 0 || gcp.ServiceAccount != "" {
		email := gcp.ServiceAccount
		if email == "" {
			email = "default"
		}
		body["serviceAccounts"] = []map[string]any{{
			"email":  email,
			"scopes": effectiveScopes(gcp),
		}}
	}
	if shielded := shieldedConfig(gcp); len(shielded) > 0 {
		body["shieldedInstanceConfig"] = shielded
	}
	if c.DryRun {
		return Instance{Name: plan.Name, Labels: gcpLabels(plan.Labels)}, nil
	}
	path := fmt.Sprintf("/projects/%s/zones/%s/instances", url.PathEscape(gcp.ProjectID), url.PathEscape(plan.Location))
	if err := c.do(ctx, http.MethodPost, path, body, nil); err != nil {
		return Instance{}, err
	}
	return c.GetInstance(ctx, gcp, plan.Name)
}

func (c Client) GetInstance(ctx context.Context, gcp config.GCPConfig, name string) (Instance, error) {
	if strings.TrimSpace(name) == "" {
		return Instance{}, fmt.Errorf("instance name is required")
	}
	var out Instance
	path := fmt.Sprintf("/projects/%s/zones/%s/instances/%s", url.PathEscape(gcp.ProjectID), url.PathEscape(gcp.Zone), url.PathEscape(name))
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return Instance{}, err
	}
	return out, nil
}

func (c Client) EnsureFirewall(ctx context.Context, project, environment string, gcp config.GCPConfig) (Firewall, error) {
	name := gcp.Firewall.Name
	if strings.TrimSpace(name) == "" {
		name = resourceName(project, environment, "firewall")
	}
	existing, err := c.ListFirewalls(ctx, gcp)
	if err != nil {
		return Firewall{}, err
	}
	existingByName := map[string]Firewall{}
	for _, firewall := range existing {
		existingByName[firewall.Name] = firewall
	}
	publicFirewall, err := c.ensureFirewallRule(ctx, project, environment, gcp, existingByName, name, []string{"0.0.0.0/0", "::/0"}, []string{"80", "443"})
	if err != nil {
		return Firewall{}, err
	}
	if gcp.EffectiveSSHFirewall() == config.SSHFirewallManaged {
		if _, err := c.ensureFirewallRule(ctx, project, environment, gcp, existingByName, name+"-ssh", gcp.SSHAllowedCIDRs, []string{"22"}); err != nil {
			return Firewall{}, err
		}
	}
	return publicFirewall, nil
}

func (c Client) ListFirewalls(ctx context.Context, gcp config.GCPConfig) ([]Firewall, error) {
	var firewalls []Firewall
	pageToken := ""
	for {
		path := fmt.Sprintf("/projects/%s/global/firewalls", url.PathEscape(gcp.ProjectID))
		values := url.Values{}
		if pageToken != "" {
			values.Set("pageToken", pageToken)
		}
		if encoded := values.Encode(); encoded != "" {
			path += "?" + encoded
		}
		var out listFirewallsResponse
		if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		firewalls = append(firewalls, out.Items...)
		if out.NextPageToken == "" {
			break
		}
		pageToken = out.NextPageToken
	}
	return firewalls, nil
}

func (c Client) ensureFirewallRule(ctx context.Context, project, environment string, gcp config.GCPConfig, existing map[string]Firewall, name string, sourceRanges []string, ports []string) (Firewall, error) {
	if firewall, ok := existing[name]; ok {
		return firewall, nil
	}
	allowed := []map[string]any{{"IPProtocol": "tcp", "ports": ports}}
	if containsString(ports, "443") {
		allowed = append(allowed, map[string]any{"IPProtocol": "udp", "ports": []string{"443"}})
	}
	body := map[string]any{
		"name":         name,
		"network":      networkURL(gcp.ProjectID, gcp.Network),
		"direction":    "INGRESS",
		"targetTags":   []string{targetTag(project, environment)},
		"sourceRanges": compactStrings(sourceRanges),
		"allowed":      allowed,
	}
	if c.DryRun {
		return Firewall{Name: name}, nil
	}
	var out Firewall
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/projects/%s/global/firewalls", url.PathEscape(gcp.ProjectID)), body, &out); err != nil {
		return Firewall{}, err
	}
	if out.Name == "" {
		out.Name = name
	}
	return out, nil
}

func (c Client) do(ctx context.Context, method, path string, body any, out any) error {
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
	token, err := c.bearerToken(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
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
		return fmt.Errorf("gcp %s %s failed: %s", method, path, strings.TrimSpace(string(data)))
	}
	if out == nil || len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c Client) bearerToken(ctx context.Context) (string, error) {
	if strings.TrimSpace(c.AccessToken) != "" {
		return strings.TrimSpace(c.AccessToken), nil
	}
	if strings.TrimSpace(c.CredentialsFile) == "" {
		return "", fmt.Errorf("GCP_ACCESS_TOKEN or GOOGLE_APPLICATION_CREDENTIALS is required")
	}
	credentials, err := readServiceAccountCredentials(c.CredentialsFile)
	if err != nil {
		return "", err
	}
	assertion, err := c.serviceAccountAssertion(credentials)
	if err != nil {
		return "", err
	}
	values := url.Values{}
	values.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	values.Set("assertion", assertion)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL(c.TokenURL, credentials.TokenURI), strings.NewReader(values.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("gcp token request failed: %s", strings.TrimSpace(string(data)))
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("gcp token response missing access_token")
	}
	return out.AccessToken, nil
}

func (c Client) serviceAccountAssertion(credentials serviceAccountCredentials) (string, error) {
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"iss":   credentials.ClientEmail,
		"scope": computeScope,
		"aud":   tokenURL(c.TokenURL, credentials.TokenURI),
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	headerData, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimData, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(headerData) + "." + base64.RawURLEncoding.EncodeToString(claimData)
	key, err := parsePrivateKey(credentials.PrivateKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (c Client) apiBase() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return defaultComputeBase
}

func tokenURL(overrides ...string) string {
	for _, value := range overrides {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return defaultTokenURL
}

type serviceAccountCredentials struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

func readServiceAccountCredentials(path string) (serviceAccountCredentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return serviceAccountCredentials{}, fmt.Errorf("read GOOGLE_APPLICATION_CREDENTIALS: %w", err)
	}
	var credentials serviceAccountCredentials
	if err := json.Unmarshal(data, &credentials); err != nil {
		return serviceAccountCredentials{}, err
	}
	if credentials.ClientEmail == "" || credentials.PrivateKey == "" {
		return serviceAccountCredentials{}, fmt.Errorf("service account credentials must include client_email and private_key")
	}
	return credentials, nil
}

func parsePrivateKey(text string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, fmt.Errorf("service account private_key is not PEM")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("service account private_key is not RSA")
		}
		return rsaKey, nil
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	return key, nil
}

func instanceMatches(instance Instance, project, environment string) bool {
	return instance.Labels[LabelManagedBy] == "ship" &&
		instance.Labels[LabelProject] == safeLabelValue(project) &&
		instance.Labels[LabelEnvironment] == safeLabelValue(environment)
}

func hostFromInstance(instance Instance) provider.Host {
	return provider.Host{
		ID:            instance.Name,
		Name:          instance.Name,
		Pool:          instance.Labels[LabelPool],
		PublicAddress: publicAddress(instance),
		Labels:        instance.Labels,
	}
}

func publicAddress(instance Instance) string {
	for _, networkInterface := range instance.NetworkInterfaces {
		for _, accessConfig := range networkInterface.AccessConfigs {
			if accessConfig.NatIP != "" {
				return accessConfig.NatIP
			}
		}
		if networkInterface.NetworkIP != "" {
			return networkInterface.NetworkIP
		}
	}
	return ""
}

func gcpLabels(labels map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range labels {
		key = safeLabelKey(key)
		value = safeLabelValue(value)
		if key == "" || value == "" {
			continue
		}
		out[key] = value
	}
	return out
}

func safeLabelKey(value string) string {
	return safeLabelPart(value, true)
}

func safeLabelValue(value string) string {
	return safeLabelPart(value, false)
}

func safeLabelPart(value string, requireLetter bool) string {
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
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		return ""
	}
	if requireLetter && (out[0] < 'a' || out[0] > 'z') {
		out = "x-" + out
	}
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-_")
	}
	return out
}

func instanceNetworkTags(plan provider.HostPlan, gcp config.GCPConfig) []string {
	tags := append([]string(nil), gcp.NetworkTags...)
	tags = append(tags, targetTag(plan.Project, plan.Environment))
	return uniqueSafeNames(tags)
}

func uniqueSafeNames(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		safe := safeName(value)
		if safe == "" || seen[safe] {
			continue
		}
		seen[safe] = true
		out = append(out, safe)
	}
	sort.Strings(out)
	return out
}

func targetTag(project, environment string) string {
	return resourceName(project, environment, "hosts")
}

func machineTypeURL(project, zone, machineType string) string {
	if strings.Contains(machineType, "/") {
		return machineType
	}
	return fmt.Sprintf("zones/%s/machineTypes/%s", zone, machineType)
}

func sourceImage(project, image string, gcp config.GCPConfig) string {
	if strings.HasPrefix(image, "projects/") || strings.HasPrefix(image, "https://") || strings.HasPrefix(image, "global/") {
		return image
	}
	imageProject := gcp.ImageProject
	if imageProject == "" {
		return image
	}
	if family, ok := strings.CutPrefix(image, "family/"); ok {
		return fmt.Sprintf("projects/%s/global/images/family/%s", imageProject, family)
	}
	return fmt.Sprintf("projects/%s/global/images/%s", imageProject, image)
}

func initializeParams(project, zone, image string, gcp config.GCPConfig) map[string]any {
	params := map[string]any{"sourceImage": sourceImage(project, image, gcp)}
	if gcp.BootDisk.SizeGB > 0 {
		params["diskSizeGb"] = strconv.Itoa(gcp.BootDisk.SizeGB)
	}
	if gcp.BootDisk.Type != "" {
		params["diskType"] = diskTypeURL(project, zone, gcp.BootDisk.Type)
	}
	return params
}

func diskTypeURL(project, zone, diskType string) string {
	if strings.Contains(diskType, "/") {
		return diskType
	}
	return fmt.Sprintf("zones/%s/diskTypes/%s", zone, diskType)
}

func networkInterface(project, zone string, gcp config.GCPConfig) map[string]any {
	networkInterface := map[string]any{"network": networkURL(project, gcp.Network)}
	if gcp.Subnetwork != "" {
		networkInterface["subnetwork"] = subnetworkURL(project, zone, gcp.Subnetwork)
	}
	if gcp.ExternalIP == nil || *gcp.ExternalIP {
		networkInterface["accessConfigs"] = []map[string]any{{
			"name": "External NAT",
			"type": "ONE_TO_ONE_NAT",
		}}
	}
	return networkInterface
}

func networkURL(project, network string) string {
	if network == "" {
		network = "default"
	}
	if strings.Contains(network, "/") {
		return network
	}
	return fmt.Sprintf("global/networks/%s", network)
}

func subnetworkURL(project, zone, subnetwork string) string {
	if strings.Contains(subnetwork, "/") {
		return subnetwork
	}
	return fmt.Sprintf("regions/%s/subnetworks/%s", regionFromZone(zone), subnetwork)
}

func regionFromZone(zone string) string {
	if i := strings.LastIndex(zone, "-"); i > 0 {
		return zone[:i]
	}
	return zone
}

func metadataItems(plan provider.HostPlan, gcp config.GCPConfig) []map[string]string {
	metadata := map[string]string{}
	for key, value := range gcp.Metadata {
		if key != "" {
			metadata[key] = value
		}
	}
	if plan.UserData != "" {
		metadata["user-data"] = plan.UserData
	}
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	items := make([]map[string]string, 0, len(keys))
	for _, key := range keys {
		items = append(items, map[string]string{"key": key, "value": metadata[key]})
	}
	return items
}

func effectiveScopes(gcp config.GCPConfig) []string {
	if len(gcp.Scopes) > 0 {
		return append([]string(nil), gcp.Scopes...)
	}
	return []string{computeScope}
}

func shieldedConfig(gcp config.GCPConfig) map[string]any {
	out := map[string]any{}
	if gcp.ShieldedVM.SecureBoot != nil {
		out["enableSecureBoot"] = *gcp.ShieldedVM.SecureBoot
	}
	if gcp.ShieldedVM.VTPM != nil {
		out["enableVtpm"] = *gcp.ShieldedVM.VTPM
	}
	if gcp.ShieldedVM.IntegrityMonitoring != nil {
		out["enableIntegrityMonitoring"] = *gcp.ShieldedVM.IntegrityMonitoring
	}
	return out
}

func compactStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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
	if out[0] < 'a' || out[0] > 'z' {
		out = "x-" + out
	}
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	return out
}

type listInstancesResponse struct {
	Items         []Instance `json:"items"`
	NextPageToken string     `json:"nextPageToken"`
}

type listFirewallsResponse struct {
	Items         []Firewall `json:"items"`
	NextPageToken string     `json:"nextPageToken"`
}
