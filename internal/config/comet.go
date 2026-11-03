package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	cometconfig "github.com/cometbft/cometbft/config"
	"github.com/spf13/viper"
)

// LoadCometConfig loads CometBFT settings from <home>/config/config.toml.
// A default file is created only when it is missing; existing node settings are
// preserved. Non-empty command-line overrides take precedence over the file.
func LoadCometConfig(home, logLevel, timeoutCommit string) (*cometconfig.Config, error) {
	if strings.TrimSpace(home) == "" {
		return nil, fmt.Errorf("node home directory is required")
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, fmt.Errorf("create node home: %w", err)
	}
	cometconfig.EnsureRoot(home)

	configPath := filepath.Join(home, cometconfig.DefaultConfigDir, cometconfig.DefaultConfigFileName)
	reader := viper.New()
	reader.SetConfigFile(configPath)
	reader.SetConfigType("toml")
	if err := reader.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read CometBFT config %s: %w", configPath, err)
	}

	loaded := cometconfig.DefaultConfig()
	if err := reader.Unmarshal(loaded); err != nil {
		return nil, fmt.Errorf("decode CometBFT config %s: %w", configPath, err)
	}
	loaded.SetRoot(home)

	if strings.TrimSpace(logLevel) != "" {
		loaded.LogLevel = strings.TrimSpace(logLevel)
	}
	if strings.TrimSpace(timeoutCommit) != "" {
		duration, err := time.ParseDuration(strings.TrimSpace(timeoutCommit))
		if err != nil {
			return nil, fmt.Errorf("invalid timeout_commit %q: %w", timeoutCommit, err)
		}
		if duration < 0 {
			return nil, fmt.Errorf("timeout_commit must not be negative")
		}
		loaded.Consensus.TimeoutCommit = duration
	}
	if err := loaded.ValidateBasic(); err != nil {
		return nil, fmt.Errorf("invalid CometBFT config: %w", err)
	}
	return loaded, nil
}
