package deployment

import (
	"errors"
	"sort"
	"strconv"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/docker"
	"github.com/watzon/ship/internal/scheduler"
)

type StatusInput struct {
	Config         *config.Config
	Environment    config.Environment
	Hosts          []scheduler.Host
	EnvName        string
	CurrentRelease string
	Observed       []ObservedContainer
}

type StatusReport struct {
	Environment    string                 `json:"environment"`
	CurrentRelease string                 `json:"current_release,omitempty"`
	Desired        []DesiredReplicaStatus `json:"desired"`
	Observed       []ContainerStatus      `json:"observed"`
	ExtraObserved  []ContainerStatus      `json:"extra_observed,omitempty"`
	Summary        StatusSummary          `json:"summary"`
}

type StatusSummary struct {
	Desired      int  `json:"desired"`
	Observed     int  `json:"observed"`
	Missing      int  `json:"missing"`
	WrongRelease int  `json:"wrong_release"`
	WrongHost    int  `json:"wrong_host"`
	Extra        int  `json:"extra"`
	Drift        bool `json:"drift"`
}

type DesiredReplicaStatus struct {
	Service        string            `json:"service"`
	Replica        int               `json:"replica"`
	Host           string            `json:"host"`
	DesiredRelease string            `json:"desired_release,omitempty"`
	DesiredName    string            `json:"desired_name,omitempty"`
	State          string            `json:"state"`
	Observed       []ContainerStatus `json:"observed,omitempty"`
	Drift          []string          `json:"drift,omitempty"`
}

type ContainerStatus struct {
	Host      string            `json:"host"`
	Name      string            `json:"name"`
	ID        string            `json:"id,omitempty"`
	Image     string            `json:"image,omitempty"`
	Status    string            `json:"status,omitempty"`
	Kind      string            `json:"kind,omitempty"`
	Service   string            `json:"service,omitempty"`
	Accessory string            `json:"accessory,omitempty"`
	Replica   int               `json:"replica,omitempty"`
	Release   string            `json:"release,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type desiredKey struct {
	service string
	replica int
}

func AggregateStatus(input StatusInput) (StatusReport, error) {
	if input.Config == nil {
		return StatusReport{}, errors.New("config is required")
	}
	placements, err := scheduler.PlaceServicesOnHosts(input.Config, inputHosts(input.Environment, input.Hosts))
	if err != nil {
		return StatusReport{}, err
	}

	report := StatusReport{
		Environment:    input.EnvName,
		CurrentRelease: input.CurrentRelease,
	}
	desiredHosts := map[desiredKey]scheduler.Host{}
	exactDesired := map[string]struct{}{}
	for _, placement := range placements {
		key := desiredKey{service: placement.Service, replica: placement.Replica}
		desiredHosts[key] = placement.Host
		if input.CurrentRelease != "" {
			exactDesired[placement.Host.Name+"\x00"+ContainerName(input.Config.Project, input.EnvName, placement.Service, placement.Replica, input.CurrentRelease)] = struct{}{}
		}
	}

	observedByDesired := map[desiredKey][]ContainerStatus{}
	for _, item := range input.Observed {
		if !matchesShipScope(input.Config, input.EnvName, item.Container.Labels) {
			continue
		}
		status := containerStatus(item)
		report.Observed = append(report.Observed, status)
		if status.Service == "" || status.Replica <= 0 {
			continue
		}
		key := desiredKey{service: status.Service, replica: status.Replica}
		if _, desired := desiredHosts[key]; desired {
			observedByDesired[key] = append(observedByDesired[key], status)
		}
	}
	sortContainerStatuses(report.Observed)

	for _, placement := range placements {
		key := desiredKey{service: placement.Service, replica: placement.Replica}
		desired := DesiredReplicaStatus{
			Service:        placement.Service,
			Replica:        placement.Replica,
			Host:           placement.Host.Name,
			DesiredRelease: input.CurrentRelease,
			State:          "missing",
			Observed:       append([]ContainerStatus(nil), observedByDesired[key]...),
		}
		sortContainerStatuses(desired.Observed)
		if input.CurrentRelease != "" {
			desired.DesiredName = ContainerName(input.Config.Project, input.EnvName, placement.Service, placement.Replica, input.CurrentRelease)
		} else {
			desired.Drift = append(desired.Drift, "no current release")
		}

		exact := false
		wrongRelease := false
		wrongHost := false
		for _, observed := range desired.Observed {
			if input.CurrentRelease != "" && observed.Release == input.CurrentRelease && observed.Host == placement.Host.Name && observed.Name == desired.DesiredName {
				exact = true
			}
			if input.CurrentRelease != "" && observed.Release != "" && observed.Release != input.CurrentRelease {
				wrongRelease = true
				desired.Drift = append(desired.Drift, "wrong release "+observed.Release+" on "+observed.Host)
			}
			if input.CurrentRelease != "" && observed.Release == input.CurrentRelease && observed.Host != placement.Host.Name {
				wrongHost = true
				desired.Drift = append(desired.Drift, "wrong host "+observed.Host)
			}
		}
		if exact && len(desired.Drift) == 0 {
			desired.State = "ok"
		} else if exact {
			desired.State = "drift"
		} else if wrongRelease {
			desired.State = "wrong_release"
		} else if wrongHost {
			desired.State = "wrong_host"
		}
		if !exact {
			report.Summary.Missing++
		}
		if wrongRelease {
			report.Summary.WrongRelease++
		}
		if wrongHost {
			report.Summary.WrongHost++
		}
		report.Desired = append(report.Desired, desired)
	}

	for _, observed := range report.Observed {
		if isConfiguredAccessoryStatus(input.Config, observed) {
			continue
		}
		if _, ok := exactDesired[observed.Host+"\x00"+observed.Name]; ok {
			continue
		}
		report.ExtraObserved = append(report.ExtraObserved, observed)
	}
	sortContainerStatuses(report.ExtraObserved)
	report.Summary.Desired = len(report.Desired)
	report.Summary.Observed = len(report.Observed)
	report.Summary.Extra = len(report.ExtraObserved)
	report.Summary.Drift = report.Summary.Missing > 0 || report.Summary.WrongRelease > 0 || report.Summary.WrongHost > 0 || report.Summary.Extra > 0
	return report, nil
}

func isConfiguredAccessoryStatus(cfg *config.Config, observed ContainerStatus) bool {
	if observed.Accessory == "" {
		return false
	}
	for name := range cfg.Accessories {
		if observed.Accessory == safeLabelValue(name) {
			return true
		}
	}
	return false
}

func containerStatus(item ObservedContainer) ContainerStatus {
	labels := item.Container.Labels
	replica, _ := strconv.Atoi(labels[docker.LabelReplica])
	kind := "unknown"
	if labels[docker.LabelService] != "" {
		kind = "service"
	}
	if labels[docker.LabelAccessory] != "" {
		kind = "accessory"
	}
	return ContainerStatus{
		Host:      item.Host.Name,
		Name:      item.Container.Names,
		ID:        item.Container.ID,
		Image:     item.Container.Image,
		Status:    item.Container.Status,
		Kind:      kind,
		Service:   labels[docker.LabelService],
		Accessory: labels[docker.LabelAccessory],
		Replica:   replica,
		Release:   labels[docker.LabelRelease],
		Labels:    labels,
	}
}

func sortContainerStatuses(statuses []ContainerStatus) {
	sort.Slice(statuses, func(i, j int) bool {
		if statuses[i].Host != statuses[j].Host {
			return statuses[i].Host < statuses[j].Host
		}
		return statuses[i].Name < statuses[j].Name
	})
}
