package aws

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
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
	apiVersion = "2016-11-15"
	service    = "ec2"

	LabelManagedBy   = provider.LabelManagedBy
	LabelProject     = provider.LabelProject
	LabelEnvironment = provider.LabelEnvironment
	LabelPool        = provider.LabelPool
)

type Client struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Region          string
	DryRun          bool
	HTTP            *http.Client
	BaseURL         string
	Now             func() time.Time
}

type InstancePlan = provider.HostPlan

type Instance struct {
	ID        string
	Name      string
	PublicIP  string
	Tags      map[string]string
	StateName string
}

type SecurityGroup struct {
	ID   string
	Name string
}

type ReconcileResult = provider.ReconcileResult

func NewFromEnv(dryRun bool, region ...string) Client {
	configuredRegion := os.Getenv("AWS_REGION")
	if configuredRegion == "" {
		configuredRegion = os.Getenv("AWS_DEFAULT_REGION")
	}
	if len(region) > 0 && strings.TrimSpace(region[0]) != "" {
		configuredRegion = strings.TrimSpace(region[0])
	}
	return Client{
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
		Region:          configuredRegion,
		DryRun:          dryRun,
		HTTP:            http.DefaultClient,
	}
}

func (c Client) Name() string {
	return config.ProviderAWS
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
	aws := env.Provider.AWS
	if aws == nil {
		return nil
	}
	return provider.HostPlans(project, environment, env, provider.HostPlanOptions{
		Location: aws.Region,
		Size:     aws.InstanceType,
		Image:    aws.AMI,
		UserData: aws.UserData,
	})
}

func (c Client) PlanHosts(project, environment string, env config.Environment) ([]provider.HostPlan, error) {
	if env.Provider.AWS == nil {
		return nil, fmt.Errorf("environment %q must define provider.aws", environment)
	}
	return DesiredInstancesFor(project, environment, env), nil
}

func (c Client) Reconcile(ctx context.Context, project, environment string, env config.Environment) (provider.ReconcileResult, error) {
	if env.Provider.AWS == nil {
		return provider.ReconcileResult{}, fmt.Errorf("environment %q must define provider.aws", environment)
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

// reconcileBackendFor resolves the AWS region and security group and builds the
// reconcile backend shared by Reconcile and CreateHost so both create instances
// identically.
func (c Client) reconcileBackendFor(ctx context.Context, project, environment string, env config.Environment) (reconcileBackend, error) {
	awsConfig := *env.Provider.AWS
	c.Region = awsConfig.Region
	securityGroupID := strings.TrimSpace(awsConfig.SecurityGroup.ID)
	if awsConfig.SecurityGroup.ManagedValue(true) {
		securityGroup, err := c.EnsureSecurityGroup(ctx, project, environment, awsConfig)
		if err != nil {
			return reconcileBackend{}, err
		}
		securityGroupID = securityGroup.ID
	}
	return reconcileBackend{
		client:          c,
		aws:             awsConfig,
		securityGroupID: securityGroupID,
	}, nil
}

// CreateHost provisions a single instance using the backend Reconcile would
// build, so `ship migrate` can add a replacement alongside the existing one.
func (c Client) CreateHost(ctx context.Context, project, environment string, env config.Environment, plan provider.HostPlan) (provider.Host, error) {
	if env.Provider.AWS == nil {
		return provider.Host{}, fmt.Errorf("environment %q must define provider.aws", environment)
	}
	backend, err := c.reconcileBackendFor(ctx, project, environment, env)
	if err != nil {
		return provider.Host{}, err
	}
	return backend.Create(ctx, plan)
}

var _ provider.HostCreator = Client{}

type reconcileBackend struct {
	client          Client
	aws             config.AWSConfig
	securityGroupID string
}

func (b reconcileBackend) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	return b.client.List(ctx, project, environment)
}

func (b reconcileBackend) Create(ctx context.Context, plan provider.HostPlan) (provider.Host, error) {
	instance, err := b.client.RunInstance(ctx, plan, b.aws, b.securityGroupID)
	if err != nil {
		return provider.Host{}, err
	}
	return hostFromInstance(instance), nil
}

func (c Client) List(ctx context.Context, project, environment string) ([]provider.Host, error) {
	instances, err := c.DescribeInstances(ctx, project, environment)
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
	return c.TerminateInstance(ctx, host.ID)
}

func (c Client) DescribeInstances(ctx context.Context, project, environment string) ([]Instance, error) {
	values := actionValues("DescribeInstances")
	addFilter(values, 1, "tag:"+LabelManagedBy, "ship")
	addFilter(values, 2, "tag:"+LabelProject, project)
	addFilter(values, 3, "tag:"+LabelEnvironment, environment)
	addFilter(values, 4, "instance-state-name", "pending", "running", "stopping", "stopped")
	var instances []Instance
	for {
		var out describeInstancesResponse
		if err := c.do(ctx, "", values, &out); err != nil {
			return nil, err
		}
		for _, reservation := range out.Reservations.Items {
			for _, item := range reservation.Instances.Items {
				tags := tagsFromItems(item.Tags.Items)
				if tags[LabelManagedBy] != "ship" || tags[LabelProject] != project || tags[LabelEnvironment] != environment {
					continue
				}
				instances = append(instances, Instance{
					ID:        item.InstanceID,
					Name:      tags["Name"],
					PublicIP:  item.IPAddress,
					Tags:      tags,
					StateName: item.State.Name,
				})
			}
		}
		if strings.TrimSpace(out.NextToken) == "" {
			break
		}
		values.Set("NextToken", out.NextToken)
	}
	sort.SliceStable(instances, func(i, j int) bool {
		if instances[i].Tags[LabelPool] != instances[j].Tags[LabelPool] {
			return instances[i].Tags[LabelPool] < instances[j].Tags[LabelPool]
		}
		return instances[i].Name < instances[j].Name
	})
	return instances, nil
}

func (c Client) RunInstance(ctx context.Context, plan provider.HostPlan, awsConfig config.AWSConfig, securityGroupID string) (Instance, error) {
	values := actionValues("RunInstances")
	values.Set("ImageId", plan.Image)
	values.Set("InstanceType", plan.Size)
	values.Set("MinCount", "1")
	values.Set("MaxCount", "1")
	if awsConfig.KeyName != "" {
		values.Set("KeyName", awsConfig.KeyName)
	}
	if awsConfig.IAMInstanceProfile != "" {
		values.Set("IamInstanceProfile.Name", awsConfig.IAMInstanceProfile)
	}
	if awsConfig.Monitoring != nil {
		values.Set("Monitoring.Enabled", strconv.FormatBool(*awsConfig.Monitoring))
	}
	if plan.UserData != "" {
		values.Set("UserData", base64.StdEncoding.EncodeToString([]byte(plan.UserData)))
	}
	if awsConfig.AssociatePublicIPAddress != nil {
		values.Set("NetworkInterface.1.DeviceIndex", "0")
		values.Set("NetworkInterface.1.SubnetId", awsConfig.SubnetID)
		values.Set("NetworkInterface.1.AssociatePublicIpAddress", strconv.FormatBool(*awsConfig.AssociatePublicIPAddress))
		if securityGroupID != "" {
			values.Set("NetworkInterface.1.SecurityGroupId.1", securityGroupID)
		}
	} else {
		values.Set("SubnetId", awsConfig.SubnetID)
		if securityGroupID != "" {
			values.Set("SecurityGroupId.1", securityGroupID)
		}
	}
	addRootVolume(values, awsConfig.RootVolume)
	addTagSpecification(values, 1, "instance", tagsForPlan(plan))
	addTagSpecification(values, 2, "volume", tagsForPlan(plan))
	if c.DryRun {
		return Instance{Name: plan.Name, Tags: tagsForPlan(plan)}, nil
	}
	var out runInstancesResponse
	if err := c.do(ctx, awsConfig.Region, values, &out); err != nil {
		return Instance{}, err
	}
	if len(out.Instances.Items) == 0 {
		return Instance{}, fmt.Errorf("run instances returned no instances")
	}
	item := out.Instances.Items[0]
	tags := tagsFromItems(item.Tags.Items)
	return Instance{
		ID:        item.InstanceID,
		Name:      tags["Name"],
		PublicIP:  item.IPAddress,
		Tags:      tags,
		StateName: item.State.Name,
	}, nil
}

func addRootVolume(values url.Values, root config.AWSRootVolumeConfig) {
	if root.SizeGB == 0 && root.Type == "" {
		return
	}
	deviceName := strings.TrimSpace(root.DeviceName)
	if deviceName == "" {
		deviceName = "/dev/xvda"
	}
	values.Set("BlockDeviceMapping.1.DeviceName", deviceName)
	if root.SizeGB > 0 {
		values.Set("BlockDeviceMapping.1.Ebs.VolumeSize", strconv.Itoa(root.SizeGB))
	}
	if root.Type != "" {
		values.Set("BlockDeviceMapping.1.Ebs.VolumeType", root.Type)
	}
}

func (c Client) TerminateInstance(ctx context.Context, id string) error {
	values := actionValues("TerminateInstances")
	values.Set("InstanceId.1", id)
	if c.DryRun {
		return nil
	}
	return c.do(ctx, "", values, nil)
}

func (c Client) EnsureSecurityGroup(ctx context.Context, project, environment string, cfg config.AWSConfig) (SecurityGroup, error) {
	name := strings.TrimSpace(cfg.SecurityGroup.Name)
	if name == "" {
		name = resourceName(project, environment, "sg")
	}
	existing, err := c.DescribeSecurityGroups(ctx, cfg.Region, cfg.VPCID, name)
	if err != nil {
		return SecurityGroup{}, err
	}
	if len(existing) > 0 {
		if err := c.AuthorizeSecurityGroupIngress(ctx, cfg.Region, existing[0].ID, cfg); err != nil {
			return SecurityGroup{}, err
		}
		return existing[0], nil
	}
	group, err := c.CreateSecurityGroup(ctx, project, environment, cfg, name)
	if err != nil {
		return SecurityGroup{}, err
	}
	if err := c.AuthorizeSecurityGroupIngress(ctx, cfg.Region, group.ID, cfg); err != nil {
		return SecurityGroup{}, err
	}
	return group, nil
}

func (c Client) DescribeSecurityGroups(ctx context.Context, region, vpcID, name string) ([]SecurityGroup, error) {
	values := actionValues("DescribeSecurityGroups")
	addFilter(values, 1, "group-name", name)
	if vpcID != "" {
		addFilter(values, 2, "vpc-id", vpcID)
	}
	var groups []SecurityGroup
	for {
		var out describeSecurityGroupsResponse
		if err := c.do(ctx, region, values, &out); err != nil {
			return nil, err
		}
		for _, item := range out.SecurityGroups.Items {
			groups = append(groups, SecurityGroup{ID: item.GroupID, Name: item.GroupName})
		}
		if strings.TrimSpace(out.NextToken) == "" {
			break
		}
		values.Set("NextToken", out.NextToken)
	}
	return groups, nil
}

func (c Client) CreateSecurityGroup(ctx context.Context, project, environment string, cfg config.AWSConfig, name string) (SecurityGroup, error) {
	values := actionValues("CreateSecurityGroup")
	values.Set("GroupName", name)
	values.Set("GroupDescription", "Ship managed security group for "+project+"/"+environment)
	if cfg.VPCID != "" {
		values.Set("VpcId", cfg.VPCID)
	}
	addTagSpecification(values, 1, "security-group", provider.ShipLabels(project, environment, "security-group"))
	var out createSecurityGroupResponse
	if err := c.do(ctx, cfg.Region, values, &out); err != nil {
		return SecurityGroup{}, err
	}
	return SecurityGroup{ID: out.GroupID, Name: name}, nil
}

func (c Client) AuthorizeSecurityGroupIngress(ctx context.Context, region, groupID string, cfg config.AWSConfig) error {
	values := actionValues("AuthorizeSecurityGroupIngress")
	values.Set("GroupId", groupID)
	rule := 1
	if cfg.EffectiveSSHFirewall() == config.SSHFirewallManaged {
		addIPPermission(values, rule, "tcp", 22, 22, cfg.SSHAllowedCIDRs)
		rule++
	}
	addIPPermission(values, rule, "tcp", 80, 80, []string{"0.0.0.0/0", "::/0"})
	rule++
	addIPPermission(values, rule, "tcp", 443, 443, []string{"0.0.0.0/0", "::/0"})
	rule++
	addIPPermission(values, rule, "udp", 443, 443, []string{"0.0.0.0/0", "::/0"})
	if err := c.do(ctx, region, values, nil); err != nil {
		if strings.Contains(err.Error(), "InvalidPermission.Duplicate") {
			return nil
		}
		return err
	}
	return nil
}

func addIPPermission(values url.Values, index int, protocol string, fromPort, toPort int, cidrs []string) {
	prefix := fmt.Sprintf("IpPermissions.%d.", index)
	values.Set(prefix+"IpProtocol", protocol)
	values.Set(prefix+"FromPort", strconv.Itoa(fromPort))
	values.Set(prefix+"ToPort", strconv.Itoa(toPort))
	ipv4 := 1
	ipv6 := 1
	for _, cidr := range cidrs {
		if strings.Contains(cidr, ":") {
			values.Set(prefix+fmt.Sprintf("Ipv6Ranges.%d.CidrIpv6", ipv6), cidr)
			ipv6++
			continue
		}
		values.Set(prefix+fmt.Sprintf("IpRanges.%d.CidrIp", ipv4), cidr)
		ipv4++
	}
}

func actionValues(action string) url.Values {
	values := url.Values{}
	values.Set("Action", action)
	values.Set("Version", apiVersion)
	return values
}

func addFilter(values url.Values, index int, name string, filterValues ...string) {
	values.Set(fmt.Sprintf("Filter.%d.Name", index), name)
	for i, value := range filterValues {
		values.Set(fmt.Sprintf("Filter.%d.Value.%d", index, i+1), value)
	}
}

func addTagSpecification(values url.Values, index int, resourceType string, tags map[string]string) {
	values.Set(fmt.Sprintf("TagSpecification.%d.ResourceType", index), resourceType)
	keys := make([]string, 0, len(tags))
	for key := range tags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for i, key := range keys {
		values.Set(fmt.Sprintf("TagSpecification.%d.Tag.%d.Key", index, i+1), key)
		values.Set(fmt.Sprintf("TagSpecification.%d.Tag.%d.Value", index, i+1), tags[key])
	}
}

func tagsForPlan(plan provider.HostPlan) map[string]string {
	labels := plan.Labels
	if len(labels) == 0 {
		labels = provider.ShipLabels(plan.Project, plan.Environment, plan.Pool)
	}
	tags := make(map[string]string, len(labels)+1)
	for key, value := range labels {
		tags[key] = value
	}
	tags["Name"] = plan.Name
	return tags
}

func tagsFromItems(items []tagItem) map[string]string {
	tags := map[string]string{}
	for _, item := range items {
		tags[item.Key] = item.Value
	}
	return tags
}

func hostFromInstance(instance Instance) provider.Host {
	return provider.Host{
		ID:            instance.ID,
		Name:          instance.Name,
		Pool:          instance.Tags[LabelPool],
		PublicAddress: instance.PublicIP,
		Labels:        instance.Tags,
	}
}

func (c Client) do(ctx context.Context, region string, values url.Values, out any) error {
	if !c.credentialsPresent() {
		return fmt.Errorf("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are required")
	}
	if region == "" {
		region = c.Region
	}
	if region == "" {
		return fmt.Errorf("aws region is required")
	}
	endpoint := c.endpoint(region)
	body := values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	c.sign(req, region, []byte(body))
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
		return fmt.Errorf("aws ec2 %s failed: %s", values.Get("Action"), strings.TrimSpace(string(data)))
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return xml.Unmarshal(data, out)
}

func (c Client) endpoint(region string) string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return "https://ec2." + region + ".amazonaws.com"
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
	return out
}

type describeInstancesResponse struct {
	Reservations reservationSet `xml:"reservationSet"`
	NextToken    string         `xml:"nextToken"`
}

type runInstancesResponse struct {
	Instances instanceSet `xml:"instancesSet"`
}

type reservationSet struct {
	Items []reservationItem `xml:"item"`
}

type reservationItem struct {
	Instances instanceSet `xml:"instancesSet"`
}

type instanceSet struct {
	Items []instanceItem `xml:"item"`
}

type instanceItem struct {
	InstanceID string `xml:"instanceId"`
	IPAddress  string `xml:"ipAddress"`
	State      struct {
		Name string `xml:"name"`
	} `xml:"instanceState"`
	Tags tagSet `xml:"tagSet"`
}

type tagSet struct {
	Items []tagItem `xml:"item"`
}

type tagItem struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

type describeSecurityGroupsResponse struct {
	SecurityGroups securityGroupSet `xml:"securityGroupInfo"`
	NextToken      string           `xml:"nextToken"`
}

type securityGroupSet struct {
	Items []securityGroupItem `xml:"item"`
}

type securityGroupItem struct {
	GroupID   string `xml:"groupId"`
	GroupName string `xml:"groupName"`
}

type createSecurityGroupResponse struct {
	GroupID string `xml:"groupId"`
}
