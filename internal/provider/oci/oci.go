package oci

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
	apiVersion = "20160918"

	LabelManagedBy   = provider.LabelManagedBy
	LabelProject     = provider.LabelProject
	LabelEnvironment = provider.LabelEnvironment
	LabelPool        = provider.LabelPool
)

type Client struct {
	TenancyOCID    string
	UserOCID       string
	Fingerprint    string
	PrivateKeyPEM  string
	PrivateKeyFile string
	Region         string
	CompartmentID  string
	PreserveBoot   bool
	DryRun         bool
	HTTP           *http.Client
	BaseURL        string
	Now            func() time.Time
}

type Instance struct {
	ID                 string            `json:"id"`
	DisplayName        string            `json:"displayName"`
	LifecycleState     string            `json:"lifecycleState"`
	AvailabilityDomain string            `json:"availabilityDomain"`
	FreeformTags       map[string]string `json:"freeformTags"`
	PublicIP           string            `json:"-"`
}

type VNICAttachment struct {
	ID             string `json:"id"`
	InstanceID     string `json:"instanceId"`
	VNICID         string `json:"vnicId"`
	LifecycleState string `json:"lifecycleState"`
}

type VNIC struct {
	ID        string `json:"id"`
	PublicIP  string `json:"publicIp"`
	PrivateIP string `json:"privateIp"`
}

type NetworkSecurityGroup struct {
	ID           string            `json:"id"`
	DisplayName  string            `json:"displayName"`
	FreeformTags map[string]string `json:"freeformTags"`
}

type SecurityRule struct {
	ID              string      `json:"id,omitempty"`
	Direction       string      `json:"direction"`
	Protocol        string      `json:"protocol"`
	Source          string      `json:"source,omitempty"`
	SourceType      string      `json:"sourceType,omitempty"`
	Destination     string      `json:"destination,omitempty"`
	DestinationType string      `json:"destinationType,omitempty"`
	TCPOptions      *TCPOptions `json:"tcpOptions,omitempty"`
	UDPOptions      *UDPOptions `json:"udpOptions,omitempty"`
	Description     string      `json:"description,omitempty"`
	IsStateless     bool        `json:"isStateless"`
}

type TCPOptions struct {
	DestinationPortRange PortRange `json:"destinationPortRange"`
}

type UDPOptions struct {
	DestinationPortRange PortRange `json:"destinationPortRange"`
}

type PortRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

type ReconcileResult = provider.ReconcileResult

func NewFromEnv(dryRun bool, configs ...config.OCIConfig) Client {
	cfg := config.OCIConfig{}
	if len(configs) > 0 {
		cfg = configs[0]
	}
	return Client{
		TenancyOCID:    os.Getenv("OCI_TENANCY_OCID"),
		UserOCID:       os.Getenv("OCI_USER_OCID"),
		Fingerprint:    os.Getenv("OCI_FINGERPRINT"),
		PrivateKeyPEM:  os.Getenv("OCI_PRIVATE_KEY"),
		PrivateKeyFile: firstNonEmpty(os.Getenv("OCI_PRIVATE_KEY_FILE"), cfg.PrivateKeyFile),
		Region:         cfg.Region,
		CompartmentID:  cfg.CompartmentID,
		PreserveBoot:   cfg.PreserveBootVolumeValue(false),
		DryRun:         dryRun,
		HTTP:           http.DefaultClient,
	}
}

func (c Client) Name() string {
	return config.ProviderOCI
}

func (c Client) CredentialsPresent() bool {
	return strings.TrimSpace(c.TenancyOCID) != "" &&
		strings.TrimSpace(c.UserOCID) != "" &&
		strings.TrimSpace(c.Fingerprint) != "" &&
		(strings.TrimSpace(c.PrivateKeyPEM) != "" || strings.TrimSpace(c.PrivateKeyFile) != "")
}

func (c Client) CredentialChecks(lookupEnv func(string) (string, bool)) []provider.CredentialCheck {
	_, tenancyOK := lookupEnv("OCI_TENANCY_OCID")
	_, userOK := lookupEnv("OCI_USER_OCID")
	_, fingerprintOK := lookupEnv("OCI_FINGERPRINT")
	_, keyOK := lookupEnv("OCI_PRIVATE_KEY")
	_, keyFileOK := lookupEnv("OCI_PRIVATE_KEY_FILE")
	return []provider.CredentialCheck{
		{
			Name:           "oci identity",
			Present:        tenancyOK && userOK && fingerprintOK,
			Required:       true,
			PresentMessage: "OCI_TENANCY_OCID/OCI_USER_OCID/OCI_FINGERPRINT are set",
			MissingMessage: "missing OCI_TENANCY_OCID/OCI_USER_OCID/OCI_FINGERPRINT",
		},
		{
			Name:           "oci private key",
			Present:        keyOK || keyFileOK,
			Required:       true,
			PresentMessage: "OCI_PRIVATE_KEY or OCI_PRIVATE_KEY_FILE is set",
			MissingMessage: "missing OCI_PRIVATE_KEY or OCI_PRIVATE_KEY_FILE",
		},
	}
}

func DesiredInstances(env config.Environment) []provider.HostPlan {
	return DesiredInstancesFor("", "", env)
}

func DesiredInstancesFor(project, environment string, env config.Environment) []provider.HostPlan {
	oci := env.Provider.OCI
	if oci == nil {
		return nil
	}
	return provider.HostPlans(project, environment, env, provider.HostPlanOptions{
		Location: oci.AvailabilityDomain,
		Size:     oci.Shape,
		Image:    oci.ImageID,
		UserData: oci.UserData,
	})
}

func (c Client) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.OCI == nil {
		return nil, fmt.Errorf("environment %q must define provider.oci", environment)
	}
	return DesiredInstancesFor(project, environment, env), nil
}

func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	if env.Provider.OCI == nil {
		return provider.ReconcileResult{}, fmt.Errorf("environment %q must define provider.oci", environment)
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

	ociConfig := *env.Provider.OCI
	c.Region = firstNonEmpty(ociConfig.Region, c.Region)
	c.CompartmentID = firstNonEmpty(ociConfig.CompartmentID, c.CompartmentID)
	networkSecurityGroupIDs := append([]string(nil), ociConfig.NSGIDs...)
	if ociConfig.NetworkSecurityGroup.ManagedValue(true) {
		group, err := c.EnsureNetworkSecurityGroup(ctx, project, environment, ociConfig)
		if err != nil {
			return provider.ReconcileResult{}, err
		}
		networkSecurityGroupIDs = append(networkSecurityGroupIDs, group.ID)
	} else if ociConfig.NetworkSecurityGroup.ID != "" {
		networkSecurityGroupIDs = append(networkSecurityGroupIDs, ociConfig.NetworkSecurityGroup.ID)
	}

	return provider.ReconcileHosts(ctx, project, environment, desired, reconcileBackend{
		client:                  c,
		oci:                     ociConfig,
		networkSecurityGroupIDs: networkSecurityGroupIDs,
	})
}

type reconcileBackend struct {
	client                  Client
	oci                     config.OCIConfig
	networkSecurityGroupIDs []string
}

func (b reconcileBackend) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	return b.client.List(ctx, project, environment)
}

func (b reconcileBackend) Create(ctx context.Context, plan provider.HostPlan) (provider.Host, error) {
	instance, err := b.client.LaunchInstance(ctx, plan, b.oci, b.networkSecurityGroupIDs)
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
		return fmt.Errorf("oci instance id is required")
	}
	return c.TerminateInstance(ctx, host.ID, c.PreserveBoot)
}

func (c Client) ListInstances(ctx context.Context, project, environment string) ([]Instance, error) {
	if err := c.checkReady(); err != nil {
		return nil, err
	}
	values := url.Values{}
	values.Set("compartmentId", c.CompartmentID)
	page := ""
	var instances []Instance
	for {
		if page != "" {
			values.Set("page", page)
		}
		var out []Instance
		next, err := c.request(ctx, http.MethodGet, "/"+apiVersion+"/instances?"+values.Encode(), nil, &out)
		if err != nil {
			return nil, err
		}
		for _, instance := range out {
			if !instanceMatches(instance, project, environment) {
				continue
			}
			if strings.EqualFold(instance.LifecycleState, "TERMINATED") ||
				strings.EqualFold(instance.LifecycleState, "TERMINATING") {
				continue
			}
			instance.PublicIP = c.instancePublicIP(ctx, instance.ID)
			instances = append(instances, instance)
		}
		if next == "" {
			break
		}
		page = next
	}
	sort.SliceStable(instances, func(i, j int) bool {
		if instances[i].FreeformTags[LabelPool] != instances[j].FreeformTags[LabelPool] {
			return instances[i].FreeformTags[LabelPool] < instances[j].FreeformTags[LabelPool]
		}
		return instances[i].DisplayName < instances[j].DisplayName
	})
	return instances, nil
}

func (c Client) LaunchInstance(ctx context.Context, plan provider.HostPlan, oci config.OCIConfig, networkSecurityGroupIDs []string) (Instance, error) {
	if err := c.checkReady(); err != nil {
		return Instance{}, err
	}
	oci = withPlanDefaults(plan, oci)
	body := launchInstanceBody(plan, oci, networkSecurityGroupIDs)
	if c.DryRun {
		return Instance{ID: plan.Name, DisplayName: plan.Name, FreeformTags: freeformTags(plan, oci)}, nil
	}
	var out Instance
	if _, err := c.request(ctx, http.MethodPost, "/"+apiVersion+"/instances", body, &out); err != nil {
		return Instance{}, err
	}
	out.PublicIP = c.instancePublicIP(ctx, out.ID)
	return out, nil
}

func (c Client) TerminateInstance(ctx context.Context, id string, preserveBootVolume bool) error {
	values := url.Values{}
	values.Set("preserveBootVolume", strconv.FormatBool(preserveBootVolume))
	_, err := c.request(ctx, http.MethodDelete, "/"+apiVersion+"/instances/"+url.PathEscape(id)+"?"+values.Encode(), nil, nil)
	return err
}

func (c Client) EnsureNetworkSecurityGroup(ctx context.Context, project, environment string, oci config.OCIConfig) (NetworkSecurityGroup, error) {
	if strings.TrimSpace(oci.NetworkSecurityGroup.ID) != "" {
		return NetworkSecurityGroup{ID: oci.NetworkSecurityGroup.ID}, nil
	}
	group, ok, err := c.FindNetworkSecurityGroup(ctx, project, environment, oci)
	if err != nil {
		return NetworkSecurityGroup{}, err
	}
	if !ok {
		group, err = c.CreateNetworkSecurityGroup(ctx, project, environment, oci)
		if err != nil {
			return NetworkSecurityGroup{}, err
		}
	}
	if err := c.EnsureNetworkSecurityGroupRules(ctx, group.ID, oci); err != nil {
		return NetworkSecurityGroup{}, err
	}
	return group, nil
}

func (c Client) FindNetworkSecurityGroup(ctx context.Context, project, environment string, oci config.OCIConfig) (NetworkSecurityGroup, bool, error) {
	values := url.Values{}
	values.Set("compartmentId", oci.CompartmentID)
	values.Set("vcnId", oci.NetworkSecurityGroup.VCNID)
	if name := networkSecurityGroupName(project, environment, oci); name != "" {
		values.Set("displayName", name)
	}
	var out []NetworkSecurityGroup
	if _, err := c.request(ctx, http.MethodGet, "/"+apiVersion+"/networkSecurityGroups?"+values.Encode(), nil, &out); err != nil {
		return NetworkSecurityGroup{}, false, err
	}
	for _, group := range out {
		if group.FreeformTags[LabelManagedBy] == "ship" &&
			group.FreeformTags[LabelProject] == project &&
			group.FreeformTags[LabelEnvironment] == environment {
			return group, true, nil
		}
	}
	return NetworkSecurityGroup{}, false, nil
}

func (c Client) CreateNetworkSecurityGroup(ctx context.Context, project, environment string, oci config.OCIConfig) (NetworkSecurityGroup, error) {
	body := map[string]any{
		"compartmentId": oci.CompartmentID,
		"vcnId":         oci.NetworkSecurityGroup.VCNID,
		"displayName":   networkSecurityGroupName(project, environment, oci),
		"freeformTags":  provider.ShipLabels(project, environment, "network"),
	}
	var out NetworkSecurityGroup
	if _, err := c.request(ctx, http.MethodPost, "/"+apiVersion+"/networkSecurityGroups", body, &out); err != nil {
		return NetworkSecurityGroup{}, err
	}
	return out, nil
}

func (c Client) EnsureNetworkSecurityGroupRules(ctx context.Context, id string, oci config.OCIConfig) error {
	existing, err := c.ListNetworkSecurityGroupRules(ctx, id)
	if err != nil {
		return err
	}
	var missing []SecurityRule
	for _, rule := range networkSecurityGroupRules(oci) {
		if containsSecurityRule(existing, rule) {
			continue
		}
		missing = append(missing, rule)
	}
	if len(missing) == 0 {
		return nil
	}
	body := map[string]any{"securityRules": missing}
	_, err = c.request(ctx, http.MethodPost, "/"+apiVersion+"/networkSecurityGroups/"+url.PathEscape(id)+"/actions/addSecurityRules", body, nil)
	return err
}

func (c Client) ListNetworkSecurityGroupRules(ctx context.Context, id string) ([]SecurityRule, error) {
	var out []SecurityRule
	_, err := c.request(ctx, http.MethodGet, "/"+apiVersion+"/networkSecurityGroups/"+url.PathEscape(id)+"/securityRules", nil, &out)
	return out, err
}

func (c Client) instancePublicIP(ctx context.Context, instanceID string) string {
	attachments, err := c.ListVNICAttachments(ctx, instanceID)
	if err != nil {
		return ""
	}
	for _, attachment := range attachments {
		if attachment.VNICID == "" || strings.EqualFold(attachment.LifecycleState, "DETACHED") {
			continue
		}
		vnic, err := c.GetVNIC(ctx, attachment.VNICID)
		if err == nil && vnic.PublicIP != "" {
			return vnic.PublicIP
		}
	}
	return ""
}

func (c Client) ListVNICAttachments(ctx context.Context, instanceID string) ([]VNICAttachment, error) {
	values := url.Values{}
	values.Set("compartmentId", c.CompartmentID)
	values.Set("instanceId", instanceID)
	var out []VNICAttachment
	_, err := c.request(ctx, http.MethodGet, "/"+apiVersion+"/vnicAttachments?"+values.Encode(), nil, &out)
	return out, err
}

func (c Client) GetVNIC(ctx context.Context, id string) (VNIC, error) {
	var out VNIC
	_, err := c.request(ctx, http.MethodGet, "/"+apiVersion+"/vnics/"+url.PathEscape(id), nil, &out)
	return out, err
}

func (c Client) request(ctx context.Context, method, path string, payload any, out any) (string, error) {
	base := strings.TrimRight(firstNonEmpty(c.BaseURL, endpointForRegion(c.Region)), "/")
	var body []byte
	var reader io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return "", err
		}
		body = data
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return "", err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := c.sign(req, body); err != nil {
		return "", err
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return "", fmt.Errorf("oci api %s %s failed: %s: %s", method, path, res.Status, strings.TrimSpace(string(data)))
	}
	if out != nil && res.StatusCode != http.StatusNoContent && res.ContentLength != 0 {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil && err != io.EOF {
			return "", err
		}
	}
	return res.Header.Get("opc-next-page"), nil
}

func (c Client) sign(req *http.Request, body []byte) error {
	key, err := c.privateKey()
	if err != nil {
		return err
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	req.Header.Set("Date", now().UTC().Format(http.TimeFormat))
	req.Host = req.URL.Host
	req.Header.Set("Accept", "application/json")
	headers := []string{"(request-target)", "host", "date"}
	if body != nil {
		sum := sha256.Sum256(body)
		req.Header.Set("X-Content-Sha256", base64.StdEncoding.EncodeToString(sum[:]))
		req.Header.Set("Content-Length", strconv.Itoa(len(body)))
		headers = append(headers, "x-content-sha256", "content-type", "content-length")
	}
	signingString := c.signingString(req, headers)
	hash := sha256.Sum256([]byte(signingString))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf(
		`Signature version="1",keyId="%s",algorithm="rsa-sha256",headers="%s",signature="%s"`,
		c.keyID(),
		strings.Join(headers, " "),
		base64.StdEncoding.EncodeToString(signature),
	))
	return nil
}

func (c Client) signingString(req *http.Request, headers []string) string {
	var lines []string
	for _, header := range headers {
		switch header {
		case "(request-target)":
			target := req.URL.EscapedPath()
			if req.URL.RawQuery != "" {
				target += "?" + req.URL.RawQuery
			}
			lines = append(lines, "(request-target): "+strings.ToLower(req.Method)+" "+target)
		case "host":
			host := req.Host
			if host == "" {
				host = req.URL.Host
			}
			lines = append(lines, "host: "+host)
		default:
			lines = append(lines, header+": "+req.Header.Get(http.CanonicalHeaderKey(header)))
		}
	}
	return strings.Join(lines, "\n")
}

func (c Client) privateKey() (*rsa.PrivateKey, error) {
	data := []byte(strings.TrimSpace(c.PrivateKeyPEM))
	if len(data) == 0 && strings.TrimSpace(c.PrivateKeyFile) != "" {
		fileData, err := os.ReadFile(c.PrivateKeyFile)
		if err != nil {
			return nil, err
		}
		data = fileData
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("invalid OCI private key PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("OCI private key must be RSA")
	}
	return key, nil
}

func (c Client) keyID() string {
	return c.TenancyOCID + "/" + c.UserOCID + "/" + c.Fingerprint
}

func (c Client) checkReady() error {
	if !c.CredentialsPresent() {
		return fmt.Errorf("OCI_TENANCY_OCID/OCI_USER_OCID/OCI_FINGERPRINT and OCI_PRIVATE_KEY or OCI_PRIVATE_KEY_FILE are required")
	}
	if strings.TrimSpace(c.Region) == "" {
		return fmt.Errorf("oci region is required")
	}
	if strings.TrimSpace(c.CompartmentID) == "" {
		return fmt.Errorf("oci compartment_id is required")
	}
	return nil
}

func launchInstanceBody(plan provider.HostPlan, oci config.OCIConfig, networkSecurityGroupIDs []string) map[string]any {
	body := map[string]any{
		"availabilityDomain": oci.AvailabilityDomain,
		"compartmentId":      oci.CompartmentID,
		"displayName":        plan.Name,
		"shape":              oci.Shape,
		"sourceDetails": map[string]any{
			"sourceType": "image",
			"imageId":    oci.ImageID,
		},
		"createVnicDetails": map[string]any{
			"subnetId":       oci.SubnetID,
			"assignPublicIp": oci.AssignPublicIPValue(true),
		},
		"metadata":     metadata(plan, oci),
		"freeformTags": freeformTags(plan, oci),
	}
	if oci.BootVolumeSizeGB > 0 {
		body["sourceDetails"].(map[string]any)["bootVolumeSizeInGBs"] = oci.BootVolumeSizeGB
	}
	if len(networkSecurityGroupIDs) > 0 {
		body["createVnicDetails"].(map[string]any)["nsgIds"] = uniqueStrings(networkSecurityGroupIDs)
	}
	if shapeConfig := launchShapeConfig(oci); len(shapeConfig) > 0 {
		body["shapeConfig"] = shapeConfig
	}
	return body
}

func metadata(plan provider.HostPlan, oci config.OCIConfig) map[string]string {
	out := map[string]string{}
	for key, value := range oci.Metadata {
		if key != "" && value != "" {
			out[key] = value
		}
	}
	if len(oci.SSHAuthorizedKeys) > 0 {
		out["ssh_authorized_keys"] = strings.Join(oci.SSHAuthorizedKeys, "\n")
	}
	if strings.TrimSpace(oci.UserData) != "" {
		out["user_data"] = base64.StdEncoding.EncodeToString([]byte(oci.UserData))
	}
	return out
}

func freeformTags(plan provider.HostPlan, oci config.OCIConfig) map[string]string {
	tags := map[string]string{}
	for key, value := range oci.FreeformTags {
		if key != "" && value != "" {
			tags[key] = value
		}
	}
	for key, value := range labelsForPlan(plan) {
		tags[key] = value
	}
	return tags
}

func labelsForPlan(plan provider.HostPlan) map[string]string {
	if len(plan.Labels) > 0 {
		return plan.Labels
	}
	return provider.ShipLabels(plan.Project, plan.Environment, plan.Pool)
}

func launchShapeConfig(oci config.OCIConfig) map[string]any {
	out := map[string]any{}
	if oci.ShapeConfig.OCPUs > 0 {
		out["ocpus"] = oci.ShapeConfig.OCPUs
	}
	if oci.ShapeConfig.MemoryGB > 0 {
		out["memoryInGBs"] = oci.ShapeConfig.MemoryGB
	}
	return out
}

func networkSecurityGroupRules(oci config.OCIConfig) []SecurityRule {
	var rules []SecurityRule
	rules = append(rules, SecurityRule{
		Direction:       "EGRESS",
		Protocol:        "all",
		Destination:     "0.0.0.0/0",
		DestinationType: "CIDR_BLOCK",
		Description:     "ship-egress",
	})
	if oci.EffectiveSSHFirewall() == config.SSHFirewallManaged {
		for _, cidr := range oci.SSHAllowedCIDRs {
			rules = append(rules, ingressRule("ship-ssh", cidr, 22))
		}
	}
	for _, port := range []int{80, 443} {
		rules = append(rules, ingressRule("ship-http", "0.0.0.0/0", port))
	}
	rules = append(rules, udpIngressRule("ship-http3", "0.0.0.0/0", 443))
	return rules
}

func ingressRule(description, cidr string, port int) SecurityRule {
	return SecurityRule{
		Direction:   "INGRESS",
		Protocol:    "6",
		Source:      cidr,
		SourceType:  "CIDR_BLOCK",
		Description: description,
		TCPOptions:  &TCPOptions{DestinationPortRange: PortRange{Min: port, Max: port}},
	}
}

func udpIngressRule(description, cidr string, port int) SecurityRule {
	return SecurityRule{
		Direction:   "INGRESS",
		Protocol:    "17",
		Source:      cidr,
		SourceType:  "CIDR_BLOCK",
		Description: description,
		UDPOptions:  &UDPOptions{DestinationPortRange: PortRange{Min: port, Max: port}},
	}
}

func containsSecurityRule(existing []SecurityRule, want SecurityRule) bool {
	for _, got := range existing {
		if got.Direction != want.Direction || got.Protocol != want.Protocol ||
			got.Source != want.Source || got.Destination != want.Destination {
			continue
		}
		tcpMatches := got.TCPOptions == nil && want.TCPOptions == nil ||
			got.TCPOptions != nil && want.TCPOptions != nil &&
				got.TCPOptions.DestinationPortRange == want.TCPOptions.DestinationPortRange
		udpMatches := got.UDPOptions == nil && want.UDPOptions == nil ||
			got.UDPOptions != nil && want.UDPOptions != nil &&
				got.UDPOptions.DestinationPortRange == want.UDPOptions.DestinationPortRange
		if tcpMatches && udpMatches {
			return true
		}
	}
	return false
}

func hostFromInstance(instance Instance) provider.Host {
	return provider.Host{
		ID:            instance.ID,
		Name:          instance.DisplayName,
		Pool:          instance.FreeformTags[LabelPool],
		PublicAddress: instance.PublicIP,
		Labels:        instance.FreeformTags,
	}
}

func instanceMatches(instance Instance, project, environment string) bool {
	return instance.FreeformTags[LabelManagedBy] == "ship" &&
		instance.FreeformTags[LabelProject] == project &&
		instance.FreeformTags[LabelEnvironment] == environment
}

func withPlanDefaults(plan provider.HostPlan, oci config.OCIConfig) config.OCIConfig {
	if plan.Location != "" {
		oci.AvailabilityDomain = plan.Location
	}
	if plan.Size != "" {
		oci.Shape = plan.Size
	}
	if plan.Image != "" {
		oci.ImageID = plan.Image
	}
	if plan.UserData != "" {
		oci.UserData = plan.UserData
	}
	return oci
}

func networkSecurityGroupName(project, environment string, oci config.OCIConfig) string {
	if oci.NetworkSecurityGroup.Name != "" {
		return oci.NetworkSecurityGroup.Name
	}
	return "ship-" + safeName(project) + "-" + safeName(environment)
}

func endpointForRegion(region string) string {
	return "https://iaas." + strings.TrimSpace(region) + ".oraclecloud.com"
}

func uniqueStrings(values []string) []string {
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
	sort.Strings(out)
	return out
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
