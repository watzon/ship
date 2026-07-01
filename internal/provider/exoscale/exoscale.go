package exoscale

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

const (
	defaultAPIBaseFormat = "https://api-%s.exoscale.com/v2"
	defaultZone          = "ch-gva-2"
	defaultDiskSizeGB    = 10

	LabelManagedBy   = provider.LabelManagedBy
	LabelProject     = provider.LabelProject
	LabelEnvironment = provider.LabelEnvironment
	LabelPool        = provider.LabelPool
)

type Client struct {
	APIKey    string
	APISecret string
	Zone      string
	DryRun    bool
	HTTP      *http.Client
	BaseURL   string
	Now       func() time.Time
}

type InstancePlan = provider.HostPlan

type Instance struct {
	ID                 string            `json:"id"`
	Name               string            `json:"name"`
	Labels             map[string]string `json:"labels"`
	PublicIP           string            `json:"public-ip"`
	IPv6Address        string            `json:"ipv6-address"`
	PublicIPAssignment string            `json:"public-ip-assignment"`
	SecurityGroups     []SecurityGroup   `json:"security-groups"`
}

type SecurityGroup struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	External    []string `json:"external-sources"`
}

type SecurityRule struct {
	FlowDirection string `json:"flow-direction"`
	Description   string `json:"description,omitempty"`
	Network       string `json:"network,omitempty"`
	Protocol      string `json:"protocol"`
	StartPort     int    `json:"start-port,omitempty"`
	EndPort       int    `json:"end-port,omitempty"`
}

type Operation struct {
	ID        string `json:"id"`
	Reason    string `json:"reason"`
	Message   string `json:"message"`
	State     string `json:"state"`
	Reference struct {
		ID      string `json:"id"`
		Link    string `json:"link"`
		Command string `json:"command"`
	} `json:"reference"`
}

type ReconcileResult = provider.ReconcileResult

func NewFromEnv(dryRun bool, configs ...config.ExoscaleConfig) Client {
	cfg := config.ExoscaleConfig{}
	if len(configs) > 0 {
		cfg = configs[0]
	}
	return Client{
		APIKey:    os.Getenv("EXOSCALE_API_KEY"),
		APISecret: os.Getenv("EXOSCALE_API_SECRET"),
		Zone:      cfg.Zone,
		DryRun:    dryRun,
		HTTP:      http.DefaultClient,
	}
}

func (c Client) Name() string {
	return config.ProviderExoscale
}

func (c Client) CredentialsPresent() bool {
	return strings.TrimSpace(c.APIKey) != "" && strings.TrimSpace(c.APISecret) != ""
}

func (c Client) CredentialChecks(lookupEnv func(string) (string, bool)) []provider.CredentialCheck {
	_, keyOK := lookupEnv("EXOSCALE_API_KEY")
	_, secretOK := lookupEnv("EXOSCALE_API_SECRET")
	return []provider.CredentialCheck{{
		Name:           "exoscale credentials",
		Present:        keyOK && secretOK,
		Required:       true,
		PresentMessage: "EXOSCALE_API_KEY/EXOSCALE_API_SECRET are set",
		MissingMessage: "missing EXOSCALE_API_KEY/EXOSCALE_API_SECRET",
	}}
}

func DesiredInstances(env config.Environment) []provider.HostPlan {
	return DesiredInstancesFor("", "", env)
}

func DesiredInstancesFor(project, environment string, env config.Environment) []provider.HostPlan {
	exoscale := env.Provider.Exoscale
	if exoscale == nil {
		return nil
	}
	return provider.HostPlans(project, environment, env, provider.HostPlanOptions{
		Location: exoscale.Zone,
		Size:     exoscale.InstanceType,
		Image:    exoscale.Template,
		UserData: exoscale.UserData,
	})
}

func (c Client) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.Exoscale == nil {
		return nil, fmt.Errorf("environment %q must define provider.exoscale", environment)
	}
	return DesiredInstancesFor(project, environment, env), nil
}

func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	if env.Provider.Exoscale == nil {
		return provider.ReconcileResult{}, fmt.Errorf("environment %q must define provider.exoscale", environment)
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

	exoscale := *env.Provider.Exoscale
	securityGroupIDs := append([]string{}, exoscale.SecurityGroups...)
	if exoscale.SecurityGroup.ManagedValue(true) {
		group, err := c.EnsureSecurityGroup(ctx, project, environment, exoscale)
		if err != nil {
			return provider.ReconcileResult{}, err
		}
		securityGroupIDs = append(securityGroupIDs, group.ID)
	} else if exoscale.SecurityGroup.ID != "" {
		securityGroupIDs = append(securityGroupIDs, exoscale.SecurityGroup.ID)
	}

	return provider.ReconcileHosts(ctx, project, environment, desired, reconcileBackend{
		client:           c,
		exoscale:         exoscale,
		securityGroupIDs: securityGroupIDs,
	})
}

type reconcileBackend struct {
	client           Client
	exoscale         config.ExoscaleConfig
	securityGroupIDs []string
}

func (b reconcileBackend) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	return b.client.List(ctx, project, environment)
}

func (b reconcileBackend) Create(ctx context.Context, plan provider.HostPlan) (provider.Host, error) {
	instance, err := b.client.CreateInstance(ctx, plan, b.exoscale, b.securityGroupIDs)
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
		return fmt.Errorf("exoscale instance id is required")
	}
	return c.DeleteInstance(ctx, host.ID)
}

func (c Client) ListInstances(ctx context.Context, project, environment string) ([]Instance, error) {
	if !c.CredentialsPresent() {
		return nil, fmt.Errorf("EXOSCALE_API_KEY/EXOSCALE_API_SECRET are required")
	}
	var out listInstancesResponse
	if err := c.request(ctx, http.MethodGet, "/instance", nil, &out); err != nil {
		return nil, err
	}
	var instances []Instance
	for _, instance := range out.Instances {
		if instanceMatches(instance, project, environment) {
			instances = append(instances, instance)
		}
	}
	return instances, nil
}

func (c Client) CreateInstance(ctx context.Context, plan provider.HostPlan, exoscale config.ExoscaleConfig, securityGroupIDs []string) (Instance, error) {
	if !c.CredentialsPresent() {
		return Instance{}, fmt.Errorf("EXOSCALE_API_KEY/EXOSCALE_API_SECRET are required")
	}
	exoscale = withPlanDefaults(plan, exoscale)
	body := createInstanceBody(plan, exoscale, securityGroupIDs)
	if c.DryRun {
		return Instance{Name: plan.Name, Labels: plan.Labels}, nil
	}
	var op Operation
	if err := c.request(ctx, http.MethodPost, "/instance", body, &op); err != nil {
		return Instance{}, err
	}
	instance := Instance{
		ID:     firstNonEmpty(op.Reference.ID, op.ID),
		Name:   plan.Name,
		Labels: plan.Labels,
	}
	return instance, nil
}

func (c Client) DeleteInstance(ctx context.Context, id string) error {
	if !c.CredentialsPresent() {
		return fmt.Errorf("EXOSCALE_API_KEY/EXOSCALE_API_SECRET are required")
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("exoscale instance id is required")
	}
	if c.DryRun {
		return nil
	}
	return c.request(ctx, http.MethodDelete, "/instance/"+url.PathEscape(id), nil, nil)
}

func (c Client) EnsureSecurityGroup(ctx context.Context, project, environment string, exoscale config.ExoscaleConfig) (SecurityGroup, error) {
	if !c.CredentialsPresent() {
		return SecurityGroup{}, fmt.Errorf("EXOSCALE_API_KEY/EXOSCALE_API_SECRET are required")
	}
	name := exoscale.SecurityGroup.Name
	if name == "" {
		name = "ship-" + project + "-" + environment
	}
	if exoscale.SecurityGroup.ID != "" && !exoscale.SecurityGroup.ManagedValue(true) {
		return SecurityGroup{ID: exoscale.SecurityGroup.ID, Name: name}, nil
	}
	groups, err := c.ListSecurityGroups(ctx)
	if err != nil {
		return SecurityGroup{}, err
	}
	for _, group := range groups {
		if group.Name == name {
			return group, nil
		}
	}
	description := exoscale.SecurityGroup.Description
	if description == "" {
		description = "Managed by Ship for " + project + "/" + environment
	}
	group, err := c.CreateSecurityGroup(ctx, name, description)
	if err != nil {
		return SecurityGroup{}, err
	}
	for _, rule := range securityGroupRules(exoscale.SSHAllowedCIDRs) {
		if err := c.AddSecurityGroupRule(ctx, group.ID, rule); err != nil {
			return SecurityGroup{}, err
		}
	}
	return group, nil
}

func (c Client) ListSecurityGroups(ctx context.Context) ([]SecurityGroup, error) {
	var out listSecurityGroupsResponse
	if err := c.request(ctx, http.MethodGet, "/security-group", nil, &out); err != nil {
		return nil, err
	}
	return out.SecurityGroups, nil
}

func (c Client) CreateSecurityGroup(ctx context.Context, name, description string) (SecurityGroup, error) {
	body := map[string]any{"name": name}
	if description != "" {
		body["description"] = description
	}
	var op Operation
	if err := c.request(ctx, http.MethodPost, "/security-group", body, &op); err != nil {
		return SecurityGroup{}, err
	}
	return SecurityGroup{ID: firstNonEmpty(op.Reference.ID, op.ID), Name: name, Description: description}, nil
}

func (c Client) AddSecurityGroupRule(ctx context.Context, securityGroupID string, rule SecurityRule) error {
	if strings.TrimSpace(securityGroupID) == "" {
		return fmt.Errorf("exoscale security group id is required")
	}
	return c.request(ctx, http.MethodPost, "/security-group/"+url.PathEscape(securityGroupID)+"/rules", rule, nil)
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
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", c.authorization(req, string(bodyBytes)))
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
		return fmt.Errorf("exoscale %s %s failed: %s", method, req.URL.Path, message)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode exoscale response: %w", err)
	}
	return nil
}

func (c Client) apiBase() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	zone := c.Zone
	if zone == "" {
		zone = defaultZone
	}
	return fmt.Sprintf(defaultAPIBaseFormat, zone)
}

func (c Client) authorization(req *http.Request, body string) string {
	expires := c.now().Add(5 * time.Minute).Unix()
	queryKeys, queryValues := signedQuery(req.URL.Query())
	message := strings.Join([]string{
		req.Method + " " + req.URL.EscapedPath(),
		body,
		queryValues,
		"",
		fmt.Sprintf("%d", expires),
	}, "\n")
	mac := hmac.New(sha256.New, []byte(c.APISecret))
	_, _ = mac.Write([]byte(message))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	parts := []string{
		"EXO2-HMAC-SHA256 credential=" + c.APIKey,
	}
	if len(queryKeys) > 0 {
		parts = append(parts, "signed-query-args="+strings.Join(queryKeys, ";"))
	}
	parts = append(parts,
		fmt.Sprintf("expires=%d", expires),
		"signature="+signature,
	)
	return strings.Join(parts, ",")
}

func (c Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func signedQuery(values url.Values) ([]string, string) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		for _, value := range values[key] {
			b.WriteString(value)
		}
	}
	return keys, b.String()
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

func createInstanceBody(plan provider.HostPlan, exoscale config.ExoscaleConfig, securityGroupIDs []string) map[string]any {
	body := map[string]any{
		"name":          plan.Name,
		"instance-type": map[string]string{"id": plan.Size},
		"template":      map[string]string{"id": plan.Image},
		"disk-size":     exoscale.EffectiveDiskSizeGB(defaultDiskSizeGB),
		"labels":        plan.Labels,
	}
	if assignment := exoscale.EffectivePublicIPAssignment(); assignment != "" {
		body["public-ip-assignment"] = assignment
	}
	if exoscale.AutoStart != nil {
		body["auto-start"] = *exoscale.AutoStart
	}
	if exoscale.SecureBoot != nil {
		body["secureboot-enabled"] = *exoscale.SecureBoot
	}
	if exoscale.TPM != nil {
		body["tpm-enabled"] = *exoscale.TPM
	}
	if exoscale.ApplicationConsistentSnapshot != nil {
		body["application-consistent-snapshot-enabled"] = *exoscale.ApplicationConsistentSnapshot
	}
	if plan.UserData != "" {
		body["user-data"] = base64.StdEncoding.EncodeToString([]byte(plan.UserData))
	}
	if exoscale.SSHKey != "" {
		body["ssh-key"] = map[string]string{"name": exoscale.SSHKey}
	}
	if len(exoscale.SSHKeys) > 0 {
		refs := make([]map[string]string, 0, len(exoscale.SSHKeys))
		for _, name := range exoscale.SSHKeys {
			refs = append(refs, map[string]string{"name": name})
		}
		body["ssh-keys"] = refs
	}
	if len(securityGroupIDs) > 0 {
		body["security-groups"] = idRefs(securityGroupIDs)
	}
	if len(exoscale.AntiAffinityGroups) > 0 {
		body["anti-affinity-groups"] = idRefs(exoscale.AntiAffinityGroups)
	}
	if exoscale.DeployTarget != "" {
		body["deploy-target"] = map[string]string{"id": exoscale.DeployTarget}
	}
	return body
}

func withPlanDefaults(plan provider.HostPlan, exoscale config.ExoscaleConfig) config.ExoscaleConfig {
	if plan.Location != "" {
		exoscale.Zone = plan.Location
	}
	if plan.Size != "" {
		exoscale.InstanceType = plan.Size
	}
	if plan.Image != "" {
		exoscale.Template = plan.Image
	}
	return exoscale
}

func idRefs(ids []string) []map[string]string {
	refs := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			continue
		}
		refs = append(refs, map[string]string{"id": id})
	}
	return refs
}

func securityGroupRules(sshAllowedCIDRs []string) []SecurityRule {
	rules := []SecurityRule{
		tcpRule("HTTP", "0.0.0.0/0", 80),
		tcpRule("HTTPS", "0.0.0.0/0", 443),
		udpRule("HTTP/3", "0.0.0.0/0", 443),
	}
	for _, cidr := range sshAllowedCIDRs {
		rules = append(rules, tcpRule("SSH", cidr, 22))
	}
	return rules
}

func tcpRule(description, network string, port int) SecurityRule {
	return SecurityRule{
		FlowDirection: "ingress",
		Description:   description,
		Network:       network,
		Protocol:      "tcp",
		StartPort:     port,
		EndPort:       port,
	}
}

func udpRule(description, network string, port int) SecurityRule {
	return SecurityRule{
		FlowDirection: "ingress",
		Description:   description,
		Network:       network,
		Protocol:      "udp",
		StartPort:     port,
		EndPort:       port,
	}
}

func instanceMatches(instance Instance, project, environment string) bool {
	return instance.Labels[LabelManagedBy] == "ship" &&
		instance.Labels[LabelProject] == project &&
		instance.Labels[LabelEnvironment] == environment
}

func hostFromInstance(instance Instance) provider.Host {
	pool := instance.Labels[LabelPool]
	return provider.Host{
		ID:            instance.ID,
		Name:          instance.Name,
		Pool:          pool,
		PublicAddress: firstNonEmpty(instance.PublicIP, instance.IPv6Address),
		Labels:        instance.Labels,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

type listInstancesResponse struct {
	Instances []Instance `json:"instances"`
}

type listSecurityGroupsResponse struct {
	SecurityGroups []SecurityGroup `json:"security-groups"`
}
