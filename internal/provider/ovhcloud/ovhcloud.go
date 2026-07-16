package ovhcloud

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
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

const defaultAPIBase = "https://eu.api.ovh.com/1.0"

type Client struct {
	ApplicationKey    string
	ApplicationSecret string
	ConsumerKey       string
	ServiceName       string
	Region            string
	DryRun            bool
	HTTP              *http.Client
	BaseURL           string
	Now               func() time.Time
}

type Instance struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Status      string      `json:"status"`
	Region      string      `json:"region"`
	FlavorID    string      `json:"flavorId"`
	ImageID     string      `json:"imageId"`
	SSHKeyID    string      `json:"sshKeyId"`
	IPAddresses []IPAddress `json:"ipAddresses"`
}

type IPAddress struct {
	IP      string `json:"ip"`
	Version int    `json:"version"`
	Type    string `json:"type"`
}

type ReconcileResult = provider.ReconcileResult

func NewFromEnv(dryRun bool, configs ...config.OVHCloudConfig) Client {
	cfg := config.OVHCloudConfig{}
	if len(configs) > 0 {
		cfg = configs[0]
	}
	return Client{
		ApplicationKey:    os.Getenv("OVH_APPLICATION_KEY"),
		ApplicationSecret: os.Getenv("OVH_APPLICATION_SECRET"),
		ConsumerKey:       os.Getenv("OVH_CONSUMER_KEY"),
		ServiceName:       cfg.ServiceName,
		Region:            cfg.Region,
		DryRun:            dryRun,
		HTTP:              http.DefaultClient,
		BaseURL:           endpointBase(firstNonEmpty(os.Getenv("OVH_ENDPOINT"), cfg.Endpoint)),
	}
}

func (c Client) Name() string {
	return config.ProviderOVHCloud
}

func (c Client) CredentialsPresent() bool {
	return strings.TrimSpace(c.ApplicationKey) != "" &&
		strings.TrimSpace(c.ApplicationSecret) != "" &&
		strings.TrimSpace(c.ConsumerKey) != ""
}

func (c Client) CredentialChecks(lookupEnv func(string) (string, bool)) []provider.CredentialCheck {
	_, keyOK := lookupEnv("OVH_APPLICATION_KEY")
	_, secretOK := lookupEnv("OVH_APPLICATION_SECRET")
	_, consumerOK := lookupEnv("OVH_CONSUMER_KEY")
	return []provider.CredentialCheck{{
		Name:           "ovhcloud credentials",
		Present:        keyOK && secretOK && consumerOK,
		Required:       true,
		PresentMessage: "OVH_APPLICATION_KEY/OVH_APPLICATION_SECRET/OVH_CONSUMER_KEY are set",
		MissingMessage: "missing OVH_APPLICATION_KEY/OVH_APPLICATION_SECRET/OVH_CONSUMER_KEY",
	}}
}

func DesiredServers(env config.Environment) []provider.HostPlan {
	return DesiredServersFor("", "", env)
}

func DesiredServersFor(project, environment string, env config.Environment) []provider.HostPlan {
	ovh := env.Provider.OVHCloud
	if ovh == nil {
		return nil
	}
	return provider.HostPlans(project, environment, env, provider.HostPlanOptions{
		Location: ovh.Region,
		Size:     ovh.FlavorID,
		Image:    ovh.ImageID,
		UserData: ovh.UserData,
	})
}

func (c Client) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.OVHCloud == nil {
		return nil, fmt.Errorf("environment %q must define provider.ovhcloud", environment)
	}
	return DesiredServersFor(project, environment, env), nil
}

func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	if env.Provider.OVHCloud == nil {
		return provider.ReconcileResult{}, fmt.Errorf("environment %q must define provider.ovhcloud", environment)
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
	ovh := *env.Provider.OVHCloud
	return provider.ReconcileHosts(ctx, project, environment, desired, reconcileBackend{client: c, ovh: ovh})
}

// CreateHost provisions a single instance using the same backend Reconcile
// builds, so `ship migrate` can add a replacement alongside the existing one.
func (c Client) CreateHost(ctx context.Context, project, environment string, env config.Environment, plan provider.HostPlan) (provider.Host, error) {
	if env.Provider.OVHCloud == nil {
		return provider.Host{}, fmt.Errorf("environment %q must define provider.ovhcloud", environment)
	}
	return reconcileBackend{client: c, ovh: *env.Provider.OVHCloud}.Create(ctx, plan)
}

var _ provider.HostCreator = Client{}

type reconcileBackend struct {
	client Client
	ovh    config.OVHCloudConfig
}

func (b reconcileBackend) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	return b.client.List(ctx, project, environment)
}

func (b reconcileBackend) Create(ctx context.Context, plan provider.HostPlan) (provider.Host, error) {
	instance, err := b.client.CreateInstance(ctx, plan, b.ovh)
	if err != nil {
		return provider.Host{}, err
	}
	return hostFromInstance(instance, plan.Project, plan.Environment), nil
}

func (c Client) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	instances, err := c.ListInstances(ctx, project, environment)
	if err != nil {
		return nil, err
	}
	hosts := make([]provider.Host, 0, len(instances))
	for _, instance := range instances {
		hosts = append(hosts, hostFromInstance(instance, project, environment))
	}
	return hosts, nil
}

func (c Client) Delete(ctx context.Context, host provider.Host) error {
	if strings.TrimSpace(host.ID) == "" {
		return fmt.Errorf("ovhcloud instance id is required")
	}
	return c.DeleteInstance(ctx, host.ID)
}

func (c Client) ListInstances(ctx context.Context, project, environment string) ([]Instance, error) {
	if !c.CredentialsPresent() {
		return nil, fmt.Errorf("OVH_APPLICATION_KEY/OVH_APPLICATION_SECRET/OVH_CONSUMER_KEY are required")
	}
	var out []Instance
	if err := c.request(ctx, http.MethodGet, c.projectPath("/instance"), nil, &out); err != nil {
		return nil, err
	}
	prefix := cloudNamePrefix(project, environment)
	var instances []Instance
	for _, instance := range out {
		if c.Region != "" && instance.Region != "" && !strings.EqualFold(instance.Region, c.Region) {
			continue
		}
		if strings.HasPrefix(instance.Name, prefix) {
			instances = append(instances, instance)
		}
	}
	sort.SliceStable(instances, func(i, j int) bool { return instances[i].Name < instances[j].Name })
	return instances, nil
}

func (c Client) CreateInstance(ctx context.Context, plan provider.HostPlan, ovh config.OVHCloudConfig) (Instance, error) {
	if !c.CredentialsPresent() {
		return Instance{}, fmt.Errorf("OVH_APPLICATION_KEY/OVH_APPLICATION_SECRET/OVH_CONSUMER_KEY are required")
	}
	ovh = withPlanDefaults(plan, ovh)
	body := createInstanceBody(plan, ovh)
	if c.DryRun {
		return Instance{ID: plan.Name, Name: body["name"].(string), Region: ovh.Region}, nil
	}
	var out Instance
	if err := c.request(ctx, http.MethodPost, c.projectPath("/instance"), body, &out); err != nil {
		return Instance{}, err
	}
	return out, nil
}

func (c Client) DeleteInstance(ctx context.Context, id string) error {
	return c.request(ctx, http.MethodDelete, c.projectPath("/instance/"+url.PathEscape(id)), nil, nil)
}

func (c Client) request(ctx context.Context, method, path string, payload any, out any) error {
	base := strings.TrimRight(firstNonEmpty(c.BaseURL, defaultAPIBase), "/")
	var bodyText string
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		bodyText = string(data)
		body = bytes.NewReader(data)
	}
	fullURL := base + path
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Ovh-Application", c.ApplicationKey)
	req.Header.Set("X-Ovh-Consumer", c.ConsumerKey)
	timestamp := c.timestamp()
	req.Header.Set("X-Ovh-Timestamp", timestamp)
	req.Header.Set("X-Ovh-Signature", c.signature(method, fullURL, bodyText, timestamp))
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
		return fmt.Errorf("ovhcloud api %s %s failed: %s: %s", method, path, res.Status, strings.TrimSpace(string(data)))
	}
	if out != nil && res.StatusCode != http.StatusNoContent && res.ContentLength != 0 {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil && err != io.EOF {
			return err
		}
	}
	return nil
}

func (c Client) projectPath(suffix string) string {
	return "/cloud/project/" + url.PathEscape(c.ServiceName) + suffix
}

func (c Client) timestamp() string {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	return strconv.FormatInt(now().Unix(), 10)
}

func (c Client) signature(method, fullURL, body, timestamp string) string {
	value := c.ApplicationSecret + "+" + c.ConsumerKey + "+" + strings.ToUpper(method) + "+" + fullURL + "+" + body + "+" + timestamp
	sum := sha1.Sum([]byte(value))
	return "$1$" + hex.EncodeToString(sum[:])
}

func createInstanceBody(plan provider.HostPlan, ovh config.OVHCloudConfig) map[string]any {
	body := map[string]any{
		"name":           cloudName(plan.Project, plan.Environment, plan.Name),
		"region":         ovh.Region,
		"flavorId":       ovh.FlavorID,
		"imageId":        ovh.ImageID,
		"sshKeyId":       ovh.SSHKeyID,
		"monthlyBilling": ovh.MonthlyBillingValue(false),
	}
	if strings.TrimSpace(ovh.UserData) != "" {
		body["userData"] = ovh.UserData
	}
	return body
}

func withPlanDefaults(plan provider.HostPlan, ovh config.OVHCloudConfig) config.OVHCloudConfig {
	if plan.Location != "" {
		ovh.Region = plan.Location
	}
	if plan.Size != "" {
		ovh.FlavorID = plan.Size
	}
	if plan.Image != "" {
		ovh.ImageID = plan.Image
	}
	if plan.UserData != "" {
		ovh.UserData = plan.UserData
	}
	return ovh
}

func hostFromInstance(instance Instance, project, environment string) provider.Host {
	name := strings.TrimPrefix(instance.Name, cloudNamePrefix(project, environment))
	if name == "" {
		name = instance.Name
	}
	pool := strings.SplitN(name, "-", 2)[0]
	return provider.Host{
		ID:            instance.ID,
		Name:          name,
		Pool:          pool,
		PublicAddress: publicAddress(instance),
		Labels:        provider.ShipLabels(project, environment, pool),
	}
}

func publicAddress(instance Instance) string {
	for _, ip := range instance.IPAddresses {
		if ip.Version == 4 && (ip.Type == "" || strings.EqualFold(ip.Type, "public")) {
			return ip.IP
		}
	}
	for _, ip := range instance.IPAddresses {
		if ip.IP != "" && (ip.Type == "" || strings.EqualFold(ip.Type, "public")) {
			return ip.IP
		}
	}
	return ""
}

func cloudName(project, environment, name string) string {
	return cloudNamePrefix(project, environment) + name
}

func cloudNamePrefix(project, environment string) string {
	return "ship-" + safeName(project) + "-" + safeName(environment) + "-"
}

func safeName(value string) string {
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

func endpointBase(endpoint string) string {
	switch strings.TrimSpace(endpoint) {
	case "", "ovh-eu", "eu":
		return defaultAPIBase
	case "ovh-us", "us":
		return "https://api.us.ovhcloud.com/1.0"
	case "ovh-ca", "ca":
		return "https://ca.api.ovh.com/1.0"
	default:
		if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
			return strings.TrimRight(endpoint, "/")
		}
		return endpoint
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
