package config

import (
	"fmt"
	"strings"
)

// Keyboard macros: named sequences of keystrokes the operator can fire at the
// host from the toolbar, modelled on JetKVM's (config.go KeyboardMacro there).
// A macro lives in the config file rather than the browser because the point is
// that it is the BMC's macro — any client, and any session, sees the same set.
//
// The limits below are JetKVM's, with one deliberate difference: a step carries
// at most MaxKeysPerStep = 6 keys, not 10, because the boot keyboard's report
// has exactly six key slots (see pkg/hid). Accepting ten and sending six would
// drop keys silently, so it is a validation error instead.
const (
	MaxMacros        = 25
	MaxStepsPerMacro = 10
	MaxKeysPerStep   = 6
	MinStepDelayMS   = 50
	MaxStepDelayMS   = 2000
	MaxMacroNameLen  = 64
)

// MacroStep is one keystroke in a macro: a set of keys pressed together with a
// set of modifiers held over them, then released, then a pause.
//
// Keys and Modifiers are names, not usage codes — "KeyA", "Enter", "Control" —
// so a macro stays readable in the config file and survives a change of keycode
// table. pkg/hid owns the name-to-code mapping.
type MacroStep struct {
	Keys      []string `yaml:"keys" json:"keys"`
	Modifiers []string `yaml:"modifiers" json:"modifiers"`
	// Delay in milliseconds after the step's keys are released. Clamped into
	// [MinStepDelayMS, MaxStepDelayMS] rather than rejected: a macro that
	// arrives with a 0 delay is still a usable macro.
	Delay int `yaml:"delay" json:"delay"`
}

// KeyboardMacro is a named, ordered sequence of steps.
type KeyboardMacro struct {
	ID        string      `yaml:"id" json:"id"`
	Name      string      `yaml:"name" json:"name"`
	Steps     []MacroStep `yaml:"steps" json:"steps"`
	SortOrder int         `yaml:"sortOrder" json:"sortOrder"`
}

// Validate normalises the step and reports what cannot be fixed.
func (s *MacroStep) Validate() error {
	if len(s.Keys) == 0 && len(s.Modifiers) == 0 {
		return fmt.Errorf("a step must press at least one key or modifier")
	}
	if len(s.Keys) > MaxKeysPerStep {
		return fmt.Errorf("too many keys in one step (%d, max %d — the keyboard report has %d slots)",
			len(s.Keys), MaxKeysPerStep, MaxKeysPerStep)
	}

	if s.Delay < MinStepDelayMS {
		s.Delay = MinStepDelayMS
	} else if s.Delay > MaxStepDelayMS {
		s.Delay = MaxStepDelayMS
	}
	return nil
}

// Validate normalises the macro and reports what cannot be fixed. Key and
// modifier *names* are not checked here — pkg/config has no keycode table and
// should not grow one; the API layer resolves them and reports unknown names.
func (m *KeyboardMacro) Validate() error {
	m.Name = strings.TrimSpace(m.Name)
	if m.Name == "" {
		return fmt.Errorf("a macro needs a name")
	}
	if len(m.Name) > MaxMacroNameLen {
		return fmt.Errorf("macro name is too long (%d chars, max %d)", len(m.Name), MaxMacroNameLen)
	}
	if len(m.Steps) == 0 {
		return fmt.Errorf("macro %q has no steps", m.Name)
	}
	if len(m.Steps) > MaxStepsPerMacro {
		return fmt.Errorf("macro %q has too many steps (%d, max %d)", m.Name, len(m.Steps), MaxStepsPerMacro)
	}

	for i := range m.Steps {
		if err := m.Steps[i].Validate(); err != nil {
			return fmt.Errorf("macro %q step %d: %w", m.Name, i+1, err)
		}
	}
	return nil
}
