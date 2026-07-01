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
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels"`
	PublicNet PublicNet         `json:"public_net"`
}

type PublicNet struct {
	IPv4 PublicIPv4 `json:"ipv4"`
}

type PublicIPv4 struct {
	IP string `json:"ip"`
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

	return provider.ReconcileHosts(ctx, project, environment, desired, reconcileBackend{client: c, sshKeys: env.Provider.Hetzner.SSHKeys})
}

type reconcileBackend struct {
	client  Client
	sshKeys []string
}

func (b reconcileBackend) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	return b.client.List(ctx, project, environment)
}

func (b reconcileBackend) Create(ctx context.Context, plan provider.HostPlan) (provider.Host, error) {
	server, err := b.client.CreateServer(ctx, plan, b.sshKeys)
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

func (c Client) CreateServer(ctx context.Context, plan provider.HostPlan, sshKeys []string) (Server, error) {
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
	if len(sshKeys) > 0 {
		body["ssh_keys"] = sshKeys
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
	}
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
