package hid

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/ctxutil"
)

// Macro execution. A macro is a list of steps; each step asserts a report
// (modifiers held over up to six keys), holds it briefly so the host's key
// repeat and debounce logic sees a real keypress, releases everything, then
// waits. Steps are absolute reports rather than press/release pairs so a macro
// cannot inherit state from whatever the operator happened to be holding.

// stepHold is how long a step's keys stay down. Long enough for a host to
// register a deliberate keypress (a BIOS polling at 60 Hz needs more than a
// couple of milliseconds), short enough that a ten-step macro still feels
// immediate.
const stepHold = 40 * time.Millisecond

// Step is one resolved macro step: the report to assert and the pause after it.
type Step struct {
	Modifier byte
	Keys     []byte
	Delay    time.Duration
}

// ResolveStep turns the names a stored macro carries into a report. Unknown
// names are an error rather than a silent no-op: a macro that half-works is
// worse to debug than one that refuses to run.
func ResolveStep(keys, modifiers []string, delayMS int) (Step, error) {
	step := Step{Delay: time.Duration(delayMS) * time.Millisecond}

	for _, name := range modifiers {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		mask, ok := ModifierNames[name]
		if !ok {
			// A sided key name ("ShiftLeft") is also a valid modifier here.
			code, isKey := KeyCodes[name]
			if isKey && IsModifier(code) {
				mask = ModifierMaskOf(code)
			} else {
				return Step{}, fmt.Errorf("unknown modifier %q", name)
			}
		}
		step.Modifier |= mask
	}

	for _, name := range keys {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		code, ok := KeyCodes[name]
		if !ok {
			return Step{}, fmt.Errorf("unknown key %q", name)
		}
		// A modifier listed among the keys belongs in the modifier byte; that
		// is where the host looks for it, and it would otherwise eat a slot.
		if IsModifier(code) {
			step.Modifier |= ModifierMaskOf(code)
			continue
		}
		if len(step.Keys) >= keyBufferSize {
			return Step{}, fmt.Errorf("more than %d keys in one step", keyBufferSize)
		}
		step.Keys = append(step.Keys, code)
	}

	if step.Modifier == 0 && len(step.Keys) == 0 {
		return Step{}, fmt.Errorf("step presses nothing")
	}
	return step, nil
}

// RunMacro plays the steps in order. It releases everything when it finishes,
// including on error or cancellation — a macro abandoned halfway must not leave
// a modifier held on the host.
//
// Errors from individual reports stop the macro: if the host is not accepting
// input there is no point typing the remaining steps at it.
func (c *Controller) RunMacro(ctx context.Context, steps []Step) error {
	defer func() {
		if err := c.ReleaseAll(); err != nil {
			c.log.DebugContext(ctx, "hid: releasing keys after macro failed", slog.Any("err", err))
		}
	}()

	for i, step := range steps {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := c.KeyReport(step.Modifier, step.Keys); err != nil {
			return fmt.Errorf("macro step %d: %w", i+1, err)
		}
		if err := ctxutil.SleepCtx(ctx, stepHold); err != nil {
			return err
		}

		if err := c.KeyReport(0, nil); err != nil {
			return fmt.Errorf("macro step %d release: %w", i+1, err)
		}
		if err := ctxutil.SleepCtx(ctx, step.Delay); err != nil {
			return err
		}
	}
	return nil
}
