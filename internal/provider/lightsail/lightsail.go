package lightsail

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

const (
	apiTargetPrefix     = "Lightsail_20161128."
	service             = "lightsail"
	defaultWaitTimeout  = 10 * time.Minute
	defaultPollInterval = 5 * time.Second

	LabelManagedBy   = provider.LabelManagedBy
	LabelProject     = provider.LabelProject
	LabelEnvironment = provider.LabelEnvironment
	LabelPool        = provider.LabelPool
)

type Client struct {
	AccessKeyID       string
	SecretAccessKey   string
	SessionToken      string
	Region            string
	DryRun            bool
	HTTP              *http.Client
	BaseURL           string
	ForceDeleteAddOns *bool
	Now               func() time.Time
	Sleep             func(context.Context, time.Duration) error
}

type InstancePlan = provider.HostPlan

type Instance struct {
	ARN             string       `json:"arn"`
	Name            string       `json:"name"`
	BlueprintID     string       `json:"blueprintId"`
	BundleID        string       `json:"bundleId"`
	IPAddressType   string       `json:"ipAddressType"`
	PublicIPAddress string       `json:"publicIpAddress"`
	IPv6Addresses   []string     `json:"ipv6Addresses"`
	PrivateIP       string       `json:"privateIpAddress"`
	SSHKeyName      string       `json:"sshKeyName"`
	Username        string       `json:"username"`
	State           State        `json:"state"`
	Location        Location     `json:"location"`
	Tags            []Tag        `json:"tags"`
	Networking      Networking   `json:"networking"`
	AddOns          []AddOnState `json:"addOns"`
}

type State struct {
	Code int    `json:"code"`
	Name string `json:"name"`
}

type Location struct {
	AvailabilityZone string `json:"availabilityZone"`
	RegionName       string `json:"regionName"`
}

type Networking struct {
	Ports []PortInfo `json:"ports"`
}

type PortInfo struct {
	CIDRs     []string `json:"cidrs,omitempty"`
	IPv6CIDRs []string `json:"ipv6Cidrs,omitempty"`
	FromPort  int      `json:"fromPort"`
	Protocol  string   `json:"protocol"`
	ToPort    int      `json:"toPort"`
}

type Tag struct {
	Key   string `json:"key"`
	Value string `json:"value,omitempty"`
}

type AddOnRequest struct {
	AddOnType                 string                     `json:"addOnType"`
	AutoSnapshotAddOnRequest  *AutoSnapshotAddOnRequest  `json:"autoSnapshotAddOnRequest,omitempty"`
	StopInstanceOnIdleRequest *StopInstanceOnIdleRequest `json:"stopInstanceOnIdleRequest,omitempty"`
}

type AutoSnapshotAddOnRequest struct {
	SnapshotTimeOfDay string `json:"snapshotTimeOfDay,omitempty"`
}

type StopInstanceOnIdleRequest struct {
	Duration  string `json:"duration,omitempty"`
	Threshold string `json:"threshold,omitempty"`
}

type AddOnState struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type Operation struct {
	ID              string   `json:"id"`
	ResourceName    string   `json:"resourceName"`
	ResourceType    string   `json:"resourceType"`
	OperationType   string   `json:"operationType"`
	Status          string   `json:"status"`
	IsTerminal      bool     `json:"isTerminal"`
	ErrorCode       string   `json:"errorCode"`
	ErrorDetails    string   `json:"errorDetails"`
	Location        Location `json:"location"`
	OperationDetail string   `json:"operationDetails"`
}

type ReconcileResult = provider.ReconcileResult

func NewFromEnv(dryRun bool, configs ...config.LightsailConfig) Client {
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = os.Getenv("AWS_DEFAULT_REGION")
	}
	if len(configs) > 0 && strings.TrimSpace(configs[0].Region) != "" {
		region = strings.TrimSpace(configs[0].Region)
	}
	var forceDeleteAddOns *bool
	if len(configs) > 0 {
		forceDeleteAddOns = configs[0].ForceDeleteAddOns
	}
	return Client{
		AccessKeyID:       os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey:   os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken:      os.Getenv("AWS_SESSION_TOKEN"),
		Region:            region,
		DryRun:            dryRun,
		HTTP:              http.DefaultClient,
		ForceDeleteAddOns: forceDeleteAddOns,
	}
}

func (c Client) Name() string {
	return config.ProviderLightsail
}

func (c Client) CredentialChecks(lookupEnv func(string) (string, bool)) []provider.CredentialCheck {
	_, accessOK := lookupEnv("AWS_ACCESS_KEY_ID")
	_, secretOK := lookupEnv("AWS_SECRET_ACCESS_KEY")
	return []provider.CredentialCheck{
		{
			Name:           "aws access key",
			Present:        accessOK,
			Required:       true,
			PresentMessage: "AWS_ACCESS_KEY_ID is set",
			MissingMessage: "missing AWS_ACCESS_KEY_ID",
		},
		{
			Name:           "aws secret key",
			Present:        secretOK,
			Required:       true,
			PresentMessage: "AWS_SECRET_ACCESS_KEY is set",
			MissingMessage: "missing AWS_SECRET_ACCESS_KEY",
		},
	}
}

func (c Client) credentialsPresent() bool {
	return strings.TrimSpace(c.AccessKeyID) != "" && strings.TrimSpace(c.SecretAccessKey) != ""
}

func DesiredInstances(env config.Environment) []provider.HostPlan {
	return DesiredInstancesFor("", "", env)
}

func DesiredInstancesFor(project, environment string, env config.Environment) []provider.HostPlan {
	lightsail := env.Provider.Lightsail
	if lightsail == nil {
		return nil
	}
	return provider.HostPlans(project, environment, env, provider.HostPlanOptions{
		Location: lightsail.AvailabilityZone,
		Size:     lightsail.BundleID,
		Image:    lightsail.BlueprintID,
		UserData: lightsail.UserData,
	})
}

func (c Client) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.Lightsail == nil {
		return nil, fmt.Errorf("environment %q must define provider.lightsail", environment)
	}
	return DesiredInstancesFor(project, environment, env), nil
}

func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	if env.Provider.Lightsail == nil {
		return provider.ReconcileResult{}, fmt.Errorf("environment %q must define provider.lightsail", environment)
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
	lightsail := *env.Provider.Lightsail
	result, err := provider.ReconcileHosts(ctx, project, environment, desired, reconcileBackend{client: c, lightsail: lightsail})
	if err != nil {
		return provider.ReconcileResult{}, err
	}
	if lightsail.Firewall.ManagedValue(true) {
		for _, host := range append(append([]provider.Host{}, result.Existing...), result.Created...) {
			if err := c.PutInstancePublicPorts(ctx, host.Name, lightsail); err != nil {
				return provider.ReconcileResult{}, err
			}
		}
	}
	return result, nil
}

// CreateHost provisions a single instance using the same backend Reconcile
// builds, so `ship migrate` can add a replacement alongside the existing one.
// It also applies the managed public-port rules Reconcile would apply.
func (c Client) CreateHost(ctx context.Context, project, environment string, env config.Environment, plan provider.HostPlan) (provider.Host, error) {
	if env.Provider.Lightsail == nil {
		return provider.Host{}, fmt.Errorf("environment %q must define provider.lightsail", environment)
	}
	lightsail := *env.Provider.Lightsail
	host, err := reconcileBackend{client: c, lightsail: lightsail}.Create(ctx, plan)
	if err != nil {
		return provider.Host{}, err
	}
	if lightsail.Firewall.ManagedValue(true) {
		if err := c.PutInstancePublicPorts(ctx, host.Name, lightsail); err != nil {
			return provider.Host{}, err
		}
	}
	return host, nil
}

var _ provider.HostCreator = Client{}

type reconcileBackend struct {
	client    Client
	lightsail config.LightsailConfig
}

func (b reconcileBackend) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	return b.client.List(ctx, project, environment)
}

func (b reconcileBackend) Create(ctx context.Context, plan provider.HostPlan) (provider.Host, error) {
	instance, err := b.client.CreateInstance(ctx, plan, b.lightsail)
	if err != nil {
		return provider.Host{}, err
	}
	return hostFromInstance(instance), nil
}

func (c Client) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	instances, err := c.GetInstances(ctx, project, environment)
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
	name := firstNonEmpty(host.ID, host.Name)
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("lightsail instance name is required")
	}
	force := c.ForceDeleteAddOns
	if force == nil {
		value := true
		force = &value
	}
	return c.DeleteInstance(ctx, name, force)
}

func (c Client) GetInstances(ctx context.Context, project, environment string) ([]Instance, error) {
	if !c.credentialsPresent() {
		return nil, fmt.Errorf("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are required")
	}
	var instances []Instance
	body := map[string]any{}
	for {
		var out getInstancesResponse
		if err := c.do(ctx, "GetInstances", body, &out); err != nil {
			return nil, err
		}
		for _, instance := range out.Instances {
			tags := tagsFromItems(instance.Tags)
			if tags[LabelManagedBy] != "ship" || tags[LabelProject] != project || tags[LabelEnvironment] != environment {
				continue
			}
			instances = append(instances, instance)
		}
		if strings.TrimSpace(out.NextPageToken) == "" {
			break
		}
		body["pageToken"] = out.NextPageToken
	}
	sort.SliceStable(instances, func(i, j int) bool {
		left := tagsFromItems(instances[i].Tags)
		right := tagsFromItems(instances[j].Tags)
		if left[LabelPool] != right[LabelPool] {
			return left[LabelPool] < right[LabelPool]
		}
		return instances[i].Name < instances[j].Name
	})
	return instances, nil
}

func (c Client) CreateInstance(ctx context.Context, plan provider.HostPlan, lightsail config.LightsailConfig) (Instance, error) {
	if !c.credentialsPresent() {
		return Instance{}, fmt.Errorf("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are required")
	}
	body := createInstanceBody(plan, lightsail)
	if c.DryRun {
		return Instance{Name: plan.Name, PublicIPAddress: plan.Name, Tags: tagsForPlan(plan)}, nil
	}
	var out createInstancesResponse
	if err := c.do(ctx, "CreateInstances", body, &out); err != nil {
		return Instance{}, err
	}
	return c.WaitForInstance(ctx, plan.Name, lightsail)
}

func (c Client) WaitForInstance(ctx context.Context, name string, lightsail config.LightsailConfig) (Instance, error) {
	deadline := time.Now().Add(waitTimeout(lightsail))
	var lastErr error
	for {
		instance, ok, err := c.findInstance(ctx, name)
		if err != nil {
			lastErr = err
		} else if ok && strings.TrimSpace(instance.PublicIPAddress) != "" {
			return instance, nil
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return Instance{}, fmt.Errorf("wait for Lightsail instance %q: %w", name, lastErr)
			}
			return Instance{}, fmt.Errorf("timed out waiting for Lightsail instance %q public address", name)
		}
		if err := c.sleep(ctx, pollInterval(lightsail)); err != nil {
			return Instance{}, err
		}
	}
}

func (c Client) findInstance(ctx context.Context, name string) (Instance, bool, error) {
	body := map[string]any{}
	for {
		var out getInstancesResponse
		if err := c.do(ctx, "GetInstances", body, &out); err != nil {
			return Instance{}, false, err
		}
		for _, instance := range out.Instances {
			if instance.Name == name {
				return instance, true, nil
			}
		}
		if strings.TrimSpace(out.NextPageToken) == "" {
			break
		}
		body["pageToken"] = out.NextPageToken
	}
	return Instance{}, false, nil
}

func (c Client) PutInstancePublicPorts(ctx context.Context, name string, lightsail config.LightsailConfig) error {
	body := map[string]any{
		"instanceName": name,
		"portInfos":    portInfos(lightsail),
	}
	return c.do(ctx, "PutInstancePublicPorts", body, nil)
}

func (c Client) DeleteInstance(ctx context.Context, name string, forceDeleteAddOns *bool) error {
	body := map[string]any{"instanceName": name}
	if forceDeleteAddOns != nil {
		body["forceDeleteAddOns"] = *forceDeleteAddOns
	}
	if c.DryRun {
		return nil
	}
	return c.do(ctx, "DeleteInstance", body, nil)
}

func (c Client) do(ctx context.Context, action string, body any, out any) error {
	if !c.credentialsPresent() {
		return fmt.Errorf("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are required")
	}
	region := c.Region
	if strings.TrimSpace(region) == "" {
		return fmt.Errorf("aws region is required")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(region), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", apiTargetPrefix+action)
	c.sign(req, region, data)
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respData, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("aws lightsail %s failed: %s", action, strings.TrimSpace(string(respData)))
	}
	if out == nil || len(bytes.TrimSpace(respData)) == 0 {
		return nil
	}
	if err := json.Unmarshal(respData, out); err != nil {
		return fmt.Errorf("decode Lightsail response: %w", err)
	}
	return nil
}

func (c Client) endpoint(region string) string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return "https://lightsail." + region + ".amazonaws.com"
}

func (c Client) sign(req *http.Request, region string, payload []byte) {
	now := time.Now().UTC()
	if c.Now != nil {
		now = c.Now().UTC()
	}
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	payloadHash := sha256Hex(payload)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if c.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", c.SessionToken)
	}
	host := req.URL.Host
	if req.Host != "" {
		host = req.Host
	}
	canonicalHeaders := "content-type:" + req.Header.Get("Content-Type") + "\n" +
		"host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "content-type;host;x-amz-content-sha256;x-amz-date"
	if c.SessionToken != "" {
		canonicalHeaders += "x-amz-security-token:" + c.SessionToken + "\n"
		signedHeaders += ";x-amz-security-token"
	}
	canonicalHeaders += "x-amz-target:" + req.Header.Get("X-Amz-Target") + "\n"
	signedHeaders += ";x-amz-target"
	canonicalURI := req.URL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		req.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
	scope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")
	signature := hex.EncodeToString(hmacSHA256(signingKey(c.SecretAccessKey, dateStamp, region), []byte(stringToSign)))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKeyID+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func createInstanceBody(plan provider.HostPlan, lightsail config.LightsailConfig) map[string]any {
	body := map[string]any{
		"availabilityZone": plan.Location,
		"blueprintId":      plan.Image,
		"bundleId":         plan.Size,
		"instanceNames":    []string{plan.Name},
		"tags":             tagsForPlan(plan),
	}
	if strings.TrimSpace(lightsail.KeyPairName) != "" {
		body["keyPairName"] = strings.TrimSpace(lightsail.KeyPairName)
	}
	if strings.TrimSpace(lightsail.IPAddressType) != "" {
		body["ipAddressType"] = strings.TrimSpace(lightsail.IPAddressType)
	}
	if strings.TrimSpace(plan.UserData) != "" {
		body["userData"] = plan.UserData
	}
	if len(lightsail.AddOns) > 0 {
		body["addOns"] = addOnRequests(lightsail.AddOns)
	}
	return body
}

func addOnRequests(values []config.LightsailAddOn) []AddOnRequest {
	out := make([]AddOnRequest, 0, len(values))
	for _, value := range values {
		req := AddOnRequest{AddOnType: value.Type}
		if value.Type == "AutoSnapshot" && strings.TrimSpace(value.SnapshotTimeOfDay) != "" {
			req.AutoSnapshotAddOnRequest = &AutoSnapshotAddOnRequest{SnapshotTimeOfDay: strings.TrimSpace(value.SnapshotTimeOfDay)}
		}
		if value.Type == "StopInstanceOnIdle" && (strings.TrimSpace(value.StopDuration) != "" || strings.TrimSpace(value.StopThreshold) != "") {
			req.StopInstanceOnIdleRequest = &StopInstanceOnIdleRequest{
				Duration:  strings.TrimSpace(value.StopDuration),
				Threshold: strings.TrimSpace(value.StopThreshold),
			}
		}
		out = append(out, req)
	}
	return out
}

func portInfos(lightsail config.LightsailConfig) []PortInfo {
	var ports []PortInfo
	if lightsail.EffectiveSSHFirewall() == config.SSHFirewallManaged {
		ports = appendCIDRPorts(ports, 22, lightsail.SSHAllowedCIDRs)
	}
	ports = appendCIDRPorts(ports, 80, []string{"0.0.0.0/0", "::/0"})
	ports = appendCIDRPorts(ports, 443, []string{"0.0.0.0/0", "::/0"})
	ports = appendCIDRPortsWithProtocol(ports, 443, "udp", []string{"0.0.0.0/0", "::/0"})
	return ports
}

func appendCIDRPorts(ports []PortInfo, port int, cidrs []string) []PortInfo {
	return appendCIDRPortsWithProtocol(ports, port, "tcp", cidrs)
}

func appendCIDRPortsWithProtocol(ports []PortInfo, port int, protocol string, cidrs []string) []PortInfo {
	info := PortInfo{FromPort: port, ToPort: port, Protocol: protocol}
	for _, cidr := range cidrs {
		if strings.Contains(cidr, ":") {
			info.IPv6CIDRs = append(info.IPv6CIDRs, cidr)
		} else {
			info.CIDRs = append(info.CIDRs, cidr)
		}
	}
	return append(ports, info)
}

func tagsForPlan(plan provider.HostPlan) []Tag {
	labels := plan.Labels
	if len(labels) == 0 {
		labels = provider.ShipLabels(plan.Project, plan.Environment, plan.Pool)
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	tags := make([]Tag, 0, len(keys))
	for _, key := range keys {
		tags = append(tags, Tag{Key: key, Value: labels[key]})
	}
	return tags
}

func tagsFromItems(items []Tag) map[string]string {
	tags := map[string]string{}
	for _, item := range items {
		tags[item.Key] = item.Value
	}
	return tags
}

func hostFromInstance(instance Instance) provider.Host {
	tags := tagsFromItems(instance.Tags)
	return provider.Host{
		ID:            instance.Name,
		Name:          instance.Name,
		Pool:          tags[LabelPool],
		PublicAddress: firstNonEmpty(instance.PublicIPAddress, firstIPv6(instance)),
		Labels:        tags,
	}
}

func firstIPv6(instance Instance) string {
	for _, ip := range instance.IPv6Addresses {
		if strings.TrimSpace(ip) != "" {
			return strings.TrimSpace(ip)
		}
	}
	return ""
}

func waitTimeout(lightsail config.LightsailConfig) time.Duration {
	if lightsail.WaitTimeoutSeconds > 0 {
		return time.Duration(lightsail.WaitTimeoutSeconds) * time.Second
	}
	return defaultWaitTimeout
}

func pollInterval(lightsail config.LightsailConfig) time.Duration {
	if lightsail.PollIntervalSeconds > 0 {
		return time.Duration(lightsail.PollIntervalSeconds) * time.Second
	}
	return defaultPollInterval
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

func signingKey(secret, dateStamp, region string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

type getInstancesResponse struct {
	Instances     []Instance `json:"instances"`
	NextPageToken string     `json:"nextPageToken"`
}

type createInstancesResponse struct {
	Operations []Operation `json:"operations"`
}
