// Package migrate moves the console's state out of OSS objects and into the
// database, and then checks that nothing changed.
//
// It is written to be run repeatedly and to be run while the old console is
// still live. Importing is idempotent -- it converges the database on what the
// objects say -- and the comparison is read-only, so the sequence is: import,
// compare, fix whatever the comparison complains about, import again. Only
// when a comparison comes back clean is there any reason to talk about
// switching over.
//
// What it deliberately does not do: invent credentials. Tokens live on the
// gateway and their plaintext cannot be read back, so an imported employee has
// a gateway grant recorded from the alias the gateway holds, and no credential
// row. Re-issuing gives them one; until then the objects already on their
// machine keep working, which is exactly the property that lets this be done
// without an outage.
package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// ObjectSource is the part of OSS the import reads.
type ObjectSource interface {
	Get(key string) ([]byte, string, error)
	List(prefix string) ([]string, error)
}

// Importer fills the database from the objects the old console wrote.
type Importer struct {
	Store   repo.Store
	Objects ObjectSource
	// Gateway is read for each employee's limits, models and existing token.
	// Nil is allowed and reported: the result is a database that cannot
	// provision or revoke anybody, which is fine for a dry run and for
	// nothing else.
	Gateway Gateway
	// Actor is recorded on the audit events the import writes, so the trail
	// says how these rows came to exist.
	Actor string
}

// Report is what one import did, for the person watching it.
type Report struct {
	Employees   int
	Quotas      int
	Devices     int
	Bindings    int
	Grants      int
	AuditLines  int
	Settings    int
	PolicyFound bool
	// Warnings are things worth a human's attention that did not stop the
	// import: a binding naming somebody who is not on the roster, a status
	// object for a machine nobody has ever bound.
	Warnings []string
}

func (r Report) String() string {
	return fmt.Sprintf("%d employees, %d quotas, %d machines, %d bindings, %d grants, %d audit lines, %d settings, policy=%v, %d warning(s)",
		r.Employees, r.Quotas, r.Devices, r.Bindings, r.Grants, r.AuditLines, r.Settings, r.PolicyFound, len(r.Warnings))
}

// Run imports everything, in one transaction.
//
// All of it or none of it: a half-imported database is worse than an empty
// one, because it looks ready.
func (im *Importer) Run(ctx context.Context) (Report, error) {
	var report Report
	err := im.Store.InTx(ctx, func(tx repo.Store) error {
		var err error
		report, err = im.run(ctx, tx)
		return err
	})
	return report, err
}

func (im *Importer) run(ctx context.Context, tx repo.Store) (Report, error) {
	var report Report

	roster, err := im.roster()
	if err != nil {
		return report, err
	}
	byUser := map[string]repo.Employee{}
	for _, entry := range roster.Users {
		employee, created, err := im.upsertEmployee(ctx, tx, entry)
		if err != nil {
			return report, err
		}
		byUser[employee.WindowsUser] = employee
		if created {
			report.Employees++
		}
		if err := tx.LegacyIDs().Record(ctx, repo.LegacyEmployee, entry.WindowsUser, employee.ID); err != nil {
			return report, err
		}
	}

	if err := im.importGateway(ctx, tx, byUser, &report); err != nil {
		return report, err
	}
	if err := im.importPolicy(ctx, tx, &report); err != nil {
		return report, err
	}
	if err := im.importSettings(ctx, tx, &report); err != nil {
		return report, err
	}
	if err := im.importBindings(ctx, tx, byUser, &report); err != nil {
		return report, err
	}
	if err := im.importHistory(ctx, tx, byUser, &report); err != nil {
		return report, err
	}
	return report, nil
}

// roster reads admin/users.json.
//
// A missing roster is an empty console, which is a legitimate state for a
// fresh deployment. An unreadable one is not: importing zero employees over a
// roster that exists would look like a successful migration of nothing.
func (im *Importer) roster() (model.Users, error) {
	data, _, err := im.Objects.Get(ossclient.AdminKey("users.json"))
	if errors.Is(err, ossclient.ErrNotFound) {
		return model.Users{}, nil
	}
	if err != nil {
		return model.Users{}, fmt.Errorf("read the roster: %w", err)
	}
	var users model.Users
	if err := json.Unmarshal(data, &users); err != nil {
		return model.Users{}, fmt.Errorf("the roster is not readable JSON: %w", err)
	}
	return users, nil
}

func (im *Importer) upsertEmployee(ctx context.Context, tx repo.Store, entry model.UserEntry) (repo.Employee, bool, error) {
	user := repo.NormalizeWindowsUser(entry.WindowsUser)
	existing, err := tx.Employees().ByWindowsUser(ctx, user)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		created, err := tx.Employees().Create(ctx, repo.NewEmployee{
			WindowsUser: user, Name: entry.Name, Department: entry.Department,
			CodexAccount: entry.CodexAccount,
		})
		if err != nil {
			return repo.Employee{}, false, err
		}
		if !entry.Enabled {
			created, err = tx.Employees().Offboard(ctx, created.ID, created.Version)
			if err != nil {
				return repo.Employee{}, false, err
			}
		}
		return created, true, nil
	case err != nil:
		return repo.Employee{}, false, err
	}

	// Already imported: bring the labels up to date, and nothing else. The
	// epoch is not touched, because raising it would invalidate the token the
	// employee is using right now -- during a migration nobody has asked for.
	updated, err := tx.Employees().UpdateProfile(ctx, existing.ID, existing.Version, repo.Profile{
		Name: entry.Name, Department: entry.Department,
		CodexAccount: entry.CodexAccount,
		ExternalID:   existing.ExternalID,
	})
	if err != nil {
		return repo.Employee{}, false, err
	}
	switch {
	case entry.Enabled && !updated.Active():
		updated, err = tx.Employees().Reopen(ctx, updated.ID, updated.Version)
	case !entry.Enabled && updated.Active():
		updated, err = tx.Employees().Offboard(ctx, updated.ID, updated.Version)
	}
	if err != nil {
		return repo.Employee{}, false, err
	}
	return updated, false, nil
}

// importPolicy copies the live policy in as a version, unless it is already
// there byte for byte.
func (im *Importer) importPolicy(ctx context.Context, tx repo.Store, report *Report) error {
	data, _, err := im.Objects.Get(ossclient.PolicyKey())
	if errors.Is(err, ossclient.ErrNotFound) {
		report.Warnings = append(report.Warnings,
			"no policy object: the fleet has never been given one")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the policy: %w", err)
	}
	if !json.Valid(data) {
		return fmt.Errorf("the policy object is not readable JSON")
	}
	report.PolicyFound = true

	current, err := tx.Policies().Current(ctx)
	switch {
	case errors.Is(err, repo.ErrNotFound):
	case err != nil:
		return err
	default:
		if sameJSON(current.Content, data) {
			// Importing again must not publish a second identical version and
			// make the history look like somebody changed something.
			return nil
		}
	}
	_, err = tx.Policies().Publish(ctx, data, "imported from OSS", im.Actor)
	return err
}

// importBindings reads _bindings/<machine> and registers the machines.
func (im *Importer) importBindings(ctx context.Context, tx repo.Store, byUser map[string]repo.Employee, report *Report) error {
	keys, err := im.Objects.List(ossclient.BindingPrefix)
	if err != nil {
		return fmt.Errorf("list bindings: %w", err)
	}
	sort.Strings(keys)

	for _, key := range keys {
		machine := ossclient.MachineFromBindingKey(key)
		if machine == "" {
			continue
		}
		data, _, err := im.Objects.Get(key)
		if errors.Is(err, ossclient.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read binding %s: %w", key, err)
		}
		var binding model.Binding
		if err := json.Unmarshal(data, &binding); err != nil {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("binding for %s is not readable JSON and was skipped", machine))
			continue
		}

		device, err := tx.Devices().EnsureByHostname(ctx, machine)
		if err != nil {
			return err
		}
		report.Devices++
		if err := tx.LegacyIDs().Record(ctx, repo.LegacyDevice, machine, device.ID); err != nil {
			return err
		}

		employee, ok := byUser[repo.NormalizeWindowsUser(binding.User)]
		if !ok {
			// A machine bound to somebody who is not on the roster. It is
			// exactly the kind of thing this import exists to surface, and
			// inventing an employee row for them would bury it.
			report.Warnings = append(report.Warnings, fmt.Sprintf(
				"%s is bound to %q, who is not on the roster; the machine was imported without a binding",
				machine, binding.User))
			continue
		}

		open, err := tx.Bindings().Open(ctx, device.ID)
		switch {
		case err == nil:
			if open.EmployeeID == employee.ID {
				continue // already imported
			}
			if _, err := tx.Bindings().Unbind(ctx, device.ID, im.Actor); err != nil {
				return err
			}
		case !errors.Is(err, repo.ErrNotFound):
			return err
		}
		if _, err := tx.Bindings().Bind(ctx, device.ID, employee.ID, binding.Note, im.Actor); err != nil {
			return err
		}
		report.Bindings++
	}
	return nil
}

// sameJSON compares two JSON documents by value, so that a re-serialised
// object with different key order or spacing is not treated as a change.
func sameJSON(a, b []byte) bool {
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &y); err != nil {
		return false
	}
	ax, err := json.Marshal(normalise(x))
	if err != nil {
		return false
	}
	by, err := json.Marshal(normalise(y))
	if err != nil {
		return false
	}
	return string(ax) == string(by)
}

// normalise drops fields that are expected to differ between two renderings of
// the same decision.
func normalise(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(m))
	for key, value := range m {
		// The timestamp is written at publish time and says nothing about
		// what the policy asks for.
		if key == "updatedAt" {
			continue
		}
		out[key] = value
	}
	return out
}
