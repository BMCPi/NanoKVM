package config

import (
	"os"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const ConfigurationFile = "/etc/kvm/server.yaml"

func Read() (*Config, error) {
	data, err := os.ReadFile(ConfigurationFile)
	if err != nil {
		log.Errorf("failed to read config: %v", err)
		return nil, err
	}

	var conf Config

	if err := yaml.Unmarshal(data, &conf); err != nil {
		// Was log.Fatalf, which killed the process and left the `return`
		// below unreachable. Read is a library call reached from HTTP
		// handlers (api/vm/tls.go) that already inspect its error, so a
		// malformed server.yaml must fail that request, not the daemon.
		log.Errorf("failed to unmarshal config: %v", err)
		return nil, err
	}

	log.Debugf("read %s successfully", ConfigurationFile)
	return &conf, nil
}

func Write(conf *Config) error {
	data, err := yaml.Marshal(&conf)
	if err != nil {
		log.Errorf("failed to marshal config: %v", err)
		return err
	}

	// 0600, matching create() and persistConfig() in config.go: server.yaml
	// holds the JWT secret and the IPMI credentials, and only this process
	// (running as root) reads it.
	err = os.WriteFile(ConfigurationFile, data, 0o600)
	if err != nil {
		log.Errorf("failed to write config: %v", err)
		return err
	}

	log.Debugf("write to %s successfully", ConfigurationFile)
	return nil
}
