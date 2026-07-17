package cli

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/watzon/ship/internal/accessory"
	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/deployment"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/state"
)

func hasManagedSchedules(cfg *config.Config) bool {
	for _, svc := range cfg.Services {
		if len(svc.Schedules) > 0 {
			return true
		}
	}
	for _, acc := range cfg.Accessories {
		if strings.TrimSpace(acc.Backup.Schedule.Cron) != "" {
			return true
		}
	}
	return false
}

func syncManagedSchedules(ctx context.Context, cfg *config.Config, hosts []scheduler.Host, envName, releaseID string, store state.Store) error {
	prefix := scheduleFilePrefix(cfg.Project, envName)
	filesByHost, err := managedScheduleFiles(cfg, hosts, envName, releaseID, prefix, store)
	if err != nil {
		return err
	}
	var failures []string
	for _, host := range hosts {
		params := agent.SyncCronFilesParams{
			Prefix: prefix,
			Files:  filesByHost[host.Name],
		}
		if err := newDeployAgent(host).Call(ctx, "sync_cron_files", params, nil); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", host.Name, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("sync schedules failed on %d/%d hosts: %s", len(failures), len(hosts), strings.Join(failures, "; "))
	}
	return nil
}

func managedScheduleFiles(cfg *config.Config, hosts []scheduler.Host, envName, releaseID, prefix string, store state.Store) (map[string][]agent.CronFile, error) {
	filesByHost := map[string][]agent.CronFile{}
	for _, host := range hosts {
		filesByHost[host.Name] = nil
	}
	if err := addServiceScheduleFiles(filesByHost, cfg, hosts, envName, releaseID, prefix); err != nil {
		return nil, err
	}
	if err := addAccessoryBackupScheduleFiles(filesByHost, cfg, hosts, envName, prefix, store); err != nil {
		return nil, err
	}
	return filesByHost, nil
}

func addServiceScheduleFiles(filesByHost map[string][]agent.CronFile, cfg *config.Config, hosts []scheduler.Host, envName, releaseID, prefix string) error {
	placements, err := scheduler.PlaceServicesOnHosts(cfg, hosts)
	if err != nil {
		return err
	}
	placementByServiceReplica := map[string]scheduler.Placement{}
	for _, placement := range placements {
		placementByServiceReplica[schedulePlacementKey(placement.Service, placement.Replica)] = placement
	}
	for _, serviceName := range sortedServiceNames(cfg.Services) {
		svc := cfg.Services[serviceName]
		scheduleNames := make([]string, 0, len(svc.Schedules))
		for name := range svc.Schedules {
			scheduleNames = append(scheduleNames, name)
		}
		sort.Strings(scheduleNames)
		for _, scheduleName := range scheduleNames {
			schedule := svc.Schedules[scheduleName]
			replica := schedule.Replica
			if replica == 0 {
				replica = 1
			}
			placement, ok := placementByServiceReplica[schedulePlacementKey(serviceName, replica)]
			if !ok {
				return fmt.Errorf("schedule %s.%s references unplaced replica %d", serviceName, scheduleName, replica)
			}
			container := deployment.ContainerName(cfg.Project, envName, serviceName, replica, releaseID)
			fileName := prefix + safeCronName(serviceName) + "-" + safeCronName(scheduleName)
			content := renderCronFile(schedule, container, fileName)
			filesByHost[placement.Host.Name] = append(filesByHost[placement.Host.Name], agent.CronFile{Name: fileName, Content: content})
		}
	}
	return nil
}

func addAccessoryBackupScheduleFiles(filesByHost map[string][]agent.CronFile, cfg *config.Config, hosts []scheduler.Host, envName, prefix string, store state.Store) error {
	for _, name := range accessory.SortedNames(cfg, "") {
		acc := cfg.Accessories[name]
		if strings.TrimSpace(acc.Backup.Schedule.Cron) == "" {
			continue
		}
		if strings.TrimSpace(acc.Backup.Command) == "" {
			return fmt.Errorf("accessory %q backup.schedule requires backup.command", name)
		}
		placement, err := accessory.PlacementForHosts(cfg, hosts, envName, name, store)
		if err != nil {
			return err
		}
		if !placement.Persisted {
			return fmt.Errorf("accessory %q backup.schedule requires a saved placement; run accessory deploy first", name)
		}
		fileName := prefix + "accessory-" + safeCronName(name) + "-backup"
		content, err := renderAccessoryBackupCronFile(acc, envName, name, fileName)
		if err != nil {
			return err
		}
		filesByHost[placement.Host.Name] = append(filesByHost[placement.Host.Name], agent.CronFile{Name: fileName, Content: content})
	}
	return nil
}

func schedulePlacementKey(service string, replica int) string {
	return service + "\x00" + strconv.Itoa(replica)
}

func scheduleFilePrefix(project, envName string) string {
	return "ship-" + safeCronName(project) + "-" + safeCronName(envName) + "-"
}

func renderCronFile(schedule config.Schedule, container, fileName string) string {
	command := "docker exec " + shellQuote(container) + " sh -lc " + shellQuote(schedule.Command)
	if schedule.TimeoutSeconds > 0 {
		command = "timeout " + strconv.Itoa(schedule.TimeoutSeconds) + "s " + command
	}
	logPath := "/var/log/" + fileName + ".log"
	return strings.TrimSpace(schedule.Cron) + " root " + escapeCronCommand(command+" >> "+shellQuote(logPath)+" 2>&1") + "\n"
}

func renderAccessoryBackupCronFile(acc config.Accessory, envName, name, fileName string) (string, error) {
	backupCommand := strings.TrimSpace(acc.Backup.Command)
	if backupCommand == "" {
		return "", fmt.Errorf("backup.command is required")
	}
	exportCommand := strings.TrimSpace(acc.Backup.ExportCommand)
	dir := accessory.BackupArtifactDir(acc, envName, name)
	filePrefix := safeCronName(name) + "-"
	parts := []string{
		"artifact_dir=" + shellQuote(dir),
		"artifact=\"$artifact_dir/" + filePrefix + "$(date -u +%Y%m%dT%H%M%S.000000000Z).backup\"",
		"tmp=\"$artifact.tmp\"",
		"mkdir -p \"$artifact_dir\"",
		"( " + backupCommand + " ) > \"$tmp\"",
		"test -s \"$tmp\"",
		"mv \"$tmp\" \"$artifact\"",
	}
	if exportCommand != "" {
		parts = append(parts,
			"export_output=$(SHIP_BACKUP_ARTIFACT=\"$artifact\"; export SHIP_BACKUP_ARTIFACT; "+exportCommand+")",
			"if [ -n \"$export_output\" ]; then printf '%s\\n' \"$export_output\"; fi",
		)
	}
	parts = append(parts,
		"printf '%s\\n' \"$artifact\"",
	)
	command := strings.Join(parts, " && ")
	if acc.Backup.Schedule.TimeoutSeconds > 0 {
		command = "timeout " + strconv.Itoa(acc.Backup.Schedule.TimeoutSeconds) + "s " + command
	}
	logPath := "/var/log/" + fileName + ".log"
	return strings.TrimSpace(acc.Backup.Schedule.Cron) + " root " + escapeCronCommand(command+" >> "+shellQuote(logPath)+" 2>&1") + "\n", nil
}

func safeCronName(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		allowed := r >= 'a' && r <= 'z' ||
			r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' ||
			r == '_' || r == '.' || r == '-'
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
	out := strings.Trim(b.String(), ".-")
	if out == "" {
		return "x"
	}
	return out
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func escapeCronCommand(value string) string {
	return strings.ReplaceAll(value, "%", `\%`)
}
