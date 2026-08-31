package config

import (
	"log/slog"
	"os"

	"gopkg.in/yaml.v3"
)

const ConfigurationFile = "/etc/kvm/server.yaml"

func Read() (*Config, error) {
	data, err := os.ReadFile(ConfigurationFile)
	if err != nil {
		slog.Error("failed to read config", slog.Any("err", err))
		return nil, err
	}

	var conf Config

	if err := yaml.Unmarshal(data, &conf); err != nil {
		// Was log.Fatalf, which killed the process and left the `return`
		// below unreachable. Read is a library call reached from HTTP
		// handlers (api/vm/tls.go) that already inspect its error, so a
		// malformed server.yaml must fail that request, not the daemon.
		slog.Error("failed to unmarshal config", slog.Any("err", err))
		return nil, err
	}

	slog.Debug("read config successfully", slog.String("path", ConfigurationFile))
	return &conf, nil
}

func Write(conf *Config) error {
	data, err := yaml.Marshal(&conf)
	if err != nil {
		slog.Error("failed to marshal config", slog.Any("err", err))
		return err
	}

	// 0600, matching create() and persistConfig() in config.go: server.yaml
	// holds the JWT secret and the IPMI credentials, and only this process
	// (running as root) reads it.
	err = os.WriteFile(ConfigurationFile, data, 0o600)
	if err != nil {
		slog.Error("failed to write config", slog.Any("err", err))
		return err
	}

	slog.Debug("write config successfully", slog.String("path", ConfigurationFile))
	return nil
}
