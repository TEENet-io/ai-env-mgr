package admincore

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/TEENet-io/airlock/internal/model"
	"github.com/TEENet-io/airlock/internal/ossclient"
)

// BindMachine assigns a machine (identified by hostname, the only identity
// available to an agent running as SYSTEM) to an employee.
//
// The employee must already be on the roster: a binding to an unknown name
// would hand a machine's credentials to nobody, and typos would only be
// caught once an agent failed to find its own status improving. Binding is
// idempotent — rebinding a machine (reassignment, typo fix) simply
// overwrites the previous binding rather than erroring.
func (m *Manager) BindMachine(machine, user, note string) error {
	us, err := m.LoadUsers()
	if err != nil {
		return err
	}
	if us.Find(user) == nil {
		return fmt.Errorf("user %q not found in roster", user)
	}

	b := model.Binding{
		User:    user,
		BoundAt: time.Now().UTC().Format(time.RFC3339),
		Note:    note,
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("encode binding for %q: %w", machine, err)
	}
	if err := m.Store.Put(ossclient.BindingKey(machine), data); err != nil {
		return fmt.Errorf("bind machine %q to %q: %w", machine, user, err)
	}
	return nil
}

// UnbindMachine removes a machine's binding. The machine keeps reporting
// status (it doesn't know it was unbound until its next sync), but the
// admin view will show it as unbound rather than attributing it to its old
// employee.
func (m *Manager) UnbindMachine(machine string) error {
	if err := m.Store.Delete(ossclient.BindingKey(machine)); err != nil {
		return fmt.Errorf("unbind machine %q: %w", machine, err)
	}
	return nil
}

// LoadBinding reads a single machine's binding. A missing object means the
// machine has never been bound, which is a normal state (a new machine
// waiting for assignment) rather than an error.
func (m *Manager) LoadBinding(machine string) (model.Binding, bool, error) {
	data, _, err := m.Store.Get(ossclient.BindingKey(machine))
	if err != nil {
		return model.Binding{}, false, nil
	}
	var b model.Binding
	if err := json.Unmarshal(data, &b); err != nil {
		return model.Binding{}, false, fmt.Errorf("parse binding for %q: %w", machine, err)
	}
	return b, true, nil
}

// ListBindings returns every current binding, keyed by hostname.
func (m *Manager) ListBindings() (map[string]model.Binding, error) {
	keys, err := m.Store.List(ossclient.BindingPrefix)
	if err != nil {
		return nil, fmt.Errorf("list bindings: %w", err)
	}
	sort.Strings(keys)

	out := make(map[string]model.Binding, len(keys))
	for _, key := range keys {
		machine := ossclient.MachineFromBindingKey(key)
		data, _, err := m.Store.Get(key)
		if err != nil {
			return nil, fmt.Errorf("get binding %q: %w", key, err)
		}
		var b model.Binding
		if err := json.Unmarshal(data, &b); err != nil {
			return nil, fmt.Errorf("parse binding %q: %w", key, err)
		}
		out[machine] = b
	}
	return out, nil
}
