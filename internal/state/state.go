package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	ReleaseStatusPending = "pending"
	ReleaseStatusHealthy = "healthy"
	ReleaseStatusFailed  = "failed"
)

type Release struct {
	ID            string            `json:"id"`
	Environment   string            `json:"environment"`
	Images        map[string]string `json:"images"`
	SecretDigests map[string]string `json:"secret_digests,omitempty"`
	ConfigHash    string            `json:"config_hash"`
	CreatedAt     time.Time         `json:"created_at"`
	Healthy       bool              `json:"healthy"`
	Status        string            `json:"status,omitempty"`
	Error         string            `json:"error,omitempty"`
	CompletedAt   *time.Time        `json:"completed_at,omitempty"`
	FailedAt      *time.Time        `json:"failed_at,omitempty"`
}

type HostFact struct {
	Name          string `json:"name"`
	Pool          string `json:"pool"`
	User          string `json:"user"`
	IPv4          string `json:"ipv4,omitempty"`
	PublicAddress string `json:"public_address,omitempty"`
	Provider      string `json:"provider,omitempty"`
	ProviderID    string `json:"provider_id,omitempty"`
	ServerID      int64  `json:"server_id,omitempty"`
}

type AccessoryState struct {
	Environment string            `json:"environment"`
	Name        string            `json:"name"`
	Host        HostFact          `json:"host"`
	UpdatedAt   time.Time         `json:"updated_at"`
	LastBackup  *AccessoryBackup  `json:"last_backup,omitempty"`
	LastRestore *AccessoryRestore `json:"last_restore,omitempty"`
}

type AccessoryBackup struct {
	Artifact  string    `json:"artifact"`
	Host      string    `json:"host"`
	Output    string    `json:"output,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type AccessoryRestore struct {
	Artifact  string    `json:"artifact"`
	Host      string    `json:"host"`
	Output    string    `json:"output,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Event struct {
	Time        time.Time `json:"time"`
	Environment string    `json:"environment"`
	Kind        string    `json:"kind"`
	Status      string    `json:"status"`
	Message     string    `json:"message,omitempty"`
	Release     string    `json:"release,omitempty"`
	Service     string    `json:"service,omitempty"`
	Accessory   string    `json:"accessory,omitempty"`
	Host        string    `json:"host,omitempty"`
}

type Store struct {
	Dir string
}

func NewStore(dir string) Store {
	return Store{Dir: dir}
}

func (s Store) SaveHostFacts(environment string, hosts []HostFact) error {
	if strings.TrimSpace(environment) == "" {
		return errors.New("environment is required")
	}
	dir := filepath.Join(s.Dir, "environments", environment)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(hosts, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "hosts.json"), data, 0o644)
}

func (s Store) ReadHostFacts(environment string) ([]HostFact, error) {
	environment = strings.TrimSpace(environment)
	if err := validateStateName("environment", environment); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, "environments", environment, "hosts.json"))
	if err != nil {
		return nil, err
	}
	var hosts []HostFact
	if err := json.Unmarshal(data, &hosts); err != nil {
		return nil, err
	}
	return hosts, nil
}

func (s Store) DeleteHostFacts(environment string) error {
	environment = strings.TrimSpace(environment)
	if err := validateStateName("environment", environment); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(s.Dir, "environments", environment, "hosts.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s Store) SaveAccessoryState(accessory AccessoryState) error {
	environment, name, err := validateAccessoryKey(accessory.Environment, accessory.Name)
	if err != nil {
		return err
	}
	if strings.TrimSpace(accessory.Host.Name) == "" {
		return errors.New("accessory host is required")
	}
	accessory.Environment = environment
	accessory.Name = name
	if strings.TrimSpace(accessory.Host.User) == "" {
		accessory.Host.User = "root"
	}
	if accessory.UpdatedAt.IsZero() {
		accessory.UpdatedAt = time.Now().UTC()
	}
	data, err := json.MarshalIndent(accessory, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(s.accessoryStatePath(environment, name), data, 0o644)
}

func (s Store) ReadAccessoryState(environment, name string) (AccessoryState, error) {
	environment, name, err := validateAccessoryKey(environment, name)
	if err != nil {
		return AccessoryState{}, err
	}
	data, err := os.ReadFile(s.accessoryStatePath(environment, name))
	if err != nil {
		return AccessoryState{}, err
	}
	var accessory AccessoryState
	if err := json.Unmarshal(data, &accessory); err != nil {
		return AccessoryState{}, err
	}
	return accessory, nil
}

func (s Store) AccessoryStates(environment string) ([]AccessoryState, error) {
	environment = strings.TrimSpace(environment)
	if err := validateStateName("environment", environment); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.Dir, "accessories", environment)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var accessories []AccessoryState
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var accessory AccessoryState
		if err := json.Unmarshal(data, &accessory); err != nil {
			return nil, err
		}
		accessories = append(accessories, accessory)
	}
	sort.Slice(accessories, func(i, j int) bool {
		return accessories[i].Name < accessories[j].Name
	})
	return accessories, nil
}

func (s Store) RecordAccessoryBackup(environment, name string, backup AccessoryBackup) (AccessoryState, error) {
	accessory, err := s.ReadAccessoryState(environment, name)
	if err != nil {
		return AccessoryState{}, err
	}
	if backup.CreatedAt.IsZero() {
		backup.CreatedAt = time.Now().UTC()
	}
	accessory.LastBackup = &backup
	accessory.UpdatedAt = backup.CreatedAt
	if err := s.SaveAccessoryState(accessory); err != nil {
		return AccessoryState{}, err
	}
	return accessory, nil
}

func (s Store) RecordAccessoryRestore(environment, name string, restore AccessoryRestore) (AccessoryState, error) {
	accessory, err := s.ReadAccessoryState(environment, name)
	if err != nil {
		return AccessoryState{}, err
	}
	if restore.CreatedAt.IsZero() {
		restore.CreatedAt = time.Now().UTC()
	}
	accessory.LastRestore = &restore
	accessory.UpdatedAt = restore.CreatedAt
	if err := s.SaveAccessoryState(accessory); err != nil {
		return AccessoryState{}, err
	}
	return accessory, nil
}

func (s Store) RecordEvent(event Event) error {
	event.Environment = strings.TrimSpace(event.Environment)
	event.Kind = strings.TrimSpace(event.Kind)
	event.Status = strings.TrimSpace(event.Status)
	if err := validateStateName("environment", event.Environment); err != nil {
		return err
	}
	if event.Kind == "" {
		return errors.New("event kind is required")
	}
	if event.Status == "" {
		return errors.New("event status is required")
	}
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	} else {
		event.Time = event.Time.UTC()
	}
	events, err := s.Events(event.Environment)
	if err != nil {
		return err
	}
	events = append(events, event)
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].Time.Before(events[j].Time)
	})
	data, err := json.MarshalIndent(events, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(s.eventsPath(event.Environment), data, 0o644)
}

func (s Store) Events(environment string) ([]Event, error) {
	environment = strings.TrimSpace(environment)
	if err := validateStateName("environment", environment); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.eventsPath(environment))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var events []Event
	if err := json.Unmarshal(data, &events); err != nil {
		return nil, err
	}
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].Time.Before(events[j].Time)
	})
	return events, nil
}

func (s Store) SaveRelease(release Release) error {
	release.Status = ReleaseStatusHealthy
	release.Healthy = true
	if release.CompletedAt == nil {
		completedAt := release.CreatedAt
		if completedAt.IsZero() {
			completedAt = time.Now().UTC()
		}
		release.CompletedAt = &completedAt
	}
	if err := s.SaveReleaseRecord(release); err != nil {
		return err
	}
	return s.PromoteRelease(release.ID)
}

func (s Store) accessoryStatePath(environment, name string) string {
	return filepath.Join(s.Dir, "accessories", environment, name+".json")
}

func (s Store) eventsPath(environment string) string {
	return filepath.Join(s.Dir, "events", environment+".json")
}

func (s Store) SaveReleaseRecord(release Release) error {
	if err := validateReleaseID(release.ID); err != nil {
		return err
	}
	release = normalizeRelease(release)
	releasesDir := filepath.Join(s.Dir, "releases")
	if err := os.MkdirAll(releasesDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(release, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(releasesDir, release.ID+".json")
	if err := atomicWriteFile(path, data, 0o644); err != nil {
		return err
	}
	return nil
}

func (s Store) PromoteRelease(id string) error {
	release, err := s.ReadRelease(id)
	if err != nil {
		return err
	}
	if release.Environment == "" {
		return errors.New("release environment is required")
	}
	return atomicWriteFile(filepath.Join(s.Dir, "current"), []byte(release.ID+"\n"), 0o644)
}

func (s Store) MarkReleaseHealthy(id string, at time.Time) (Release, error) {
	release, err := s.ReadRelease(id)
	if err != nil {
		return Release{}, err
	}
	release.Status = ReleaseStatusHealthy
	release.Healthy = true
	release.Error = ""
	release.FailedAt = nil
	completedAt := at.UTC()
	release.CompletedAt = &completedAt
	if err := s.SaveReleaseRecord(release); err != nil {
		return Release{}, err
	}
	if err := s.PromoteRelease(release.ID); err != nil {
		return Release{}, err
	}
	return release, nil
}

func (s Store) MarkReleaseFailed(id string, message string, at time.Time) (Release, error) {
	release, err := s.ReadRelease(id)
	if err != nil {
		return Release{}, err
	}
	release.Status = ReleaseStatusFailed
	release.Healthy = false
	release.Error = strings.TrimSpace(message)
	failedAt := at.UTC()
	release.FailedAt = &failedAt
	release.CompletedAt = nil
	if err := s.SaveReleaseRecord(release); err != nil {
		return Release{}, err
	}
	return release, nil
}

func (s Store) ReadRelease(id string) (Release, error) {
	id = strings.TrimSpace(id)
	if err := validateReleaseID(id); err != nil {
		return Release{}, err
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, "releases", id+".json"))
	if err != nil {
		return Release{}, err
	}
	var release Release
	if err := json.Unmarshal(data, &release); err != nil {
		return Release{}, err
	}
	return release, nil
}

func (s Store) CurrentRelease(environment string) (Release, error) {
	data, err := os.ReadFile(filepath.Join(s.Dir, "current"))
	if err == nil {
		release, readErr := s.ReadRelease(strings.TrimSpace(string(data)))
		if readErr != nil {
			return Release{}, readErr
		}
		if (environment == "" || release.Environment == environment) && releaseIsHealthy(release) {
			return release, nil
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Release{}, err
	}
	releases, err := s.Releases(environment)
	if err != nil {
		return Release{}, err
	}
	if len(releases) == 0 {
		if environment == "" {
			return Release{}, errors.New("no current release")
		}
		return Release{}, fmt.Errorf("no current release for %q", environment)
	}
	for i := len(releases) - 1; i >= 0; i-- {
		if releaseIsHealthy(releases[i]) {
			return releases[i], nil
		}
	}
	if environment == "" {
		return Release{}, errors.New("no current release")
	}
	return Release{}, fmt.Errorf("no current release for %q", environment)
}

func (s Store) Releases(environment string) ([]Release, error) {
	entries, err := os.ReadDir(filepath.Join(s.Dir, "releases"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var releases []Release
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.Dir, "releases", entry.Name()))
		if err != nil {
			return nil, err
		}
		var release Release
		if err := json.Unmarshal(data, &release); err != nil {
			return nil, err
		}
		if environment == "" || release.Environment == environment {
			releases = append(releases, release)
		}
	}
	sort.Slice(releases, func(i, j int) bool {
		return releases[i].CreatedAt.Before(releases[j].CreatedAt)
	})
	return releases, nil
}

func (s Store) RollbackTarget(environment string) (Release, error) {
	releases, err := s.Releases(environment)
	if err != nil {
		return Release{}, err
	}
	var healthy []Release
	for _, release := range releases {
		if releaseIsHealthy(release) {
			healthy = append(healthy, release)
		}
	}
	if len(healthy) < 2 {
		return Release{}, fmt.Errorf("no previous release for %q", environment)
	}
	current, err := s.CurrentRelease(environment)
	if err == nil {
		for i := len(healthy) - 1; i >= 0; i-- {
			if healthy[i].ID == current.ID {
				if i == 0 {
					break
				}
				return healthy[i-1], nil
			}
		}
	}
	return healthy[len(healthy)-2], nil
}

func normalizeRelease(release Release) Release {
	if strings.TrimSpace(release.Status) == "" {
		if release.Healthy {
			release.Status = ReleaseStatusHealthy
		} else {
			release.Status = ReleaseStatusPending
		}
	}
	release.Healthy = release.Status == ReleaseStatusHealthy
	return release
}

func releaseIsHealthy(release Release) bool {
	release = normalizeRelease(release)
	return release.Healthy && release.Status == ReleaseStatusHealthy
}

func validateReleaseID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("release id is required")
	}
	if id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("invalid release id %q", id)
	}
	return nil
}

func validateAccessoryKey(environment, name string) (string, string, error) {
	environment = strings.TrimSpace(environment)
	name = strings.TrimSpace(name)
	if err := validateStateName("environment", environment); err != nil {
		return "", "", err
	}
	if err := validateStateName("accessory", name); err != nil {
		return "", "", err
	}
	return environment, name, nil
}

func validateStateName(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", kind)
	}
	if value == "." || value == ".." || strings.ContainsAny(value, `/\`) {
		return fmt.Errorf("invalid %s %q", kind, value)
	}
	return nil
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".ship-state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	_ = syncDir(dir)
	return nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
