package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/state"
)

var runLocalHookCommand = defaultRunLocalHookCommand

var sendWebhookNotification = defaultSendWebhookNotification

type hookContext struct {
	Project     string
	Environment string
	Hook        string
	ReleaseID   string
	ConfigPath  string
	ConfigDir   string
	Failure     string
}

type notificationPayload struct {
	Project     string            `json:"project"`
	Environment string            `json:"environment"`
	Operation   string            `json:"operation"`
	Status      string            `json:"status"`
	Release     string            `json:"release,omitempty"`
	Message     string            `json:"message,omitempty"`
	Images      map[string]string `json:"images,omitempty"`
	Time        time.Time         `json:"time"`
}

func runDeployHooks(ctx context.Context, w io.Writer, store state.Store, cfg *config.Config, envName, hookName, releaseID, failure, configPath string) error {
	hooks := deployHooksFor(cfg.Hooks, hookName)
	if len(hooks) == 0 {
		return nil
	}
	configDir := "."
	if strings.TrimSpace(configPath) != "" {
		configDir = filepath.Dir(configPath)
		if abs, err := filepath.Abs(configDir); err == nil {
			configDir = abs
		}
	}
	hctx := hookContext{
		Project:     cfg.Project,
		Environment: envName,
		Hook:        hookName,
		ReleaseID:   releaseID,
		ConfigPath:  configPath,
		ConfigDir:   configDir,
		Failure:     failure,
	}
	for i, hook := range hooks {
		message := fmt.Sprintf("hook=%s index=%d command=%q", hookName, i, hook.Command)
		recordEvent(store, state.Event{Environment: envName, Kind: "deploy_hook", Status: "started", Release: releaseID, Message: message})
		if err := runLocalHookCommand(ctx, hook, hctx, w); err != nil {
			recordEvent(store, state.Event{Environment: envName, Kind: "deploy_hook", Status: "failed", Release: releaseID, Message: message + ": " + err.Error()})
			return err
		}
		recordEvent(store, state.Event{Environment: envName, Kind: "deploy_hook", Status: "succeeded", Release: releaseID, Message: message})
	}
	return nil
}

func runNotifications(ctx context.Context, store state.Store, cfg *config.Config, envName, operation, status, releaseID, message string, images map[string]string) {
	webhooks := cfg.Notifications.Webhooks
	if len(webhooks) == 0 {
		return
	}
	payload := notificationPayload{
		Project:     cfg.Project,
		Environment: envName,
		Operation:   operation,
		Status:      status,
		Release:     releaseID,
		Message:     message,
		Images:      copyStringMap(images),
		Time:        deployNow().UTC(),
	}
	eventName := operation + ":" + status
	for i, webhook := range webhooks {
		if !webhookWantsEvent(webhook, eventName) {
			continue
		}
		label := fmt.Sprintf("operation=%s status=%s index=%d", operation, status, i)
		if err := sendWebhookNotification(ctx, webhook, payload); err != nil {
			recordEvent(store, state.Event{Environment: envName, Kind: "notification", Status: "failed", Release: releaseID, Message: label + ": " + err.Error()})
			continue
		}
		recordEvent(store, state.Event{Environment: envName, Kind: "notification", Status: "succeeded", Release: releaseID, Message: label})
	}
}

func webhookWantsEvent(webhook config.WebhookNotification, eventName string) bool {
	if len(webhook.Events) == 0 {
		return true
	}
	for _, raw := range webhook.Events {
		event := strings.TrimSpace(raw)
		if event == "*" || event == eventName {
			return true
		}
		if strings.HasSuffix(event, ":*") && strings.HasPrefix(eventName, strings.TrimSuffix(event, "*")) {
			return true
		}
	}
	return false
}

func defaultSendWebhookNotification(ctx context.Context, webhook config.WebhookNotification, payload notificationPayload) error {
	url := strings.TrimSpace(webhook.URL)
	if url == "" {
		envName := strings.TrimSpace(webhook.URLEnv)
		if envName == "" {
			return fmt.Errorf("webhook url or url_env is required")
		}
		value, ok := os.LookupEnv(envName)
		if !ok || strings.TrimSpace(value) == "" {
			return fmt.Errorf("webhook url_env %s is not set", envName)
		}
		url = strings.TrimSpace(value)
	}
	if webhook.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(webhook.TimeoutSeconds)*time.Second)
		defer cancel()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range webhook.Headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func deployHooksFor(hooks config.Hooks, hookName string) []config.HookCommand {
	switch hookName {
	case "pre_deploy":
		return hooks.PreDeploy
	case "pre_build":
		return hooks.PreBuild
	case "post_deploy":
		return hooks.PostDeploy
	case "deploy_failed":
		return hooks.DeployFailed
	default:
		return nil
	}
}

func defaultRunLocalHookCommand(ctx context.Context, hook config.HookCommand, hctx hookContext, w io.Writer) error {
	if hook.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(hook.TimeoutSeconds)*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "sh", "-lc", hook.Command)
	cmd.Dir = hctx.ConfigDir
	cmd.Env = hookEnv(os.Environ(), hook, hctx)
	output, err := cmd.CombinedOutput()
	if len(output) > 0 {
		_, _ = w.Write(output)
	}
	if err != nil {
		if len(output) > 0 {
			return fmt.Errorf("hook %s command %q failed: %w: %s", hctx.Hook, hook.Command, err, strings.TrimSpace(string(output)))
		}
		return fmt.Errorf("hook %s command %q failed: %w", hctx.Hook, hook.Command, err)
	}
	return nil
}

func hookEnv(base []string, hook config.HookCommand, hctx hookContext) []string {
	env := append([]string{}, base...)
	shipValues := map[string]string{
		"SHIP_PROJECT":     hctx.Project,
		"SHIP_ENVIRONMENT": hctx.Environment,
		"SHIP_HOOK":        hctx.Hook,
		"SHIP_RELEASE":     hctx.ReleaseID,
		"SHIP_CONFIG":      hctx.ConfigPath,
		"SHIP_CONFIG_DIR":  hctx.ConfigDir,
		"SHIP_FAILURE":     hctx.Failure,
	}
	for _, key := range sortedMapKeys(shipValues) {
		env = append(env, key+"="+shipValues[key])
	}
	for _, key := range sortedMapKeys(hook.Env) {
		env = append(env, key+"="+hook.Env[key])
	}
	return env
}
