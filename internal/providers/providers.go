package providers

import (
	"fmt"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/hetzner"
	"github.com/watzon/ship/internal/provider"
)

func ForEnvironment(env config.Environment, dryRun bool) (provider.Provider, error) {
	switch env.Provider.Name() {
	case config.ProviderHetzner:
		return hetzner.NewFromEnv(dryRun), nil
	default:
		return nil, fmt.Errorf("unsupported provider %q", env.Provider.Name())
	}
}
