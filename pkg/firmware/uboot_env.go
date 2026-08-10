package firmware

// uboot_env.go provides accessors for the U-Boot environment.
//
// The environment lives in a region of the I2C EEPROM (the host's
// CONFIG_ENV_IS_IN_EEPROM partition), not in files inside the boot image:
// U-Boot reads exactly these bytes at boot and rewrites them on saveenv, so
// there is a single store rather than the old machine/persistent/once files.
// See firmware.go for the store wiring and ubootenv.Store for the layout.

import (
	"github.com/pi-bmc/nanokvm-app/pkg/ubootenv"
)

// LoadUbootEnv returns the U-Boot environment from the EEPROM. Fresh read
// (subject to the store's short cache).
func (c *Controller) LoadUbootEnv() (*ubootenv.Env, error) {
	return c.env.Load()
}

// GetUbootEnvVar returns a single environment variable.
func (c *Controller) GetUbootEnvVar(key string) (string, bool, error) {
	env, err := c.LoadUbootEnv()
	if err != nil {
		return "", false, err
	}
	v, ok := env.Get(key)
	return v, ok, nil
}

// SetUbootEnvVar writes a variable to the environment, preserving the others.
// Pass del=true to delete the key instead.
func (c *Controller) SetUbootEnvVar(key, value string, del bool) error {
	return c.env.Update(func(env *ubootenv.Env) {
		if del {
			env.Delete(key)
			return
		}
		env.Set(key, value)
	})
}

// SetUbootEnvVars applies multiple variable updates atomically (a single
// read-modify-write of the environment region).
func (c *Controller) SetUbootEnvVars(updates map[string]string, deletes []string) error {
	return c.env.Update(func(env *ubootenv.Env) {
		for k, v := range updates {
			env.Set(k, v)
		}
		for _, k := range deletes {
			env.Delete(k)
		}
	})
}
