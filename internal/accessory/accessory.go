package accessory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/docker"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/state"
)

const defaultBackupTimeoutSeconds = 30 * 60

type PlanAction struct {
	Name    string
	Details string
}

type Placement struct {
	Name      string
	Host      scheduler.Host
	Persisted bool
}

func Plan(cfg *config.Config) []PlanAction {
	names := SortedNames(cfg, "")
	actions := make([]PlanAction, 0, len(names))
	for _, name := range names {
		acc := cfg.Accessories[name]
		details := fmt.Sprintf("image=%s pool=%s", acc.Image, acc.Pool)
		if acc.Primary {
			details += " primary"
		}
		if acc.Backup.Required {
			details += " backup-required"
		}
		actions = append(actions, PlanAction{Name: name, Details: details})
	}
	return actions
}

func SortedNames(cfg *config.Config, only string) []string {
	if cfg == nil {
		return nil
	}
	if strings.TrimSpace(only) != "" {
		if _, ok := cfg.Accessories[only]; ok {
			return []string{only}
		}
		return nil
	}
	names := make([]string, 0, len(cfg.Accessories))
	for name := range cfg.Accessories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func ValidateDeploy(acc config.Accessory) error {
	if strings.TrimSpace(acc.Image) == "" {
		return errors.New("accessory image is required")
	}
	if strings.TrimSpace(acc.Pool) == "" {
		return errors.New("accessory pool is required")
	}
	if acc.Backup.Required && strings.TrimSpace(acc.Backup.Command) == "" {
		return errors.New("backup.required accessories require backup.command")
	}
	return nil
}

func ValidateRestore(acc config.Accessory) error {
	if !acc.Primary {
		return fmt.Errorf("restore is only supported for primary accessories")
	}
	if !acc.Backup.Required {
		return fmt.Errorf("restore requires backup.required=true")
	}
	if strings.TrimSpace(acc.Backup.Command) == "" {
		return fmt.Errorf("restore requires backup.command")
	}
	if strings.TrimSpace(acc.Backup.RestoreCommand) == "" {
		return fmt.Errorf("restore requires backup.restore_command")
	}
	return nil
}

func PlacementFor(cfg *config.Config, env config.Environment, envName, name string, store state.Store) (Placement, error) {
	return PlacementForHosts(cfg, scheduler.HostsForEnvironment(env), envName, name, store)
}

func PlacementForHosts(cfg *config.Config, hosts []scheduler.Host, envName, name string, store state.Store) (Placement, error) {
	acc, ok := cfg.Accessories[name]
	if !ok {
		return Placement{}, fmt.Errorf("unknown accessory %q", name)
	}
	eligible := eligibleHosts(hosts, acc.Pool)
	if len(eligible) == 0 {
		return Placement{}, fmt.Errorf("accessory %q pool %q has no hosts", name, acc.Pool)
	}
	if saved, err := store.ReadAccessoryState(envName, name); err == nil {
		if host, ok := matchingHost(saved.Host, eligible); ok {
			return Placement{Name: name, Host: host, Persisted: true}, nil
		}
		return Placement{}, fmt.Errorf("accessory %q saved placement host %q is not eligible in pool %q; failover is not implemented", name, saved.Host.Name, acc.Pool)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Placement{}, err
	}
	return Placement{Name: name, Host: eligible[0]}, nil
}

func EnsurePlacement(cfg *config.Config, env config.Environment, envName, name string, store state.Store, now time.Time) (Placement, error) {
	return EnsurePlacementForHosts(cfg, scheduler.HostsForEnvironment(env), envName, name, store, now)
}

func EnsurePlacementForHosts(cfg *config.Config, hosts []scheduler.Host, envName, name string, store state.Store, now time.Time) (Placement, error) {
	placement, err := PlacementForHosts(cfg, hosts, envName, name, store)
	if err != nil {
		return Placement{}, err
	}
	if placement.Persisted {
		return placement, nil
	}
	record := state.AccessoryState{
		Environment: envName,
		Name:        name,
		Host:        HostFact(placement.Host),
		UpdatedAt:   now.UTC(),
	}
	if existing, err := store.ReadAccessoryState(envName, name); err == nil {
		record.LastBackup = existing.LastBackup
		record.LastRestore = existing.LastRestore
	}
	if err := store.SaveAccessoryState(record); err != nil {
		return Placement{}, err
	}
	placement.Persisted = true
	return placement, nil
}

func HostFact(host scheduler.Host) state.HostFact {
	return state.HostFact{Name: host.Name, Pool: host.Pool, User: host.User, PublicAddress: host.Contact}
}

func HostFromFact(fact state.HostFact) scheduler.Host {
	user := fact.User
	if user == "" {
		user = "root"
	}
	contact := strings.TrimSpace(fact.PublicAddress)
	if contact == "" {
		contact = strings.TrimSpace(fact.IPv4)
	}
	return scheduler.Host{Name: fact.Name, Pool: fact.Pool, User: user, Contact: contact}
}

func ContainerName(project, envName, name string) string {
	parts := []string{"ship", safeNamePart(project), safeNamePart(envName), "accessory", safeNamePart(name)}
	return strings.Join(parts, "_")
}

func ContainerLabels(project, envName, name string) map[string]string {
	return map[string]string{
		docker.LabelProject:     safeLabelValue(project),
		docker.LabelEnvironment: safeLabelValue(envName),
		docker.LabelAccessory:   safeLabelValue(name),
	}
}

func DockerArgs(acc config.Accessory, envFiles ...string) []string {
	var args []string
	for _, env := range acc.Env {
		if strings.TrimSpace(env) != "" {
			args = append(args, "-e", env)
		}
	}
	for _, envFile := range envFiles {
		if strings.TrimSpace(envFile) != "" {
			args = append(args, "--env-file", envFile)
		}
	}
	for _, port := range acc.Ports {
		args = append(args, "-p", fmt.Sprintf("%d:%d", port, port))
	}
	for _, volume := range acc.Volumes {
		if strings.TrimSpace(volume) != "" {
			args = append(args, "-v", volume)
		}
	}
	return args
}

func NamedVolumes(acc config.Accessory) []string {
	seen := map[string]struct{}{}
	var volumes []string
	for _, spec := range acc.Volumes {
		name, ok := namedVolume(spec)
		if !ok {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		volumes = append(volumes, name)
	}
	sort.Strings(volumes)
	return volumes
}

func BackupArtifactPath(acc config.Accessory, envName, name string, now time.Time) string {
	dir := BackupArtifactDir(acc, envName, name)
	file := safeNamePart(name) + "-" + now.UTC().Format("20060102T150405.000000000Z") + ".backup"
	return filepath.Join(dir, file)
}

func BackupArtifactDir(acc config.Accessory, envName, name string) string {
	dir := strings.TrimSpace(acc.Backup.ArtifactDir)
	if dir == "" {
		dir = filepath.Join(config.RemoteStateDir, "accessories", envName, name, "backups")
	}
	return filepath.Clean(dir)
}

func ValidateRestoreArtifact(acc config.Accessory, envName, name, artifact string) (string, error) {
	artifact = strings.TrimSpace(artifact)
	if artifact == "" {
		return "", errors.New("backup artifact path is required")
	}
	artifact = filepath.Clean(artifact)
	if !strings.HasSuffix(artifact, ".backup") {
		return "", fmt.Errorf("restore artifact %q must be a .backup file", artifact)
	}
	dir := BackupArtifactDir(acc, envName, name)
	rel, err := filepath.Rel(dir, artifact)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("restore artifact %q must be within backup artifact directory %q", artifact, dir)
	}
	return artifact, nil
}

func BackupCommand(acc config.Accessory, artifact string) (string, error) {
	command := strings.TrimSpace(acc.Backup.Command)
	if command == "" {
		return "", errors.New("backup.command is required")
	}
	artifact = strings.TrimSpace(artifact)
	if artifact == "" {
		return "", errors.New("backup artifact path is required")
	}
	tmp := artifact + ".tmp"
	return fmt.Sprintf("mkdir -p %s && ( %s ) > %s && test -s %s && mv %s %s && printf '%%s\\n' %s",
		shellQuote(filepath.Dir(artifact)),
		command,
		shellQuote(tmp),
		shellQuote(tmp),
		shellQuote(tmp),
		shellQuote(artifact),
		shellQuote(artifact),
	), nil
}

func RestoreCheckCommand(acc config.Accessory, envName, name, artifact string) (string, error) {
	artifact, err := ValidateRestoreArtifact(acc, envName, name, artifact)
	if err != nil {
		return "", err
	}
	return "test -s " + shellQuote(artifact), nil
}

func RestoreCommand(acc config.Accessory, artifact string) (string, error) {
	if err := ValidateRestore(acc); err != nil {
		return "", err
	}
	artifact = strings.TrimSpace(artifact)
	if artifact == "" {
		return "", errors.New("backup artifact path is required")
	}
	return "SHIP_BACKUP_ARTIFACT=" + shellQuote(artifact) + " " + strings.TrimSpace(acc.Backup.RestoreCommand), nil
}

func BackupTimeoutSeconds(acc config.Accessory) int {
	return defaultBackupTimeoutSeconds
}

func MatchesLabels(cfg *config.Config, envName, name string, labels map[string]string) bool {
	if labels == nil {
		return false
	}
	return labels[docker.LabelManagedBy] == docker.LabelManagedByValue &&
		labels[docker.LabelProject] == safeLabelValue(cfg.Project) &&
		labels[docker.LabelEnvironment] == safeLabelValue(envName) &&
		labels[docker.LabelAccessory] == safeLabelValue(name)
}

func eligibleHosts(hosts []scheduler.Host, pool string) []scheduler.Host {
	var eligible []scheduler.Host
	for _, host := range hosts {
		if host.Pool == pool {
			eligible = append(eligible, host)
		}
	}
	return eligible
}

func matchingHost(saved state.HostFact, eligible []scheduler.Host) (scheduler.Host, bool) {
	for _, host := range eligible {
		if host.Name == saved.Name {
			return host, true
		}
	}
	return scheduler.Host{}, false
}

func namedVolume(spec string) (string, bool) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", false
	}
	source, _, ok := strings.Cut(spec, ":")
	if !ok {
		return "", false
	}
	source = strings.TrimSpace(source)
	if source == "" || strings.HasPrefix(source, "/") || strings.HasPrefix(source, ".") {
		return "", false
	}
	return source, true
}

func safeNamePart(value string) string {
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
	out := strings.Trim(b.String(), ".-_")
	if out == "" {
		return "unknown"
	}
	if _, err := strconv.Atoi(out[:1]); err == nil {
		return "x" + out
	}
	return out
}

func safeLabelValue(value string) string {
	return safeNamePart(value)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
