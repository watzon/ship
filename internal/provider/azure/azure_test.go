package azure

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

var _ provider.Provider = Client{}

func TestDesiredVirtualMachinesUsesPoolsAndProviderShape(t *testing.T) {
	vms := DesiredVirtualMachines(testEnvironment(2))
	if len(vms) != 2 {
		t.Fatalf("len = %d", len(vms))
	}
	if vms[0].Name != "web-1" || vms[0].Pool != "web" {
		t.Fatalf("vm = %+v", vms[0])
	}
	if vms[0].Location != "eastus" || vms[0].Size != "Standard_B2s" || vms[0].Image != "Canonical:ubuntu-24_04-lts:server:latest" {
		t.Fatalf("provider shape missing: %+v", vms[0])
	}
}

func TestCreateHostProvisionsReplacement(t *testing.T) {
	api := newFakeAzureAPI(t, nil)
	env := testEnvironment(1)
	plans := DesiredVirtualMachinesFor("demo", "production", env)
	if len(plans) == 0 {
		t.Fatal("no desired plans")
	}
	host, err := api.client().CreateHost(context.Background(), "demo", "production", env, plans[0])
	if err != nil {
		t.Fatal(err)
	}
	if host.PublicAddress != "203.0.113.20" {
		t.Fatalf("created host = %+v", host)
	}
	if len(api.securityGroups) != 1 {
		t.Fatalf("security group not ensured through the shared backend: %+v", api.securityGroups)
	}
	if len(api.virtualMachines) != 1 {
		t.Fatalf("virtual machine not created through the shared backend: %+v", api.virtualMachines)
	}
}

func TestReconcileCreatesSecurityGroupNetworkAndVirtualMachine(t *testing.T) {
	api := newFakeAzureAPI(t, nil)
	result, err := api.client().Reconcile(context.Background(), "demo", "production", testEnvironment(1))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("created = %+v", result.Created)
	}
	if result.Created[0].PublicAddress != "203.0.113.20" {
		t.Fatalf("created host = %+v", result.Created[0])
	}
	if len(api.securityGroups) != 1 {
		t.Fatalf("security groups = %+v", api.securityGroups)
	}
	sg := api.securityGroups[0]
	if sg.Location != "eastus" || len(sg.Properties.SecurityRules) != 4 {
		t.Fatalf("security group = %+v", sg)
	}
	sshRule := sg.Properties.SecurityRules[0]
	if sshRule.Name != "ship-ssh" || sshRule.Properties.DestinationPortRange != "22" ||
		sshRule.Properties.SourceAddressPrefix != "203.0.113.0/24" {
		t.Fatalf("ssh rule = %+v", sshRule)
	}
	if len(api.publicIPs) != 1 || api.publicIPs[0].Properties.PublicIPAllocationMethod != "Static" {
		t.Fatalf("public ips = %+v", api.publicIPs)
	}
	if len(api.networkInterfaces) != 1 {
		t.Fatalf("nics = %+v", api.networkInterfaces)
	}
	nic := api.networkInterfaces[0]
	if nic.Properties.NetworkSecurityGroup.ID == "" || nic.Properties.IPConfigurations[0].Properties.PublicIPAddress.ID == "" {
		t.Fatalf("nic = %+v", nic)
	}
	if len(api.virtualMachines) != 1 {
		t.Fatalf("virtual machines = %+v", api.virtualMachines)
	}
	vm := api.virtualMachines[0]
	if vm.Location != "eastus" || vm.Properties.HardwareProfile.VMSize != "Standard_B2s" {
		t.Fatalf("vm = %+v", vm)
	}
	if vm.Tags["managed-by"] != "ship" || vm.Tags["project"] != "demo" || vm.Tags["environment"] != "production" || vm.Tags["pool"] != "web" {
		t.Fatalf("tags = %+v", vm.Tags)
	}
	image := vm.Properties.StorageProfile.ImageReference
	if image.Publisher != "Canonical" || image.Offer != "ubuntu-24_04-lts" || image.SKU != "server" || image.Version != "latest" {
		t.Fatalf("image = %+v", image)
	}
	if vm.Properties.OSProfile.CustomData != base64.StdEncoding.EncodeToString([]byte("#cloud-config\npackages: [htop]\n")) {
		t.Fatalf("custom data = %q", vm.Properties.OSProfile.CustomData)
	}
	if vm.Properties.OSProfile.LinuxConfiguration.SSH.PublicKeys[0].KeyData != "ssh-ed25519 AAAA..." {
		t.Fatalf("ssh keys = %+v", vm.Properties.OSProfile.LinuxConfiguration.SSH.PublicKeys)
	}
}

func TestListFiltersTagsAndDeleteRemovesCompanionResources(t *testing.T) {
	api := newFakeAzureAPI(t, []VirtualMachine{
		{Name: "web-1", Tags: provider.ShipLabels("demo", "production", "web")},
		{Name: "staging", Tags: provider.ShipLabels("demo", "staging", "web")},
		{Name: "other", Tags: map[string]string{"managed-by": "other"}},
	})
	hosts, err := api.client().List(context.Background(), "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].Name != "web-1" || hosts[0].PublicAddress != "203.0.113.20" {
		t.Fatalf("hosts = %+v", hosts)
	}
	if err := api.client().Delete(context.Background(), hosts[0]); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(api.deletes, ","), "vm:web-1,nic:web-1-nic,pip:web-1-pip"; got != want {
		t.Fatalf("deletes = %q, want %q", got, want)
	}
}

func TestDeleteWaitsForAzureOperations(t *testing.T) {
	api := newFakeAzureAPI(t, nil)
	api.operations = map[string][]fakeOperationResponse{
		"vm": {
			{Status: "Running", RetryAfter: "0"},
			{Status: "Succeeded"},
		},
		"nic": {
			{Status: "InProgress", RetryAfter: "0"},
			{Status: "Succeeded"},
		},
		"pip": {
			{Status: "Accepted", RetryAfter: "0"},
			{Status: "sUcCeEdEd"},
		},
	}
	api.operationHeaders = map[string]string{
		"vm":  "Azure-AsyncOperation",
		"nic": "Location",
		"pip": "Azure-AsyncOperation",
	}

	client := api.client()
	client.PollInterval = time.Hour
	err := client.Delete(context.Background(), provider.Host{ID: "web-1"})
	if err != nil {
		t.Fatal(err)
	}
	if api.prematureNIC {
		t.Fatal("network interface deletion began before the virtual machine operation succeeded")
	}
	if got, want := strings.Join(api.events, ","), "delete:vm,poll:vm:Running,poll:vm:Succeeded,delete:nic,poll:nic:InProgress,poll:nic:Succeeded,delete:pip,poll:pip:Accepted,poll:pip:sUcCeEdEd"; got != want {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

func TestDeleteVirtualMachineOperationFailure(t *testing.T) {
	tests := []struct {
		name    string
		status  string
		code    string
		message string
	}{
		{name: "failed", status: "Failed", code: "DeleteFailed", message: "the VM is locked"},
		{name: "canceled", status: "Canceled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAzureAPI(t, nil)
			api.operations = map[string][]fakeOperationResponse{
				"vm": {{Status: tt.status, Code: tt.code, Message: tt.message}},
			}
			err := api.client().DeleteVirtualMachine(context.Background(), "web-1")
			if err == nil {
				t.Fatal("expected operation failure")
			}
			for _, want := range []string{"virtual machine", "web-1", tt.status} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %q, want %q", err, want)
				}
			}
			if tt.code != "" && (!strings.Contains(err.Error(), tt.code) || !strings.Contains(err.Error(), tt.message)) {
				t.Fatalf("error = %q, want Azure error details", err)
			}
			if strings.Contains(err.Error(), "test-token") {
				t.Fatalf("error contains bearer token: %q", err)
			}
		})
	}
}

func TestDeleteVirtualMachineOperationStopsWithContext(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		api := newFakeAzureAPI(t, nil)
		api.operations = map[string][]fakeOperationResponse{
			"vm": {{Status: "Running"}},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		err := api.client().DeleteVirtualMachine(ctx, "web-1")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want deadline exceeded", err)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		api := newFakeAzureAPI(t, nil)
		api.operations = map[string][]fakeOperationResponse{
			"vm": {{Status: "Running"}},
		}
		polled := make(chan struct{})
		var once sync.Once
		api.operationPollHook = func(kind string) {
			if kind == "vm" {
				once.Do(func() { close(polled) })
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-polled
			cancel()
		}()
		err := api.client().DeleteVirtualMachine(ctx, "web-1")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context canceled", err)
		}
	})

	t.Run("configured operation timeout", func(t *testing.T) {
		api := newFakeAzureAPI(t, nil)
		api.operations = map[string][]fakeOperationResponse{
			"vm": {{Status: "Running"}},
		}
		client := api.client()
		client.OperationTimeout = 10 * time.Millisecond
		err := client.DeleteVirtualMachine(context.Background(), "web-1")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want configured operation timeout", err)
		}
	})
}

func TestDeleteVirtualMachineAcceptedWithoutOperationURL(t *testing.T) {
	api := newFakeAzureAPI(t, nil)
	api.operations = map[string][]fakeOperationResponse{
		"vm": {{Status: "Succeeded"}},
	}
	api.omitOperationURL = map[string]bool{"vm": true}
	err := api.client().DeleteVirtualMachine(context.Background(), "web-1")
	if err == nil || !strings.Contains(err.Error(), "202") || !strings.Contains(err.Error(), "operation URL") {
		t.Fatalf("error = %v, want missing operation URL", err)
	}
}

func TestDeleteReturnsCompanionCleanupFailures(t *testing.T) {
	tests := []struct {
		name          string
		failures      map[string]fakeDeleteFailure
		wantFragments []string
	}{
		{
			name:     "network interface failure still attempts public IP",
			failures: map[string]fakeDeleteFailure{"nic": {Status: http.StatusConflict, Body: `{"error":{"code":"NicBusy"}}`}},
			wantFragments: []string{
				"web-1-nic",
			},
		},
		{
			name:     "public IP failure",
			failures: map[string]fakeDeleteFailure{"pip": {Status: http.StatusConflict, Body: `{"error":{"code":"IPBusy"}}`}},
			wantFragments: []string{
				"web-1-pip",
			},
		},
		{
			name: "failures use stable cleanup order",
			failures: map[string]fakeDeleteFailure{
				"nic": {Status: http.StatusConflict, Body: `{"error":{"code":"NicBusy"}}`},
				"pip": {Status: http.StatusConflict, Body: `{"error":{"code":"IPBusy"}}`},
			},
			wantFragments: []string{"web-1-nic", "web-1-pip"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAzureAPI(t, nil)
			api.deleteFailures = tt.failures
			err := api.client().Delete(context.Background(), provider.Host{ID: "web-1"})
			if err == nil {
				t.Fatal("expected cleanup failure")
			}
			if got, want := strings.Join(api.deletes, ","), "vm:web-1,nic:web-1-nic,pip:web-1-pip"; got != want {
				t.Fatalf("deletes = %q, want %q", got, want)
			}
			previous := -1
			for _, want := range tt.wantFragments {
				index := strings.Index(err.Error(), want)
				if index < 0 {
					t.Fatalf("error = %q, want %q", err, want)
				}
				if index <= previous {
					t.Fatalf("error = %q, cleanup errors are out of order", err)
				}
				previous = index
			}
		})
	}
}

func TestDeleteAttemptsPublicIPAfterNetworkInterfaceOperationFailure(t *testing.T) {
	api := newFakeAzureAPI(t, nil)
	api.operations = map[string][]fakeOperationResponse{
		"nic": {{Status: "Failed", Code: "NicBusy", Message: "network interface is still attached"}},
	}
	err := api.client().Delete(context.Background(), provider.Host{ID: "web-1"})
	if err == nil || !strings.Contains(err.Error(), "web-1-nic") || !strings.Contains(err.Error(), "Failed") {
		t.Fatalf("error = %v, want network interface operation failure", err)
	}
	if got, want := strings.Join(api.deletes, ","), "vm:web-1,nic:web-1-nic,pip:web-1-pip"; got != want {
		t.Fatalf("deletes = %q, want %q", got, want)
	}
}

func TestDeleteTreatsMissingCompanionResourcesAsAbsent(t *testing.T) {
	api := newFakeAzureAPI(t, nil)
	api.deleteFailures = map[string]fakeDeleteFailure{
		"nic": {Status: http.StatusNotFound, Body: `{"error":{"code":"ResourceNotFound"}}`},
		"pip": {Status: http.StatusNotFound, Body: `{"error":{"code":"ResourceNotFound"}}`},
	}
	if err := api.client().Delete(context.Background(), provider.Host{ID: "web-1"}); err != nil {
		t.Fatal(err)
	}

	api.publicIPGetFailure = &fakeDeleteFailure{Status: http.StatusNotFound, Body: `{"error":{"code":"ResourceNotFound"}}`}
	if _, err := api.client().GetPublicIPAddress(context.Background(), "web-1-pip"); err == nil {
		t.Fatal("GET 404 must remain an error")
	}
}

func TestDeleteDryRunPerformsNoRequests(t *testing.T) {
	api := newFakeAzureAPI(t, nil)
	client := api.client()
	client.DryRun = true
	if err := client.Delete(context.Background(), provider.Host{ID: "web-1"}); err != nil {
		t.Fatal(err)
	}
	if len(api.events) != 0 || len(api.deletes) != 0 {
		t.Fatalf("dry-run requests = events %v, deletes %v", api.events, api.deletes)
	}
}

func TestBearerTokenUsesClientCredentials(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("client_id") != "client" || r.Form.Get("client_secret") != "secret" ||
			r.Form.Get("scope") != armScope || r.Form.Get("grant_type") != "client_credentials" {
			t.Fatalf("form = %+v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "arm-token"})
	}))
	defer tokenServer.Close()
	client := Client{
		TenantID:         "tenant",
		ClientID:         "client",
		ClientSecret:     "secret",
		HTTP:             tokenServer.Client(),
		TokenURLTemplate: tokenServer.URL + "/{tenant}/token",
	}
	token, err := client.bearerToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "arm-token" {
		t.Fatalf("token = %q", token)
	}
}

func TestCredentialChecks(t *testing.T) {
	checks := Client{}.CredentialChecks(func(key string) (string, bool) {
		return "", key == "AZURE_ACCESS_TOKEN"
	})
	if len(checks) != 1 || !checks[0].Present || checks[0].Name != "azure credentials" {
		t.Fatalf("checks = %+v", checks)
	}
}

func testEnvironment(count int) config.Environment {
	return config.Environment{
		Provider: config.ProviderConfig{Azure: &config.AzureConfig{
			SubscriptionID:  "sub-123",
			ResourceGroup:   "rg-ship",
			Location:        "eastus",
			VMSize:          "Standard_B2s",
			Image:           "Canonical:ubuntu-24_04-lts:server:latest",
			AdminUsername:   "deploy",
			SSHPublicKey:    "ssh-ed25519 AAAA...",
			UserData:        "#cloud-config\npackages: [htop]\n",
			VirtualNetwork:  "ship-vnet",
			Subnet:          "default",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
			OSDisk: config.AzureOSDiskConfig{
				SizeGB: 40,
				Type:   "Premium_LRS",
			},
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {Count: count},
		}},
	}
}

type fakeAzureAPI struct {
	server             *httptest.Server
	existing           []VirtualMachine
	securityGroups     []securityGroupRequest
	publicIPs          []publicIPRequest
	networkInterfaces  []networkInterfaceRequest
	virtualMachines    []virtualMachineRequest
	deletes            []string
	events             []string
	operations         map[string][]fakeOperationResponse
	operationPolls     map[string]int
	operationHeaders   map[string]string
	omitOperationURL   map[string]bool
	operationSucceeded map[string]bool
	operationPollHook  func(string)
	deleteFailures     map[string]fakeDeleteFailure
	publicIPGetFailure *fakeDeleteFailure
	prematureNIC       bool
}

type fakeOperationResponse struct {
	Status     string
	Code       string
	Message    string
	RetryAfter string
}

type fakeDeleteFailure struct {
	Status int
	Body   string
}

type securityGroupRequest struct {
	Location   string            `json:"location"`
	Tags       map[string]string `json:"tags"`
	Properties struct {
		SecurityRules []struct {
			Name       string `json:"name"`
			Properties struct {
				DestinationPortRange  string   `json:"destinationPortRange"`
				SourceAddressPrefix   string   `json:"sourceAddressPrefix"`
				SourceAddressPrefixes []string `json:"sourceAddressPrefixes"`
			} `json:"properties"`
		} `json:"securityRules"`
	} `json:"properties"`
}

type publicIPRequest struct {
	Location   string `json:"location"`
	Properties struct {
		PublicIPAllocationMethod string `json:"publicIPAllocationMethod"`
	} `json:"properties"`
}

type networkInterfaceRequest struct {
	Properties struct {
		NetworkSecurityGroup struct {
			ID string `json:"id"`
		} `json:"networkSecurityGroup"`
		IPConfigurations []struct {
			Properties struct {
				Subnet struct {
					ID string `json:"id"`
				} `json:"subnet"`
				PublicIPAddress struct {
					ID string `json:"id"`
				} `json:"publicIPAddress"`
			} `json:"properties"`
		} `json:"ipConfigurations"`
	} `json:"properties"`
}

type virtualMachineRequest struct {
	Location   string            `json:"location"`
	Tags       map[string]string `json:"tags"`
	Properties struct {
		HardwareProfile struct {
			VMSize string `json:"vmSize"`
		} `json:"hardwareProfile"`
		StorageProfile struct {
			ImageReference struct {
				Publisher string `json:"publisher"`
				Offer     string `json:"offer"`
				SKU       string `json:"sku"`
				Version   string `json:"version"`
			} `json:"imageReference"`
		} `json:"storageProfile"`
		OSProfile struct {
			CustomData         string `json:"customData"`
			LinuxConfiguration struct {
				SSH struct {
					PublicKeys []struct {
						KeyData string `json:"keyData"`
					} `json:"publicKeys"`
				} `json:"ssh"`
			} `json:"linuxConfiguration"`
		} `json:"osProfile"`
	} `json:"properties"`
}

func newFakeAzureAPI(t *testing.T, existing []VirtualMachine) *fakeAzureAPI {
	t.Helper()
	api := &fakeAzureAPI{
		existing:           existing,
		operationPolls:     make(map[string]int),
		operationSucceeded: make(map[string]bool),
	}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/operations/"):
			kind := pathBase(path)
			responses := api.operations[kind]
			if len(responses) == 0 {
				t.Fatalf("no operation responses for %q", kind)
			}
			poll := api.operationPolls[kind]
			response := responses[min(poll, len(responses)-1)]
			api.operationPolls[kind] = poll + 1
			api.events = append(api.events, "poll:"+kind+":"+response.Status)
			if strings.EqualFold(response.Status, "Succeeded") {
				api.operationSucceeded[kind] = true
			}
			if response.RetryAfter != "" {
				w.Header().Set("Retry-After", response.RetryAfter)
			}
			if api.operationPollHook != nil {
				api.operationPollHook(kind)
			}
			body := map[string]any{"status": response.Status}
			if response.Code != "" || response.Message != "" {
				body["error"] = map[string]string{"code": response.Code, "message": response.Message}
			}
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == http.MethodPut && strings.Contains(path, "/providers/Microsoft.Network/networkSecurityGroups/"):
			var req securityGroupRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			api.securityGroups = append(api.securityGroups, req)
			name := pathBase(path)
			_ = json.NewEncoder(w).Encode(SecurityGroup{ID: resourceID(name, "Microsoft.Network/networkSecurityGroups"), Name: name})
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/providers/Microsoft.Compute/virtualMachines"):
			_ = json.NewEncoder(w).Encode(listVirtualMachinesResponse{Value: api.existing})
		case r.Method == http.MethodPut && strings.Contains(path, "/providers/Microsoft.Network/publicIPAddresses/"):
			var req publicIPRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			api.publicIPs = append(api.publicIPs, req)
			name := pathBase(path)
			_ = json.NewEncoder(w).Encode(PublicIPAddress{ID: resourceID(name, "Microsoft.Network/publicIPAddresses"), Name: name})
		case r.Method == http.MethodPut && strings.Contains(path, "/providers/Microsoft.Network/networkInterfaces/"):
			var req networkInterfaceRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			api.networkInterfaces = append(api.networkInterfaces, req)
			name := pathBase(path)
			_ = json.NewEncoder(w).Encode(networkInterface{ID: resourceID(name, "Microsoft.Network/networkInterfaces"), Name: name})
		case r.Method == http.MethodPut && strings.Contains(path, "/providers/Microsoft.Compute/virtualMachines/"):
			var req virtualMachineRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			api.virtualMachines = append(api.virtualMachines, req)
			name := pathBase(path)
			_ = json.NewEncoder(w).Encode(VirtualMachine{Name: name, Tags: req.Tags, Location: req.Location})
		case r.Method == http.MethodGet && strings.Contains(path, "/providers/Microsoft.Network/publicIPAddresses/"):
			if api.publicIPGetFailure != nil {
				w.WriteHeader(api.publicIPGetFailure.Status)
				_, _ = w.Write([]byte(api.publicIPGetFailure.Body))
				return
			}
			name := pathBase(path)
			_ = json.NewEncoder(w).Encode(PublicIPAddress{Name: name, Properties: struct {
				IPAddress string `json:"ipAddress"`
			}{IPAddress: "203.0.113.20"}})
		case r.Method == http.MethodDelete && strings.Contains(path, "/providers/Microsoft.Compute/virtualMachines/"):
			api.handleDelete(w, "vm", pathBase(path))
		case r.Method == http.MethodDelete && strings.Contains(path, "/providers/Microsoft.Network/networkInterfaces/"):
			if _, waits := api.operations["vm"]; waits && !api.operationSucceeded["vm"] {
				api.prematureNIC = true
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":{"code":"VMStillDeleting"}}`))
				return
			}
			api.handleDelete(w, "nic", pathBase(path))
		case r.Method == http.MethodDelete && strings.Contains(path, "/providers/Microsoft.Network/publicIPAddresses/"):
			api.handleDelete(w, "pip", pathBase(path))
		default:
			t.Fatalf("%s %s", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (api *fakeAzureAPI) handleDelete(w http.ResponseWriter, kind, name string) {
	api.deletes = append(api.deletes, kind+":"+name)
	api.events = append(api.events, "delete:"+kind)
	if failure, ok := api.deleteFailures[kind]; ok {
		w.WriteHeader(failure.Status)
		_, _ = w.Write([]byte(failure.Body))
		return
	}
	if _, async := api.operations[kind]; async {
		if !api.omitOperationURL[kind] {
			header := api.operationHeaders[kind]
			if header == "" {
				header = "Azure-AsyncOperation"
			}
			w.Header().Set(header, "/operations/"+kind)
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *fakeAzureAPI) client() Client {
	return Client{
		AccessToken:      "test-token",
		SubscriptionID:   "sub-123",
		ResourceGroup:    "rg-ship",
		HTTP:             api.server.Client(),
		BaseURL:          api.server.URL,
		PollInterval:     time.Millisecond,
		OperationTimeout: 100 * time.Millisecond,
	}
}

func resourceID(name, typ string) string {
	return fmt.Sprintf("/subscriptions/sub-123/resourceGroups/rg-ship/providers/%s/%s", typ, url.PathEscape(name))
}

func pathBase(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return parts[len(parts)-1]
}
