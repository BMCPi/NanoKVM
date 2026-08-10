package efivars

// override.go maps Redfish-style boot source overrides onto the UEFI boot
// manager variables:
//
//   - Once       -> BootNext = first active Boot#### matching the target
//   - Continuous -> BootOrder reordered so matching entries come first
//   - Disabled   -> BootNext removed (BootOrder is left as-is; a continuous
//     override is a plain reorder and has no marker to undo)

import (
	"fmt"
)

// ErrNoMatchingEntry reports that no Boot#### entry matches the requested
// boot source target.
var ErrNoMatchingEntry = fmt.Errorf("efivars: no boot entry matches the requested target")

// matchingEntries returns the active Boot#### IDs classified as target,
// ordered by current BootOrder position (unlisted entries last, by ID).
func (m *Manager) matchingEntries(target BootTarget) ([]uint16, error) {
	entries, err := m.BootEntries()
	if err != nil {
		return nil, err
	}
	order, err := m.BootOrder()
	if err != nil {
		return nil, err
	}
	pos := make(map[uint16]int, len(order))
	for i, id := range order {
		pos[id] = i
	}
	rank := func(id uint16) int {
		if p, ok := pos[id]; ok {
			return p
		}
		return len(order) + int(id)
	}

	var ids []uint16
	for _, e := range entries {
		if e.Active && e.Target == target {
			ids = append(ids, e.ID)
		}
	}
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && rank(ids[j]) < rank(ids[j-1]); j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
	return ids, nil
}

// SetBootSourceOverride applies a Redfish boot source override. target is a
// BootTarget; once selects BootNext semantics, otherwise BootOrder is
// reordered with matching entries first.
func (m *Manager) SetBootSourceOverride(target BootTarget, once bool) error {
	ids, err := m.matchingEntries(target)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return fmt.Errorf("%w: %s", ErrNoMatchingEntry, target)
	}

	if once {
		return m.SetBootNext(ids[0])
	}

	order, err := m.BootOrder()
	if err != nil {
		return err
	}
	matched := make(map[uint16]bool, len(ids))
	for _, id := range ids {
		matched[id] = true
	}
	newOrder := append([]uint16(nil), ids...)
	for _, id := range order {
		if !matched[id] {
			newOrder = append(newOrder, id)
		}
	}
	return m.SetBootOrder(newOrder)
}

// ClearBootSourceOverride removes the one-shot override.
func (m *Manager) ClearBootSourceOverride() error {
	return m.ClearBootNext()
}

// BootSourceOverride reports the current override as (target, enabled):
// a pending BootNext maps to (its target, "Once"); otherwise ("", "Disabled").
// Continuous overrides are plain BootOrder reorders and are not detectable.
func (m *Manager) BootSourceOverride() (BootTarget, string, error) {
	next, err := m.BootNext()
	if err != nil || next == nil {
		return TargetUnknown, "Disabled", err
	}
	entries, err := m.BootEntries()
	if err != nil {
		return TargetUnknown, "Disabled", err
	}
	for _, e := range entries {
		if e.ID == *next {
			return e.Target, "Once", nil
		}
	}
	return TargetUnknown, "Once", nil
}
