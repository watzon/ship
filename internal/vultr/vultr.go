package vultr

import (
	"bytes"
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
	defaultAPIBase = "https://api.vultr.com/v2"

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

type InstancePlan = provider.HostPlan

type Instance struct {
	ID     string   `json:"id"`
	Label  string   `json:"label"`
	MainIP string   `json:"main_ip"`
	Tags   []string `json:"tags"`
}

type ReconcileResult = provider.ReconcileResult

func NewFromEnv(dryRun bool) Client {
	return Client{Token: os.Getenv("VULTR_API_KEY"), DryRun: dryRun, HTTP: http.DefaultClient}
}

func (c Client) Name() string {
	return config.ProviderVultr
}

func (c Client) TokenPresent() bool {
	return strings.TrimSpace(c.Token) != ""
}

func (c Client) CredentialChecks(lookupEnv func(string) (string, bool)) []provider.CredentialCheck {
	_, ok := lookupEnv("VULTR_API_KEY")
	return []provider.CredentialCheck{{
		Name:           "vultr token",
		Present:        ok,
		Required:       true,
		PresentMessage: "VULTR_API_KEY is set",
		MissingMessage: "missing VULTR_API_KEY",
	}}
}

func DesiredInstances(env config.Environment) []provider.HostPlan {
	return DesiredInstancesFor("", "", env)
}

func DesiredInstancesFor(project, environment string, env config.Environment) []provider.HostPlan {
	vultr := env.Provider.Vultr
	if vultr == nil {
		return nil
	}
	return provider.HostPlans(project, environment, env, provider.HostPlanOptions{
		Location: vultr.Region,
		Size:     vultr.Plan,
		Image:    sourceDescription(*vultr),
	})
}

func (c Client) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.Vultr == nil {
		return nil, fmt.Errorf("environment %q must define provider.vultr", environment)
	}
	return DesiredInstancesFor(project, environment, env), nil
}

func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	if env.Provider.Vultr == nil {
		return provider.ReconcileResult{}, fmt.Errorf("environment %q must define provider.vultr", environment)
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

	return provider.ReconcileHosts(ctx, project, environment, desired, reconcileBackend{client: c, vultr: *env.Provider.Vultr})
}

type reconcileBackend struct {
	client Client
	vultr  config.VultrConfig
}

func (b reconcileBackend) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	return b.client.List(ctx, project, environment)
}

func (b reconcileBackend) Create(ctx context.Context, plan provider.HostPlan) (provider.Host, error) {
	instance, err := b.client.CreateInstance(ctx, plan, b.vultr)
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

func (c Client) Delete(ctx context.Context, host provider.Host) error {
	if strings.TrimSpace(host.ID) == "" {
		return fmt.Errorf("instance id is required")
	}
	return c.DeleteInstance(ctx, host.ID)
}

func (c Client) ListInstances(ctx context.Context, project, environment string) ([]Instance, error) {
	if !c.TokenPresent() {
		return nil, fmt.Errorf("VULTR_API_KEY is required")
	}
	var instances []Instance
	cursor := ""
	for {
		values := url.Values{}
		values.Set("per_page", "500")
		if cursor != "" {
			values.Set("cursor", cursor)
		}
		var out listInstancesResponse
		if err := c.request(ctx, http.MethodGet, "/instances?"+values.Encode(), nil, &out); err != nil {
			return nil, err
		}
		for _, instance := range out.Instances {
			if instanceMatches(instance, project, environment) {
				instances = append(instances, instance)
			}
		}
		if out.Meta.Links.Next == "" {
			break
		}
		cursor = nextCursor(out.Meta.Links.Next)
	}
	return instances, nil
}

func (c Client) CreateInstance(ctx context.Context, plan provider.HostPlan, vultr config.VultrConfig) (Instance, error) {
	if !c.TokenPresent() {
		return Instance{}, fmt.Errorf("VULTR_API_KEY is required")
	}
	tags := tagsForPlan(plan)
	body := map[string]any{
		"region": plan.Location,
		"plan":   plan.Size,
		"label":  plan.Name,
		"tags":   tags,
	}
	if len(vultr.SSHKeyIDs) > 0 {
		body["sshkey_id"] = vultr.SSHKeyIDs
	}
	addSource(body, vultr)
	if c.DryRun {
		return Instance{Label: plan.Name, Tags: tags}, nil
	}
	var out createInstanceResponse
	if err := c.request(ctx, http.MethodPost, "/instances", body, &out); err != nil {
		return Instance{}, err
	}
	return out.Instance, nil
}

func (c Client) DeleteInstance(ctx context.Context, id string) error {
	if !c.TokenPresent() {
		return fmt.Errorf("VULTR_API_KEY is required")
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("instance id is required")
	}
	if c.DryRun {
		return nil
	}
	return c.request(ctx, http.MethodDelete, "/instances/"+url.PathEscape(id), nil, nil)
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
		return fmt.Errorf("vultr %s %s failed: %s", method, path, strings.TrimSpace(string(data)))
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

func nextCursor(next string) string {
	parsed, err := url.Parse(next)
	if err != nil || parsed.Query().Get("cursor") == "" {
		return next
	}
	return parsed.Query().Get("cursor")
}

func sourceDescription(vultr config.VultrConfig) string {
	switch {
	case vultr.OSID != 0:
		return "os_id:" + strconv.Itoa(vultr.OSID)
	case vultr.ImageID != "":
		return "image_id:" + vultr.ImageID
	case vultr.SnapshotID != "":
		return "snapshot_id:" + vultr.SnapshotID
	case vultr.AppID != 0:
		return "app_id:" + strconv.Itoa(vultr.AppID)
	default:
		return ""
	}
}

func addSource(body map[string]any, vultr config.VultrConfig) {
	switch {
	case vultr.OSID != 0:
		body["os_id"] = vultr.OSID
	case vultr.ImageID != "":
		body["image_id"] = vultr.ImageID
	case vultr.SnapshotID != "":
		body["snapshot_id"] = vultr.SnapshotID
	case vultr.AppID != 0:
		body["app_id"] = vultr.AppID
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
		tags = append(tags, tagPrefix+key+"="+value)
	}
	sort.Strings(tags)
	return tags
}

func labelsFromTags(tags []string) map[string]string {
	labels := map[string]string{}
	for _, tag := range tags {
		trimmed := strings.TrimPrefix(tag, tagPrefix)
		if trimmed == tag {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok || key == "" {
			continue
		}
		labels[key] = value
	}
	return labels
}

func hostFromInstance(instance Instance) provider.Host {
	labels := labelsFromTags(instance.Tags)
	return provider.Host{
		ID:            instance.ID,
		Name:          instance.Label,
		Pool:          labels[LabelPool],
		PublicAddress: instance.MainIP,
		Labels:        labels,
	}
}

type listInstancesResponse struct {
	Instances []Instance `json:"instances"`
	Meta      struct {
		Links struct {
			Next string `json:"next"`
		} `json:"links"`
	} `json:"meta"`
}

type createInstanceResponse struct {
	Instance Instance `json:"instance"`
	JobIDs   []string `json:"job_ids"`
}
