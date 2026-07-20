package implslo

import (
	"context"
	"os"

	"github.com/SigNoz/signoz/pkg/errors"
	"github.com/SigNoz/signoz/pkg/types/slotypes"
	"gopkg.in/yaml.v3"
)

// EnvSLOConfigPath is the environment variable that points at the SLO-as-code
// YAML file.
const EnvSLOConfigPath = "SIGNOZ_SLO_CONFIG_PATH"

// ConfigProvider supplies the SLO definitions the engine evaluates.
//
// M3 ships a file-backed provider. Swapping to a sqlstore-backed, per-org
// provider later is a drop-in replacement behind this interface.
type ConfigProvider interface {
	Load(ctx context.Context) (*slotypes.Config, error)
}

type fileConfigProvider struct {
	path string
}

// NewFileConfigProvider reads SLO definitions from a YAML file at path. An empty
// path or a missing file yields an empty config (no SLOs) rather than an error,
// so the server boots cleanly when no SLOs are configured.
func NewFileConfigProvider(path string) ConfigProvider {
	return &fileConfigProvider{path: path}
}

// NewEnvFileConfigProvider reads the file path from EnvSLOConfigPath.
func NewEnvFileConfigProvider() ConfigProvider {
	return NewFileConfigProvider(os.Getenv(EnvSLOConfigPath))
}

func (f *fileConfigProvider) Load(_ context.Context) (*slotypes.Config, error) {
	if f.path == "" {
		return &slotypes.Config{}, nil
	}

	data, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &slotypes.Config{}, nil
		}
		return nil, errors.Wrapf(err, errors.TypeInternal, errors.CodeInternal, "reading SLO config %q", f.path)
	}

	var cfg slotypes.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, errors.Wrapf(err, errors.TypeInvalidInput, errors.CodeInvalidInput, "parsing SLO config %q", f.path)
	}

	cfg.Normalize()
	for _, def := range cfg.SLOs {
		if err := def.Validate(); err != nil {
			return nil, errors.Wrapf(err, errors.TypeInvalidInput, errors.CodeInvalidInput, "invalid SLO config %q", f.path)
		}
	}
	return &cfg, nil
}
