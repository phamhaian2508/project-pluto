package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	cometconfig "github.com/cometbft/cometbft/config"
)

func TestLoadCometConfigReadsFileAndAppliesOverrides(t *testing.T) {
	home := t.TempDir()
	cometconfig.EnsureRoot(home)
	path := filepath.Join(home, cometconfig.DefaultConfigDir, cometconfig.DefaultConfigFileName)
	cfg := cometconfig.DefaultConfig()
	cfg.LogLevel = "debug"
	cfg.Consensus.TimeoutCommit = 2 * time.Second
	cometconfig.WriteConfigFile(path, cfg)

	loaded, err := LoadCometConfig(home, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LogLevel != "debug" || loaded.Consensus.TimeoutCommit != 2*time.Second {
		t.Fatalf("config values not loaded: log_level=%q timeout_commit=%s", loaded.LogLevel, loaded.Consensus.TimeoutCommit)
	}

	loaded, err = LoadCometConfig(home, "error", "750ms")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LogLevel != "error" || loaded.Consensus.TimeoutCommit != 750*time.Millisecond {
		t.Fatalf("CLI overrides not applied: log_level=%q timeout_commit=%s", loaded.LogLevel, loaded.Consensus.TimeoutCommit)
	}
}

func TestLoadCometConfigRejectsInvalidTimeout(t *testing.T) {
	if _, err := LoadCometConfig(t.TempDir(), "", "later"); err == nil {
		t.Fatal("invalid timeout_commit was accepted")
	}
}

func TestLoadCometConfigDoesNotReplaceExistingConfig(t *testing.T) {
	home := t.TempDir()
	cometconfig.EnsureRoot(home)
	path := filepath.Join(home, cometconfig.DefaultConfigDir, cometconfig.DefaultConfigFileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCometConfig(home, "", ""); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("existing config.toml was modified")
	}
}
