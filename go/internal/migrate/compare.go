package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// Difference is one thing the database would publish that does not match what
// is published now.
type Difference struct {
	Object string // the OSS key
	What   string // what differs, in a sentence
}

func (d Difference) String() string { return d.Object + ": " + d.What }

// Comparison is the result of one shadow run.
type Comparison struct {
	Checked     int
	Differences []Difference
}

// Clean reports whether the database would publish exactly what is published
// now. It is the only condition under which switching over is uneventful.
func (c Comparison) Clean() bool { return len(c.Differences) == 0 }

func (c Comparison) String() string {
	if c.Clean() {
		return fmt.Sprintf("%d object(s) checked, all identical", c.Checked)
	}
	lines := make([]string, 0, len(c.Differences)+1)
	lines = append(lines, fmt.Sprintf("%d object(s) checked, %d differ:", c.Checked, len(c.Differences)))
	for _, d := range c.Differences {
		lines = append(lines, "  "+d.String())
	}
	return strings.Join(lines, "\n")
}

// Comparer answers the question the cutover depends on: if the database were
// in charge, would the machines see anything different?
//
// It reads and compares; it never writes. Running it against production while
// the old console is live is the point -- an export that would change bytes on
// a desktop is something to find out about now rather than during the switch.
//
// Credentials are compared by presence, not by content. The archive contains a
// token this side cannot reproduce (the gateway returns a token once), so
// "there is one" and "there is not one" are the two answers that matter; a
// difference in the token itself would be reported on every single employee
// and drown everything else.
type Comparer struct {
	Store   repo.Store
	Objects ObjectSource
}

// Run compares every object the export would write.
func (c *Comparer) Run(ctx context.Context) (Comparison, error) {
	var result Comparison
	if err := c.comparePolicy(ctx, &result); err != nil {
		return result, err
	}
	if err := c.compareBindings(ctx, &result); err != nil {
		return result, err
	}
	if err := c.compareCredentials(ctx, &result); err != nil {
		return result, err
	}
	return result, nil
}

func (c *Comparer) comparePolicy(ctx context.Context, result *Comparison) error {
	key := ossclient.PolicyKey()
	result.Checked++

	current, err := c.Store.Policies().Current(ctx)
	dbHas := err == nil
	if err != nil && !errors.Is(err, repo.ErrNotFound) {
		return err
	}

	live, _, err := c.Objects.Get(key)
	ossHas := err == nil
	if err != nil && !errors.Is(err, ossclient.ErrNotFound) {
		return fmt.Errorf("read %s: %w", key, err)
	}

	switch {
	case !dbHas && !ossHas:
		return nil
	case !dbHas:
		result.Differences = append(result.Differences, Difference{key,
			"published today, but the database has no policy: the fleet would be left with nothing"})
	case !ossHas:
		result.Differences = append(result.Differences, Difference{key,
			"the database has a policy that has never been published"})
	case !sameJSON(current.Content, live):
		result.Differences = append(result.Differences, Difference{key, describeJSONDiff(live, current.Content)})
	}
	return nil
}

func (c *Comparer) compareBindings(ctx context.Context, result *Comparison) error {
	keys, err := c.Objects.List(ossclient.BindingPrefix)
	if err != nil {
		return fmt.Errorf("list bindings: %w", err)
	}
	seen := map[string]bool{}

	for _, key := range keys {
		machine := ossclient.MachineFromBindingKey(key)
		if machine == "" {
			continue
		}
		seen[strings.ToLower(machine)] = true
		result.Checked++

		data, _, err := c.Objects.Get(key)
		if errors.Is(err, ossclient.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", key, err)
		}
		var live model.Binding
		if err := json.Unmarshal(data, &live); err != nil {
			result.Differences = append(result.Differences, Difference{key, "the published object is not readable JSON"})
			continue
		}

		device, err := c.Store.Devices().ByHostname(ctx, machine)
		if errors.Is(err, repo.ErrNotFound) {
			result.Differences = append(result.Differences, Difference{key,
				"this machine is not in the database, so its binding would disappear"})
			continue
		}
		if err != nil {
			return err
		}
		binding, err := c.Store.Bindings().Open(ctx, device.ID)
		if errors.Is(err, repo.ErrNotFound) {
			result.Differences = append(result.Differences, Difference{key, fmt.Sprintf(
				"published as bound to %q, but the database has nobody on this machine", live.User)})
			continue
		}
		if err != nil {
			return err
		}
		employee, err := c.Store.Employees().ByID(ctx, binding.EmployeeID)
		if err != nil {
			return err
		}
		if !strings.EqualFold(employee.WindowsUser, live.User) {
			result.Differences = append(result.Differences, Difference{key, fmt.Sprintf(
				"published as bound to %q, the database says %q", live.User, employee.WindowsUser)})
		}
	}

	// A machine bound in the database but not published is the other
	// direction: the switch would hand somebody a machine they do not have
	// today.
	open, err := c.Store.Bindings().ListOpen(ctx)
	if err != nil {
		return err
	}
	for _, binding := range open {
		device, err := c.Store.Devices().ByID(ctx, binding.DeviceID)
		if err != nil {
			return err
		}
		if seen[strings.ToLower(device.Hostname)] {
			continue
		}
		result.Checked++
		result.Differences = append(result.Differences, Difference{
			ossclient.BindingKey(device.Hostname),
			"bound in the database but not published today"})
	}
	return nil
}

// compareCredentials checks presence only.
func (c *Comparer) compareCredentials(ctx context.Context, result *Comparison) error {
	employees, err := c.Store.Employees().List(ctx, repo.EmployeeFilter{IncludeOffboarded: true})
	if err != nil {
		return err
	}
	for _, employee := range employees {
		key := ossclient.UserKey(employee.WindowsUser, "credentials.zip")
		result.Checked++

		_, _, err := c.Objects.Get(key)
		published := err == nil
		if err != nil && !errors.Is(err, ossclient.ErrNotFound) {
			return fmt.Errorf("read %s: %w", key, err)
		}

		_, err = c.Store.Credentials().Live(ctx, employee.ID, repo.PurposeCodexGateway)
		stored := err == nil
		if err != nil && !errors.Is(err, repo.ErrNotFound) {
			return err
		}

		switch {
		case published && !stored && employee.Active():
			// The expected state during a migration: the employee is working
			// with a token the database does not hold, because a token cannot
			// be read back from the gateway. It is a difference worth naming,
			// not a fault -- but it does mean the export would withdraw their
			// credentials, so nobody should switch over until each of these
			// has been re-issued.
			result.Differences = append(result.Differences, Difference{key, fmt.Sprintf(
				"%s has published credentials and no stored token: switching over would withdraw them; re-issue first",
				employee.WindowsUser)})
		case !published && stored:
			result.Differences = append(result.Differences, Difference{key, fmt.Sprintf(
				"the database holds a token for %s that has never been delivered", employee.WindowsUser)})
		case published && !employee.Active():
			result.Differences = append(result.Differences, Difference{key, fmt.Sprintf(
				"%s is offboarded in the database but still has credentials published", employee.WindowsUser)})
		}
	}
	return nil
}

// describeJSONDiff names the fields that differ, so a report says what
// changed rather than that something did.
func describeJSONDiff(live, proposed []byte) string {
	var a, b map[string]any
	if json.Unmarshal(live, &a) != nil || json.Unmarshal(proposed, &b) != nil {
		return "the two objects differ and at least one is not a JSON object"
	}
	fields := map[string]bool{}
	for key := range a {
		fields[key] = true
	}
	for key := range b {
		fields[key] = true
	}
	var changed []string
	for key := range fields {
		if key == "updatedAt" {
			continue
		}
		left, _ := json.Marshal(a[key])
		right, _ := json.Marshal(b[key])
		if string(left) != string(right) {
			changed = append(changed, fmt.Sprintf("%s: %s -> %s", key, left, right))
		}
	}
	if len(changed) == 0 {
		return "the objects differ only in fields that are not compared"
	}
	sort.Strings(changed)
	return strings.Join(changed, "; ")
}
