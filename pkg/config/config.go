package config

import (
	"bytes"
	"errors"
	"log"
	"os"
	"sync"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

var (
	instance Config
	once     sync.Once
)

// configFilePath is the file this package writes when it has to persist —
// a generated JWT secret, a settings change, the one-time discovery
// migration. It is a var rather than the ConfigurationFile constant used
// directly so a test can redirect those writes away from the real /etc/kvm.
// Reads still go through viper's search path in readByFile.
var configFilePath = ConfigurationFile

func GetInstance() *Config {
	once.Do(initialize)

	return &instance
}

// initialize loads the process-wide configuration. It runs exactly once, under
// the sync.Once in GetInstance, before any subsystem starts.
//
// The log.Fatalf calls below are deliberate and cannot be turned into returned
// errors: GetInstance's signature (*Config, no error) is what the whole tree
// calls, and every one of these failures means the process has no configuration
// at all — not even the compiled-in defaults could be read or parsed. There is
// no caller that could recover, and continuing would hand every subsystem a
// zero-valued Config: authentication "" (not "enable"), no ports, no cert
// paths. Failing to boot is the correct and safest outcome.
func initialize() {
	if err := readByFile(); err != nil {
		if errors.As(err, &viper.ConfigFileNotFoundError{}) {
			create()
		}

		if err = readByDefault(); err != nil {
			//nolint:revive // deep-exit: the compiled-in defaults are unreadable, so the process has no configuration at all and no caller of GetInstance can recover
			log.Fatalf("Failed to read default configuration!")
		}

		log.Println("using default configuration")
	}

	if err := validate(); err != nil {
		//nolint:revive // deep-exit: validate() already rewrote and re-read the file; a failure here leaves no usable configuration for any caller of GetInstance
		log.Fatalf("Failed to validate configuration!")
	}

	if err := viper.Unmarshal(&instance); err != nil {
		//nolint:revive // deep-exit: configuration is a process precondition; an unparseable config would leave every subsystem with a zero-valued Config
		log.Fatalf("Failed to parse configuration: %s", err)
	}

	if err := checkDefaultValue(); err != nil {
		//nolint:revive // deep-exit: a rejected config (e.g. an invalid power.reset) leaves no usable configuration for any caller of GetInstance
		log.Fatalf("Failed to apply configuration defaults: %s", err)
	}

	if instance.Authentication == "disable" {
		log.Println("NOTICE: Authentication is disabled! Please ensure your service is secure!")
	}

	log.Println("config loaded successfully")
}

func readByFile() error {
	viper.SetConfigName("server")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("/etc/kvm/")

	return viper.ReadInConfig()
}

func readByDefault() error {
	data, err := yaml.Marshal(defaultConfig)
	if err != nil {
		log.Printf("failed to marshal default config: %s", err)
		return err
	}

	return viper.ReadConfig(bytes.NewBuffer(data))
}

// Create configuration file.
func create() {
	var (
		file *os.File
		data []byte
		err  error
	)

	_ = os.MkdirAll("/etc/kvm", 0o755)

	file, err = os.OpenFile(configFilePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		log.Printf("open config failed: %s", err)
		return
	}
	defer func() {
		_ = file.Close()
	}()

	if data, err = yaml.Marshal(defaultConfig); err != nil {
		log.Printf("failed to marshal default config: %s", err)
		return
	}

	if _, err = file.Write(data); err != nil {
		log.Printf("failed to save config: %s", err)
		return
	}

	if err = file.Sync(); err != nil {
		log.Printf("failed to sync config: %s", err)
		return
	}

	log.Printf("create file %s with default configuration", configFilePath)
}

// Validate the configuration. This is to ensure compatibility with earlier versions.
func validate() error {
	if viper.GetInt("port.http") > 0 && viper.GetInt("port.https") > 0 {
		return nil
	}

	_ = os.Remove(configFilePath)
	log.Println("delete empty configuration file")

	create()

	return readByDefault()
}

// Save persists the in-memory config to /etc/kvm/server.yaml. Used by
// settings handlers (e.g. /api/autoupdate/settings) so user-driven changes
// survive restarts.
func Save() {
	persistConfig()
}

// persistConfig writes the current in-memory config back to disk. It saves
// generated values (e.g. the JWT secret key) so they survive restarts, and
// it is what makes the one-time discovery migration stick — see
// migrateDiscovery. It marshals the whole struct, so nothing else in the
// config is lost by a rewrite.
func persistConfig() {
	data, err := yaml.Marshal(&instance)
	if err != nil {
		log.Printf("failed to marshal config for persist: %s", err)
		return
	}

	_ = os.MkdirAll("/etc/kvm", 0o755)

	if err := os.WriteFile(configFilePath, data, 0o600); err != nil {
		log.Printf("failed to persist config: %s", err)
		return
	}

	log.Printf("persisted configuration to %s", configFilePath)
}
