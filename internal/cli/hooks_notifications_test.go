package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/state"
)

func TestDeployRunsLifecycleHooksInOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(hookDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var events []string
	var hookRuns []hookRun
	releaseID := "abc123def456-20260630T183456.123456789Z"
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	installDeployHooks(t, &recordingDeployDocker{events: &events, resolved: map[string]string{tag: digestRef}}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events}
	})
	installLocalHookRunner(t, &hookRuns, "", &events)

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	wantHooks := []string{
		"pre_deploy:echo root-pre",
		"pre_deploy:echo env-pre",
		"pre_build:echo pre-build",
		"post_deploy:echo post-deploy",
	}
	var gotHooks []string
	for _, run := range hookRuns {
		gotHooks = append(gotHooks, run.Context.Hook+":"+run.Command)
		if run.Context.Project != "demo" || run.Context.Environment != "production" || run.Context.ReleaseID != releaseID || run.Context.ConfigPath != path {
			t.Fatalf("hook context = %+v", run.Context)
		}
	}
	if strings.Join(gotHooks, "\n") != strings.Join(wantHooks, "\n") {
		t.Fatalf("hooks = %#v, want %#v", gotHooks, wantHooks)
	}
	joined := strings.Join(events, "\n")
	preBuildAt := strings.Index(joined, "hook:pre_build:echo pre-build")
	buildAt := strings.Index(joined, "build:"+tag)
	postDeployAt := strings.Index(joined, "hook:post_deploy:echo post-deploy")
	healthyAt := strings.Index(joined, "agent:web-1:write_release_state:"+releaseID+":healthy")
	if preBuildAt < 0 || buildAt < 0 || preBuildAt > buildAt {
		t.Fatalf("pre-build hook did not precede build:\n%s", joined)
	}
	if postDeployAt < 0 || healthyAt < 0 || postDeployAt > healthyAt {
		t.Fatalf("post-deploy hook did not precede healthy promotion:\n%s", joined)
	}
	if hookRuns[3].Hook.Env["SMOKE"] != "1" || hookRuns[3].Hook.TimeoutSeconds != 3 {
		t.Fatalf("post deploy hook config = %+v", hookRuns[3].Hook)
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "deploy_hook", "started", releaseID) || !timelineContains(timeline, "deploy_hook", "succeeded", releaseID) {
		t.Fatalf("timeline missing deploy hook events: %+v", timeline)
	}
}

func TestDeployRunsFailureHookWhenLifecycleHookFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(hookDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var events []string
	var hookRuns []hookRun
	installDeployHooks(t, panicDeployDocker{t: t}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events}
	})
	installLocalHookRunner(t, &hookRuns, "pre_build", &events)

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "pre_build hook failed") {
		t.Fatalf("expected pre_build hook failure, got %v", err)
	}
	var gotHooks []string
	for _, run := range hookRuns {
		gotHooks = append(gotHooks, run.Context.Hook+":"+run.Command)
	}
	wantHooks := []string{
		"pre_deploy:echo root-pre",
		"pre_deploy:echo env-pre",
		"pre_build:echo pre-build",
		"deploy_failed:echo failed",
	}
	if strings.Join(gotHooks, "\n") != strings.Join(wantHooks, "\n") {
		t.Fatalf("hooks = %#v, want %#v", gotHooks, wantHooks)
	}
	if !strings.Contains(hookRuns[len(hookRuns)-1].Context.Failure, "pre_build hook failed") {
		t.Fatalf("failure hook context = %+v", hookRuns[len(hookRuns)-1].Context)
	}
	var mutating []string
	for _, event := range events {
		if !strings.HasSuffix(event, ":negotiate") {
			mutating = append(mutating, event)
		}
	}
	if len(mutating) != 4 {
		t.Fatalf("unexpected deploy events after hook failure: %#v", events)
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "deploy_hook", "failed", "abc123def456-20260630T183456.123456789Z") || !timelineContains(timeline, "deploy", "failed", "abc123def456-20260630T183456.123456789Z") {
		t.Fatalf("timeline missing failed hook/deploy: %+v", timeline)
	}
}

func TestDeploySendsWebhookNotifications(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(notificationDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var events []string
	var deliveries []webhookDelivery
	releaseID := "abc123def456-20260630T183456.123456789Z"
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	installDeployHooks(t, &recordingDeployDocker{events: &events, resolved: map[string]string{tag: digestRef}}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events}
	})
	installWebhookNotifier(t, &deliveries, nil)

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	if len(deliveries) != 2 {
		t.Fatalf("deliveries = %+v", deliveries)
	}
	if deliveries[0].Webhook.URL != "https://hooks.example/deploys" || deliveries[1].Webhook.URLEnv != "SHIP_NOTIFY_WEBHOOK" {
		t.Fatalf("notification webhook order = %+v", deliveries)
	}
	payload := deliveries[0].Payload
	if payload.Project != "demo" || payload.Environment != "production" || payload.Operation != "deploy" || payload.Status != "succeeded" || payload.Release != releaseID {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Images["web"] != digestRef {
		t.Fatalf("payload images = %+v", payload.Images)
	}
	if !payload.Time.Equal(deployNow().UTC()) {
		t.Fatalf("payload time = %s", payload.Time)
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "notification", "succeeded", releaseID) {
		t.Fatalf("timeline missing notification success: %+v", timeline)
	}
}

func TestDeployFailureSendsWebhookNotificationWithoutMaskingError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(notificationDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var events []string
	var deliveries []webhookDelivery
	installDeployHooks(t, &recordingDeployDocker{events: &events, failStage: "build"}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events}
	})
	installWebhookNotifier(t, &deliveries, nil)

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "build failed") {
		t.Fatalf("expected build failure, got %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %+v", deliveries)
	}
	payload := deliveries[0].Payload
	if payload.Operation != "deploy" || payload.Status != "failed" || !strings.Contains(payload.Message, "build failed") {
		t.Fatalf("payload = %+v", payload)
	}
	if len(payload.Images) != 0 {
		t.Fatalf("failed payload images = %+v", payload.Images)
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	timeline, timelineErr := store.Events("production")
	if timelineErr != nil {
		t.Fatal(timelineErr)
	}
	if !timelineContains(timeline, "notification", "succeeded", payload.Release) {
		t.Fatalf("timeline missing notification success: %+v", timeline)
	}
}

func TestDeployWebhookNotificationFailureIsRecordedButNonFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(notificationDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var events []string
	var deliveries []webhookDelivery
	releaseID := "abc123def456-20260630T183456.123456789Z"
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	installDeployHooks(t, &recordingDeployDocker{events: &events, resolved: map[string]string{tag: digestRef}}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events}
	})
	installWebhookNotifier(t, &deliveries, errors.New("webhook unavailable"))

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 2 {
		t.Fatalf("deliveries = %+v", deliveries)
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "deploy", "succeeded", releaseID) || !timelineContains(timeline, "notification", "failed", releaseID) {
		t.Fatalf("timeline missing deploy success/notification failure: %+v", timeline)
	}
}

func TestDefaultSendWebhookNotificationPostsJSON(t *testing.T) {
	var gotMethod string
	var gotContentType string
	var gotAuth string
	var gotPayload notificationPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	t.Setenv("SHIP_WEBHOOK_URL", server.URL)

	payload := notificationPayload{
		Project:     "demo",
		Environment: "production",
		Operation:   "deploy",
		Status:      "succeeded",
		Release:     "release-1",
		Images:      map[string]string{"web": "image"},
		Time:        time.Unix(10, 0).UTC(),
	}
	err := defaultSendWebhookNotification(context.Background(), config.WebhookNotification{
		URLEnv:         "SHIP_WEBHOOK_URL",
		Headers:        map[string]string{"Authorization": "Bearer test"},
		TimeoutSeconds: 1,
	}, payload)
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotContentType != "application/json" || gotAuth != "Bearer test" {
		t.Fatalf("request method/content-type/auth = %q %q %q", gotMethod, gotContentType, gotAuth)
	}
	if gotPayload.Project != payload.Project || gotPayload.Images["web"] != "image" {
		t.Fatalf("payload = %+v", gotPayload)
	}
}

func hookDeployConfig() string {
	return `project: demo
registry: registry.local/acme/demo

hooks:
  pre_deploy:
    - echo root-pre
  pre_build:
    - echo pre-build
  deploy_failed:
    - echo failed

environments:
  production:
    provider:
      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    hosts:
      pools:
        web:
          count: 1
    hooks:
      pre_deploy:
        - echo env-pre
      post_deploy:
        - command: echo post-deploy
          timeout_seconds: 3
          env:
            SMOKE: "1"

services:
  web:
    image:
      build: .
    command: ./bin/server
    pool: web
    scale: 1
    ports: [3000]
`
}

func notificationDeployConfig() string {
	return `project: demo
registry: registry.local/acme/demo

notifications:
  webhooks:
    - url: https://hooks.example/deploys
      events: [deploy:*]
      headers:
        X-Ship: root

environments:
  production:
    provider:
      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    hosts:
      pools:
        web:
          count: 1
    notifications:
      webhooks:
        - url_env: SHIP_NOTIFY_WEBHOOK
          events: [deploy:succeeded]
          timeout_seconds: 3
          headers:
            X-Env: production

services:
  web:
    image:
      build: .
    command: ./bin/server
    pool: web
    scale: 1
    ports: [3000]
`
}

type hookRun struct {
	Command   string
	Context   hookContext
	Hook      config.HookCommand
	ReleaseID string
}

func installLocalHookRunner(t *testing.T, runs *[]hookRun, failHook string, events *[]string) {
	t.Helper()
	originalRunner := runLocalHookCommand
	runLocalHookCommand = func(ctx context.Context, hook config.HookCommand, hctx hookContext, w io.Writer) error {
		*runs = append(*runs, hookRun{Command: hook.Command, Context: hctx, Hook: hook, ReleaseID: hctx.ReleaseID})
		if events != nil {
			*events = append(*events, "hook:"+hctx.Hook+":"+hook.Command)
		}
		if hctx.Hook == failHook {
			return fmt.Errorf("%s hook failed", hctx.Hook)
		}
		return nil
	}
	t.Cleanup(func() {
		runLocalHookCommand = originalRunner
	})
}

type webhookDelivery struct {
	Webhook config.WebhookNotification
	Payload notificationPayload
}

func installWebhookNotifier(t *testing.T, deliveries *[]webhookDelivery, err error) {
	t.Helper()
	originalNotifier := sendWebhookNotification
	sendWebhookNotification = func(ctx context.Context, webhook config.WebhookNotification, payload notificationPayload) error {
		*deliveries = append(*deliveries, webhookDelivery{Webhook: webhook, Payload: payload})
		return err
	}
	t.Cleanup(func() {
		sendWebhookNotification = originalNotifier
	})
}
