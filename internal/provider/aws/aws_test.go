package aws

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

var _ provider.Provider = Client{}

func TestDesiredInstancesUsesPoolsAndProviderOptions(t *testing.T) {
	instances := DesiredInstances(testEnvironment(2))
	if len(instances) != 2 {
		t.Fatalf("len = %d", len(instances))
	}
	if instances[0].Name != "web-1" || instances[0].Pool != "web" {
		t.Fatalf("unexpected instance: %+v", instances[0])
	}
	if instances[0].Location != "us-east-1" || instances[0].Size != "t3.medium" || instances[0].Image != "ami-0123456789abcdef0" {
		t.Fatalf("provider options missing from plan: %+v", instances[0])
	}
}

func TestTagsForPlanIncludeShipLabelsAndName(t *testing.T) {
	instances := DesiredInstancesFor("demo", "production", testEnvironment(1))
	if len(instances) != 1 {
		t.Fatalf("len = %d", len(instances))
	}
	tags := tagsForPlan(instances[0])
	want := map[string]string{
		"Name":           "web-1",
		LabelManagedBy:   "ship",
		LabelProject:     "demo",
		LabelEnvironment: "production",
		LabelPool:        "web",
	}
	for key, value := range want {
		if tags[key] != value {
			t.Fatalf("tag %s = %q, want %q in %+v", key, tags[key], value, tags)
		}
	}
}

func TestReconcileCreatesMissingInstances(t *testing.T) {
	api := newFakeEC2API(t, nil)
	result, err := api.client().Reconcile(context.Background(), "demo", "production", testEnvironment(1))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("created = %+v", result.Created)
	}
	if len(result.Existing) != 0 || len(result.Extra) != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	createSG := api.firstAction("CreateSecurityGroup")
	if createSG.Get("GroupName") != "ship-demo-production-sg" || createSG.Get("VpcId") != "vpc-0123456789abcdef0" {
		t.Fatalf("create sg = %s", createSG.Encode())
	}
	ingress := api.firstAction("AuthorizeSecurityGroupIngress")
	if ingress.Get("GroupId") != "sg-100" ||
		ingress.Get("IpPermissions.1.FromPort") != "22" ||
		ingress.Get("IpPermissions.2.FromPort") != "80" ||
		ingress.Get("IpPermissions.3.FromPort") != "443" {
		t.Fatalf("ingress = %s", ingress.Encode())
	}
	run := api.firstAction("RunInstances")
	if run.Get("ImageId") != "ami-0123456789abcdef0" ||
		run.Get("InstanceType") != "t3.medium" ||
		run.Get("KeyName") != "ship-key" ||
		run.Get("IamInstanceProfile.Name") != "ship-profile" ||
		run.Get("Monitoring.Enabled") != "true" {
		t.Fatalf("run = %s", run.Encode())
	}
	if got, want := run.Get("UserData"), base64.StdEncoding.EncodeToString([]byte("#cloud-config\npackages: [htop]\n")); got != want {
		t.Fatalf("UserData = %q, want %q", got, want)
	}
	if run.Get("NetworkInterface.1.SubnetId") != "subnet-0123456789abcdef0" ||
		run.Get("NetworkInterface.1.SecurityGroupId.1") != "sg-100" ||
		run.Get("NetworkInterface.1.AssociatePublicIpAddress") != "true" {
		t.Fatalf("network interface params = %s", run.Encode())
	}
	if run.Get("BlockDeviceMapping.1.Ebs.VolumeSize") != "40" || run.Get("BlockDeviceMapping.1.Ebs.VolumeType") != "gp3" {
		t.Fatalf("root volume params = %s", run.Encode())
	}
	assertTagSpec(t, run, "instance", map[string]string{
		"Name":           "web-1",
		"cost-center":    "platform",
		LabelManagedBy:   "ship",
		LabelProject:     "demo",
		LabelEnvironment: "production",
		LabelPool:        "web",
	})
	if result.Created[0].ID == "" || result.Created[0].PublicAddress == "" || result.Created[0].Pool != "web" {
		t.Fatalf("created host missing facts: %+v", result.Created[0])
	}
	if api.authHeaders[0] == "" || !strings.HasPrefix(api.authHeaders[0], "AWS4-HMAC-SHA256 ") {
		t.Fatalf("authorization header = %q", api.authHeaders[0])
	}
}

func TestReconcileLeavesMatchingInstancesAndReportsExtra(t *testing.T) {
	existing := []fakeInstance{
		{ID: "i-1", Name: "web-1", Pool: "web", IP: "192.0.2.10"},
		{ID: "i-old", Name: "web-old", Pool: "web", IP: "192.0.2.11"},
	}
	api := newFakeEC2API(t, existing)
	result, err := api.client().Reconcile(context.Background(), "demo", "production", testEnvironment(2))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Existing) != 1 || result.Existing[0].ID != "i-1" {
		t.Fatalf("existing = %+v", result.Existing)
	}
	if len(result.Created) != 1 || result.Created[0].Name != "web-2" {
		t.Fatalf("created = %+v", result.Created)
	}
	if len(result.Extra) != 1 || result.Extra[0].Name != "web-old" {
		t.Fatalf("extra = %+v", result.Extra)
	}
}

func TestDescribeInstancesPaginatesAndFiltersTags(t *testing.T) {
	api := newFakeEC2API(t, nil)
	api.pages = map[string]describePage{
		"": {
			instances: []fakeInstance{
				{ID: "i-other", Name: "other", Pool: "", Project: "other", Environment: "production"},
				{ID: "i-web", Name: "web-1", Pool: "web", IP: "192.0.2.10"},
			},
			nextToken: "next-page",
		},
		"next-page": {
			instances: []fakeInstance{
				{ID: "i-staging", Name: "staging", Pool: "web", Environment: "staging"},
				{ID: "i-worker", Name: "worker-1", Pool: "worker", IP: "192.0.2.11"},
			},
		},
	}
	instances, err := api.client().DescribeInstances(context.Background(), "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 2 {
		t.Fatalf("instances = %+v", instances)
	}
	if got, want := strings.Join(api.nextTokens, ","), ",next-page"; got != want {
		t.Fatalf("next tokens = %q, want %q", got, want)
	}
}

func TestDeleteTerminatesInstanceByID(t *testing.T) {
	api := newFakeEC2API(t, nil)
	if err := api.client().Delete(context.Background(), provider.Host{ID: "i-123"}); err != nil {
		t.Fatal(err)
	}
	terminate := api.firstAction("TerminateInstances")
	if terminate.Get("InstanceId.1") != "i-123" {
		t.Fatalf("terminate = %s", terminate.Encode())
	}
}

func TestReconcileUsesExplicitSecurityGroupID(t *testing.T) {
	env := testEnvironment(1)
	managed := false
	env.Provider.AWS.SecurityGroup = config.AWSSecurityGroupConfig{Managed: &managed, ID: "sg-existing"}
	api := newFakeEC2API(t, nil)
	if _, err := api.client().Reconcile(context.Background(), "demo", "production", env); err != nil {
		t.Fatal(err)
	}
	if api.hasAction("CreateSecurityGroup") {
		t.Fatal("did not expect managed security group creation")
	}
	run := api.firstAction("RunInstances")
	if run.Get("NetworkInterface.1.SecurityGroupId.1") != "sg-existing" {
		t.Fatalf("run = %s", run.Encode())
	}
}

func TestEnsureSecurityGroupRepairsExistingIngress(t *testing.T) {
	api := newFakeEC2API(t, nil)
	api.existingSecurityGroups = []SecurityGroup{{ID: "sg-existing", Name: "ship-demo-production-sg"}}
	group, err := api.client().EnsureSecurityGroup(context.Background(), "demo", "production", *testEnvironment(1).Provider.AWS)
	if err != nil {
		t.Fatal(err)
	}
	if group.ID != "sg-existing" {
		t.Fatalf("group = %+v", group)
	}
	if api.hasAction("CreateSecurityGroup") {
		t.Fatal("did not expect security group creation")
	}
	ingress := api.firstAction("AuthorizeSecurityGroupIngress")
	if ingress.Get("GroupId") != "sg-existing" {
		t.Fatalf("ingress = %s", ingress.Encode())
	}
}

func TestReconcileDryRunDoesNotCallAPI(t *testing.T) {
	client := Client{DryRun: true}
	result, err := client.Reconcile(context.Background(), "demo", "production", testEnvironment(1))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Desired) != 1 || len(result.Created) != 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestCredentialChecks(t *testing.T) {
	checks := Client{}.CredentialChecks(func(key string) (string, bool) {
		switch key {
		case "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY":
			return "set", true
		default:
			t.Fatalf("lookup key = %q", key)
			return "", false
		}
	})
	if len(checks) != 2 || !checks[0].Present || !checks[1].Present {
		t.Fatalf("checks = %+v", checks)
	}
}

func testEnvironment(count int) config.Environment {
	publicIP := true
	monitoring := true
	return config.Environment{
		Provider: config.ProviderConfig{AWS: &config.AWSConfig{
			Region:                   "us-east-1",
			InstanceType:             "t3.medium",
			AMI:                      "ami-0123456789abcdef0",
			UserData:                 "#cloud-config\npackages: [htop]\n",
			KeyName:                  "ship-key",
			SubnetID:                 "subnet-0123456789abcdef0",
			VPCID:                    "vpc-0123456789abcdef0",
			AssociatePublicIPAddress: &publicIP,
			IAMInstanceProfile:       "ship-profile",
			Monitoring:               &monitoring,
			RootVolume:               config.AWSRootVolumeConfig{SizeGB: 40, Type: "gp3"},
			SSHAllowedCIDRs:          []string{"203.0.113.0/24"},
		}},
		Hosts: config.HostsConfig{
			Labels: map[string]string{"cost-center": "platform"},
			Pools: map[string]config.Pool{
				"web": {Count: count},
			},
		},
	}
}

type fakeEC2API struct {
	server                 *httptest.Server
	existing               []fakeInstance
	pages                  map[string]describePage
	requests               []url.Values
	authHeaders            []string
	nextTokens             []string
	nextID                 int
	existingSecurityGroups []SecurityGroup
}

type fakeInstance struct {
	ID          string
	Name        string
	Pool        string
	IP          string
	Project     string
	Environment string
}

type describePage struct {
	instances []fakeInstance
	nextToken string
}

func newFakeEC2API(t *testing.T, existing []fakeInstance) *fakeEC2API {
	t.Helper()
	api := &fakeEC2API{existing: existing, nextID: 100}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		if r.Header.Get("Authorization") == "" || r.Header.Get("X-Amz-Date") == "" {
			t.Fatalf("missing aws signing headers: auth=%q date=%q", r.Header.Get("Authorization"), r.Header.Get("X-Amz-Date"))
		}
		api.authHeaders = append(api.authHeaders, r.Header.Get("Authorization"))
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		form := cloneValues(r.PostForm)
		api.requests = append(api.requests, form)
		switch form.Get("Action") {
		case "DescribeSecurityGroups":
			writeXML(w, securityGroupsXML(api.existingSecurityGroups, ""))
		case "CreateSecurityGroup":
			writeXML(w, `<CreateSecurityGroupResponse><groupId>sg-100</groupId></CreateSecurityGroupResponse>`)
		case "AuthorizeSecurityGroupIngress":
			writeXML(w, `<AuthorizeSecurityGroupIngressResponse><return>true</return></AuthorizeSecurityGroupIngressResponse>`)
		case "DescribeInstances":
			token := form.Get("NextToken")
			api.nextTokens = append(api.nextTokens, token)
			if api.pages != nil {
				page := api.pages[token]
				writeXML(w, describeInstancesXML(page.instances, page.nextToken))
				return
			}
			writeXML(w, describeInstancesXML(api.existing, ""))
		case "RunInstances":
			api.nextID++
			instance := fakeInstance{
				ID:          fmt.Sprintf("i-%d", api.nextID),
				Name:        tagValueFromRun(form, "Name"),
				Pool:        tagValueFromRun(form, LabelPool),
				IP:          fmt.Sprintf("192.0.2.%d", api.nextID-99),
				Project:     tagValueFromRun(form, LabelProject),
				Environment: tagValueFromRun(form, LabelEnvironment),
			}
			writeXML(w, runInstancesXML(instance))
		case "TerminateInstances":
			writeXML(w, `<TerminateInstancesResponse><return>true</return></TerminateInstancesResponse>`)
		default:
			t.Fatalf("unexpected action %q form=%s", form.Get("Action"), form.Encode())
		}
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (api *fakeEC2API) client() Client {
	return Client{
		AccessKeyID:     "AKIATEST",
		SecretAccessKey: "secret",
		Region:          "us-east-1",
		HTTP:            api.server.Client(),
		BaseURL:         api.server.URL,
		Now:             func() time.Time { return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC) },
	}
}

func (api *fakeEC2API) firstAction(action string) url.Values {
	for _, req := range api.requests {
		if req.Get("Action") == action {
			return req
		}
	}
	return nil
}

func (api *fakeEC2API) hasAction(action string) bool {
	return api.firstAction(action) != nil
}

func cloneValues(in url.Values) url.Values {
	out := url.Values{}
	for key, values := range in {
		out[key] = append([]string(nil), values...)
	}
	return out
}

func assertTagSpec(t *testing.T, values url.Values, resourceType string, want map[string]string) {
	t.Helper()
	found := 0
	for spec := 1; spec <= 4; spec++ {
		if values.Get(fmt.Sprintf("TagSpecification.%d.ResourceType", spec)) != resourceType {
			continue
		}
		found = spec
		break
	}
	if found == 0 {
		t.Fatalf("missing tag spec for %s in %s", resourceType, values.Encode())
	}
	got := map[string]string{}
	for i := 1; ; i++ {
		key := values.Get(fmt.Sprintf("TagSpecification.%d.Tag.%d.Key", found, i))
		if key == "" {
			break
		}
		got[key] = values.Get(fmt.Sprintf("TagSpecification.%d.Tag.%d.Value", found, i))
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("tag %s = %q, want %q in %+v", key, got[key], value, got)
		}
	}
}

func tagValueFromRun(values url.Values, key string) string {
	for i := 1; ; i++ {
		gotKey := values.Get(fmt.Sprintf("TagSpecification.1.Tag.%d.Key", i))
		if gotKey == "" {
			return ""
		}
		if gotKey == key {
			return values.Get(fmt.Sprintf("TagSpecification.1.Tag.%d.Value", i))
		}
	}
}

func describeInstancesXML(instances []fakeInstance, nextToken string) string {
	var b strings.Builder
	b.WriteString(`<DescribeInstancesResponse><reservationSet><item><instancesSet>`)
	for _, instance := range instances {
		if instance.ID == "" {
			instance.ID = "i-empty"
		}
		if instance.Project == "" {
			instance.Project = "demo"
		}
		if instance.Environment == "" {
			instance.Environment = "production"
		}
		b.WriteString(instanceXML(instance))
	}
	b.WriteString(`</instancesSet></item></reservationSet>`)
	if nextToken != "" {
		b.WriteString(`<nextToken>` + xmlEscape(nextToken) + `</nextToken>`)
	}
	b.WriteString(`</DescribeInstancesResponse>`)
	return b.String()
}

func runInstancesXML(instance fakeInstance) string {
	return `<RunInstancesResponse><instancesSet>` + instanceXML(instance) + `</instancesSet></RunInstancesResponse>`
}

func instanceXML(instance fakeInstance) string {
	tags := map[string]string{
		"Name":           instance.Name,
		LabelManagedBy:   "ship",
		LabelProject:     instance.Project,
		LabelEnvironment: instance.Environment,
		LabelPool:        instance.Pool,
	}
	keys := make([]string, 0, len(tags))
	for key := range tags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(`<item><instanceId>` + xmlEscape(instance.ID) + `</instanceId><instanceState><name>running</name></instanceState>`)
	if instance.IP != "" {
		b.WriteString(`<ipAddress>` + xmlEscape(instance.IP) + `</ipAddress>`)
	}
	b.WriteString(`<tagSet>`)
	for _, key := range keys {
		if tags[key] == "" {
			continue
		}
		b.WriteString(`<item><key>` + xmlEscape(key) + `</key><value>` + xmlEscape(tags[key]) + `</value></item>`)
	}
	b.WriteString(`</tagSet></item>`)
	return b.String()
}

func securityGroupsXML(groups []SecurityGroup, nextToken string) string {
	var b strings.Builder
	b.WriteString(`<DescribeSecurityGroupsResponse><securityGroupInfo>`)
	for _, group := range groups {
		b.WriteString(`<item><groupId>` + xmlEscape(group.ID) + `</groupId><groupName>` + xmlEscape(group.Name) + `</groupName></item>`)
	}
	b.WriteString(`</securityGroupInfo>`)
	if nextToken != "" {
		b.WriteString(`<nextToken>` + xmlEscape(nextToken) + `</nextToken>`)
	}
	b.WriteString(`</DescribeSecurityGroupsResponse>`)
	return b.String()
}

func writeXML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/xml")
	_, _ = w.Write([]byte(body))
}

func xmlEscape(value string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(value))
	return b.String()
}
