// Package ops is what an administrator's click does.
//
// Every operation here is one database transaction that writes the new state,
// records the audit event, and enqueues the work that has to happen outside
// the database -- the gateway call, the OSS export. Nothing calls the gateway
// inline: a request that waits on an upstream either blocks the person who
// clicked or is lost when the process restarts, and both of those leave the
// console believing something that is not true.
//
// The division of labour is deliberate:
//
//	ops     decides and records; never talks to anything outside the database
//	worker  carries it out, retries it, and reconciles what actually happened
//
// That is what makes a half-finished onboarding impossible. Either the
// employee, the quota, the audit line and the provisioning task are all there,
// or none of them are.
package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// Service performs the console's business operations.
type Service struct {
	store repo.Store
	now   func() time.Time

	// Notifier, when set, is told after a commit which machines the change
	// touched, so an agent waiting on the console hears at once. It is a
	// hint: the export task wakes the same machines again once the objects
	// are written, and the agent re-reads on a timer regardless.
	Notifier Notifier
}

// Notifier wakes agents waiting for their configuration to change.
type Notifier interface {
	Wake(deviceIDs ...string)
	WakeAll()
}

// New builds a Service over a store.
func New(store repo.Store) *Service {
	return &Service{store: store, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) wake(deviceIDs ...string) {
	if s.Notifier != nil && len(deviceIDs) > 0 {
		s.Notifier.Wake(deviceIDs...)
	}
}

func (s *Service) wakeAll() {
	if s.Notifier != nil {
		s.Notifier.WakeAll()
	}
}

// Audit actions. They are constants because they are searched for: an incident
// starts with somebody grepping for one of these strings.
const (
	ActionOnboard       = "account.onboard"
	ActionReopen        = "account.reopen"
	ActionOffboard      = "account.offboard"
	ActionReissue       = "account.reissue"
	ActionRotate        = "account.rotate"
	ActionDelete        = "account.delete"
	ActionSetQuota      = "account.quota"
	ActionSetModels     = "account.models"
	ActionUpdateProfile = "account.profile"
	ActionBind          = "machine.bind"
	ActionUnbind        = "machine.unbind"
	ActionRestartCodex  = "machine.codex_restart"
	ActionRequestSync   = "machine.sync"
	ActionAllowReenrol  = "machine.allow_reenrol"
	ActionRevokeToken   = "machine.revoke_token"
	ActionPublishPolicy = "policy.publish"

	ActionArtifactRegister = "release.artifact_register"
	ActionArtifactStatus   = "release.artifact_status"
	ActionArtifactNotes    = "release.artifact_notes"
	ActionGlobalTarget     = "release.global_target"
	ActionRolloutCreate    = "release.rollout_create"
	ActionRolloutPause     = "release.rollout_pause"
	ActionRolloutCancel    = "release.rollout_cancel"
	ActionTargetExclude    = "release.target_exclude"
	ActionTargetRetry      = "release.target_retry"
	ActionFollowGlobal     = "release.follow_global"
)

// OnboardSpec is everything opening an account needs.
type OnboardSpec struct {
	WindowsUser  string
	Name         string
	Department   string
	CodexAccount string
	Email        string
	ExternalID   string
	Quota        repo.Quota
	Models       []string

	// Actor is the administrator's user name, for the audit trail.
	Actor string
	// RequestID ties the audit row to the HTTP request and to the log line.
	RequestID string
}

// Onboard opens an account, or brings an existing one up to the spec.
//
// Running it twice for the same person is not an error: it updates the labels,
// the quota and the models, raises the epoch and issues a fresh token. That is
// what makes it usable as a repair -- an account whose provisioning failed
// half way is fixed by doing it again, rather than by somebody working out
// which of the steps did happen.
func (s *Service) Onboard(ctx context.Context, spec OnboardSpec) (repo.Employee, error) {
	user := repo.NormalizeWindowsUser(spec.WindowsUser)
	if user == "" {
		return repo.Employee{}, errors.New("onboard: a Windows user name is required")
	}

	var result repo.Employee
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		employee, err := tx.Employees().ByWindowsUser(ctx, user)
		created := false
		switch {
		case errors.Is(err, repo.ErrNotFound):
			employee, err = tx.Employees().Create(ctx, repo.NewEmployee{
				WindowsUser: user, Name: spec.Name, Department: spec.Department,
				CodexAccount: spec.CodexAccount, Email: spec.Email, ExternalID: spec.ExternalID,
			})
			if err != nil {
				return err
			}
			created = true
		case err != nil:
			return err
		}
		before := employee
		action := ActionOnboard

		if !created {
			// An existing row: bring the labels up to what the form says,
			// leaving alone whatever it did not fill in.
			if employee, err = s.applyProfile(ctx, tx, employee, spec); err != nil {
				return err
			}
		}

		// The epoch goes up on every onboarding, including a repeat: a fresh
		// token is issued and whatever was outstanding stops working. An
		// account re-opened after somebody left must not come back with the
		// credentials they walked out with.
		//
		// Reopening does that bump itself, so it replaces the plain bump
		// rather than following it. Two bumps would leave the tasks queued
		// below aimed at an epoch the employee has already passed, and they
		// would never be claimed.
		if before.Active() {
			employee, err = tx.Employees().BumpAuthEpoch(ctx, employee.ID, employee.Version)
		} else {
			action = ActionReopen
			employee, err = tx.Employees().Reopen(ctx, employee.ID, employee.Version)
		}
		if err != nil {
			return err
		}

		if err := s.applyQuota(ctx, tx, employee.ID, spec.Quota); err != nil {
			return err
		}
		if err := tx.Employees().SetModels(ctx, employee.ID, spec.Models); err != nil {
			return err
		}
		if err := s.replaceOutstandingWork(ctx, tx, employee); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, spec.Actor, spec.RequestID, action, employee, before, employee); err != nil {
			return err
		}
		result = employee
		return nil
	})
	if err != nil {
		return repo.Employee{}, fmt.Errorf("onboard %s: %w", user, err)
	}
	return result, nil
}

// Offboard closes an account.
//
// The database part is complete and immediate: the epoch moves, the credential
// is retired, the grant's intent becomes revoked, and any work still queued
// for the old epoch is superseded. What is left is telling the gateway and
// removing the delivered files, and those are tasks -- so a gateway that is
// down delays the revocation rather than losing it.
func (s *Service) Offboard(ctx context.Context, employeeID, actor, requestID string) (repo.Employee, error) {
	var result repo.Employee
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		employee, err := tx.Employees().ByID(ctx, employeeID)
		if err != nil {
			return err
		}
		before := employee

		employee, err = tx.Employees().Offboard(ctx, employee.ID, employee.Version)
		if err != nil {
			return err
		}
		if _, err := tx.Tasks().SupersedeOpenForEmployee(ctx, employee.ID, employee.AuthEpoch); err != nil {
			return err
		}
		if _, err := tx.Credentials().Retire(ctx, employee.ID, repo.PurposeCodexGateway); err != nil {
			return err
		}
		if err := s.revokeGrant(ctx, tx, employee); err != nil {
			return err
		}
		if err := s.enqueueGatewayWork(ctx, tx, employee, repo.TaskGatewayRevoke, ""); err != nil {
			return err
		}
		// The export removes the delivered credentials. Every machine bound to
		// this person reads that object each cycle, and its disappearance is
		// how the revocation reaches the desktop.
		if err := s.enqueueEmployeeExport(ctx, tx, employee, epochMarker(employee)); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, actor, requestID, ActionOffboard, employee, before, employee); err != nil {
			return err
		}
		result = employee
		return nil
	})
	if err != nil {
		return repo.Employee{}, fmt.Errorf("offboard: %w", err)
	}
	return result, nil
}

// Delete removes a closed account from the console: the gateway user goes,
// whatever the export still publishes for them is withdrawn, and the name is
// free for the next person. The row and its history stay. An open account is
// refused -- closing is the step that ends their access, and it has its own
// confirmation.
func (s *Service) Delete(ctx context.Context, employeeID, actor, requestID string) (repo.Employee, error) {
	var result repo.Employee
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		employee, err := tx.Employees().ByID(ctx, employeeID)
		if err != nil {
			return err
		}
		if employee.Active() {
			return errors.New("the account is still open; close it first")
		}
		before := employee
		employee, err = tx.Employees().Delete(ctx, employee.ID, employee.Version)
		if err != nil {
			return err
		}
		if _, err := tx.Tasks().SupersedeOpenForEmployee(ctx, employee.ID, employee.AuthEpoch+1); err != nil {
			return err
		}
		if err := s.enqueueGatewayWork(ctx, tx, employee, repo.TaskGatewayDelete, "delete"); err != nil {
			return err
		}
		if err := s.enqueueEmployeeExport(ctx, tx, employee, "delete"); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, actor, requestID, ActionDelete, employee, before, employee); err != nil {
			return err
		}
		result = employee
		return nil
	})
	if err != nil {
		return repo.Employee{}, fmt.Errorf("delete account: %w", err)
	}
	return result, nil
}

// Reissue replaces an employee's token without changing anything else: a
// suspected leak, or a machine that was handed to somebody else.
func (s *Service) Reissue(ctx context.Context, employeeID, actor, requestID string) (repo.Employee, error) {
	return s.reissue(ctx, employeeID, ActionReissue, actor, requestID)
}

// Rotate is Reissue on a schedule: the same replacement, recorded under its
// own action so the audit trail tells a leak from routine.
func (s *Service) Rotate(ctx context.Context, employeeID, actor, requestID string) (repo.Employee, error) {
	return s.reissue(ctx, employeeID, ActionRotate, actor, requestID)
}

func (s *Service) reissue(ctx context.Context, employeeID, action, actor, requestID string) (repo.Employee, error) {
	var result repo.Employee
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		employee, err := tx.Employees().ByID(ctx, employeeID)
		if err != nil {
			return err
		}
		if !employee.Active() {
			return errors.New("this account is closed; reopen it instead")
		}
		before := employee

		employee, err = tx.Employees().BumpAuthEpoch(ctx, employee.ID, employee.Version)
		if err != nil {
			return err
		}
		if err := s.replaceOutstandingWork(ctx, tx, employee); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, actor, requestID, action, employee, before, employee); err != nil {
			return err
		}
		result = employee
		return nil
	})
	if err != nil {
		return repo.Employee{}, fmt.Errorf("re-issue credentials: %w", err)
	}
	return result, nil
}

// SetQuota changes the limits. expectVersion is the version the form was
// rendered from, so a second administrator's save is refused rather than
// silently overwritten.
//
// It does not touch the epoch: a quota change is not a reason to make somebody
// sign in again, and the gateway applies it to the user rather than the token.
func (s *Service) SetQuota(ctx context.Context, employeeID string, q repo.Quota, expectVersion int, actor, requestID string) error {
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		employee, err := tx.Employees().ByID(ctx, employeeID)
		if err != nil {
			return err
		}
		before, err := tx.Quotas().Get(ctx, employeeID)
		if err != nil && !errors.Is(err, repo.ErrNotFound) {
			return err
		}
		after, err := tx.Quotas().Set(ctx, employeeID, q, expectVersion)
		if err != nil {
			return err
		}
		if err := s.enqueueGatewayWork(ctx, tx, employee, repo.TaskGatewayProvision,
			"quota:v"+strconv.Itoa(after.Version)); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, requestID, ActionSetQuota, employee, before, after)
	})
	if err != nil {
		return fmt.Errorf("set quota: %w", err)
	}
	return nil
}

// SetModels changes which models an employee may call.
func (s *Service) SetModels(ctx context.Context, employeeID string, models []string, actor, requestID string) error {
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		employee, err := tx.Employees().ByID(ctx, employeeID)
		if err != nil {
			return err
		}
		before, err := tx.Employees().Models(ctx, employeeID)
		if err != nil {
			return err
		}
		if err := tx.Employees().SetModels(ctx, employeeID, models); err != nil {
			return err
		}
		after, err := tx.Employees().Models(ctx, employeeID)
		if err != nil {
			return err
		}
		// Keyed on when, not on what: a list changed A -> B -> A would find
		// the finished task for A and never reach the gateway.
		modelsMarker := "models:" + strconv.FormatInt(s.now().UnixNano(), 36)
		if err := s.enqueueGatewayWork(ctx, tx, employee, repo.TaskGatewayProvision, modelsMarker); err != nil {
			return err
		}
		// The models decide both what the token may call and what the picker
		// shows, so the delivered catalog has to be rewritten as well.
		if err := s.enqueueEmployeeExport(ctx, tx, employee, modelsMarker); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, requestID, ActionSetModels, employee,
			map[string]any{"models": before}, map[string]any{"models": after})
	})
	if err != nil {
		return fmt.Errorf("set models: %w", err)
	}
	return nil
}

// UpdateProfile edits the labels. They are mirrored onto the gateway user so
// its own UI shows the same person, which is why this enqueues anything at all.
func (s *Service) UpdateProfile(ctx context.Context, employeeID string, version int, p repo.Profile, actor, requestID string) error {
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		before, err := tx.Employees().ByID(ctx, employeeID)
		if err != nil {
			return err
		}
		after, err := tx.Employees().UpdateProfile(ctx, employeeID, version, p)
		if err != nil {
			return err
		}
		if err := s.enqueueGatewayWork(ctx, tx, after, repo.TaskGatewayProvision,
			"profile:v"+strconv.Itoa(after.Version)); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, requestID, ActionUpdateProfile, after, before, after)
	})
	if err != nil {
		return fmt.Errorf("update profile: %w", err)
	}
	return nil
}

// BindMachine assigns a machine to an employee, replacing whoever had it.
//
// Unbinding and binding are one transaction: a machine left half reassigned is
// how one employee's credentials reach another.
func (s *Service) BindMachine(ctx context.Context, hostname, employeeID, note, actor, requestID string) (repo.Binding, error) {
	var result repo.Binding
	var deviceID string
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		device, err := tx.Devices().EnsureByHostname(ctx, hostname)
		if err != nil {
			return err
		}
		deviceID = device.ID
		employee, err := tx.Employees().ByID(ctx, employeeID)
		if err != nil {
			return err
		}
		if !employee.Active() {
			return errors.New("this account is closed; reopen it before assigning a machine")
		}

		previous, err := tx.Bindings().Open(ctx, device.ID)
		switch {
		case err == nil:
			if _, err := tx.Bindings().Unbind(ctx, device.ID, actor); err != nil {
				return err
			}
		case !errors.Is(err, repo.ErrNotFound):
			return err
		}

		binding, err := tx.Bindings().Bind(ctx, device.ID, employee.ID, note, actor)
		if err != nil {
			return err
		}
		// The machine's binding object, and both people's delivered files --
		// the one who lost the machine as much as the one who got it.
		marker := "bind:" + strconv.Itoa(binding.Epoch)
		if err := s.enqueueDeviceExport(ctx, tx, device, marker); err != nil {
			return err
		}
		if err := s.enqueueEmployeeExport(ctx, tx, employee, marker+":"+device.ID); err != nil {
			return err
		}
		if previous.EmployeeID != "" && previous.EmployeeID != employee.ID {
			former, err := tx.Employees().ByID(ctx, previous.EmployeeID)
			if err != nil {
				return err
			}
			if err := s.enqueueEmployeeExport(ctx, tx, former, marker+":"+device.ID); err != nil {
				return err
			}
		}
		if err := s.auditTarget(ctx, tx, actor, requestID, ActionBind, "device", device.ID,
			map[string]any{"hostname": device.Hostname, "employee": previous.EmployeeID},
			map[string]any{"hostname": device.Hostname, "employee": employee.ID, "note": note}); err != nil {
			return err
		}
		result = binding
		return nil
	})
	if err != nil {
		return repo.Binding{}, fmt.Errorf("bind machine %s: %w", hostname, err)
	}
	s.wake(deviceID)
	return result, nil
}

// UnbindMachine takes a machine away from whoever has it.
func (s *Service) UnbindMachine(ctx context.Context, hostname, actor, requestID string) error {
	var deviceID string
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		device, err := tx.Devices().ByHostname(ctx, hostname)
		if err != nil {
			return err
		}
		deviceID = device.ID
		binding, err := tx.Bindings().Unbind(ctx, device.ID, actor)
		if err != nil {
			return err
		}
		if err := s.enqueueDeviceExport(ctx, tx, device,
			"unbind:"+strconv.Itoa(binding.Epoch)); err != nil {
			return err
		}
		return s.auditTarget(ctx, tx, actor, requestID, ActionUnbind, "device", device.ID,
			map[string]any{"hostname": device.Hostname, "employee": binding.EmployeeID},
			map[string]any{"hostname": device.Hostname, "employee": nil})
	})
	if err != nil {
		return fmt.Errorf("unbind machine %s: %w", hostname, err)
	}
	s.wake(deviceID)
	return nil
}

// RequestCodexRestart asks the machine's agent to end the bound employee's
// Codex once, and returns the nonce the agent will echo back when it has.
func (s *Service) RequestCodexRestart(ctx context.Context, hostname, actor, requestID string) (string, error) {
	nonce := strconv.FormatInt(s.now().UnixNano(), 36)
	var deviceID string
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		device, err := tx.Devices().ByHostname(ctx, hostname)
		if err != nil {
			return err
		}
		deviceID = device.ID
		if _, err := tx.Bindings().RequestCodexRestart(ctx, device.ID, nonce); err != nil {
			return err
		}
		// Keyed on the nonce: every request is its own task, because the point
		// of a one-shot instruction is that asking again asks again.
		if err := s.enqueueDeviceExport(ctx, tx, device, "restart:"+nonce); err != nil {
			return err
		}
		return s.auditTarget(ctx, tx, actor, requestID, ActionRestartCodex, "device", device.ID,
			nil, map[string]any{"hostname": device.Hostname, "nonce": nonce})
	})
	if err != nil {
		return "", fmt.Errorf("ask %s to restart Codex: %w", hostname, err)
	}
	s.wake(deviceID)
	return nonce, nil
}

// RequestSync asks a machine's agent to run a full sync now. It writes a
// nonce into the binding object; an agent from 1.2.16 checks that object's
// ETag every minute and syncs when it moves. Older agents ignore it and
// sync on their interval as before.
func (s *Service) RequestSync(ctx context.Context, hostname, actor, requestID string) (string, error) {
	nonce := strconv.FormatInt(s.now().UnixNano(), 36)
	var deviceID string
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		device, err := tx.Devices().ByHostname(ctx, hostname)
		if err != nil {
			return err
		}
		deviceID = device.ID
		if _, err := tx.Devices().RequestSync(ctx, device.ID, nonce); err != nil {
			return err
		}
		if err := s.enqueueDeviceExport(ctx, tx, device, "sync:"+nonce); err != nil {
			return err
		}
		return s.auditTarget(ctx, tx, actor, requestID, ActionRequestSync, "device", device.ID,
			nil, map[string]any{"hostname": device.Hostname, "nonce": nonce})
	})
	if err != nil {
		return "", fmt.Errorf("ask %s to sync: %w", hostname, err)
	}
	s.wake(deviceID)
	return nonce, nil
}

// PublishPolicy stores a new fleet policy and makes it current.
//
// The content is validated by the caller, which owns the rules; this records
// the decision and queues the export that puts it where the agents look.
func (s *Service) PublishPolicy(ctx context.Context, content []byte, note, actor, requestID string) (repo.PolicyVersion, error) {
	var result repo.PolicyVersion
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		before, err := tx.Policies().Current(ctx)
		if err != nil && !errors.Is(err, repo.ErrNotFound) {
			return err
		}
		published, err := s.publishPolicyTx(ctx, tx, content, note, actor, requestID, before.Version,
			json.RawMessage(nonEmpty(before.Content)), json.RawMessage(content))
		if err != nil {
			return err
		}
		result = published
		return nil
	})
	if err != nil {
		return repo.PolicyVersion{}, fmt.Errorf("publish policy: %w", err)
	}
	s.wakeAll()
	return result, nil
}

// publishPolicyTx is the shared tail of every policy change: store the
// version, point the fleet at it, queue the export, write the audit row.
func (s *Service) publishPolicyTx(ctx context.Context, tx repo.Store, content []byte, note, actor, requestID string, beforeVersion int64, before, after any) (repo.PolicyVersion, error) {
	published, err := tx.Policies().Publish(ctx, content, note, actor)
	if err != nil {
		return repo.PolicyVersion{}, err
	}
	if _, _, err := tx.Tasks().Enqueue(ctx, repo.NewTask{
		Kind:           repo.TaskOSSExport,
		IdempotencyKey: fmt.Sprintf("oss_export:policy:%d", published.Version),
		Payload:        mustJSON(map[string]any{"policy_version": published.Version}),
	}); err != nil {
		return repo.PolicyVersion{}, err
	}
	if err := s.auditTarget(ctx, tx, actor, requestID, ActionPublishPolicy, "policy",
		strconv.FormatInt(published.Version, 10),
		map[string]any{"version": beforeVersion, "policy": before},
		map[string]any{"version": published.Version, "note": note, "policy": after}); err != nil {
		return repo.PolicyVersion{}, err
	}
	return published, nil
}

// replaceOutstandingWork is what "this employee's credentials have changed"
// means in the database: stop what was queued for the old epoch, mark the old
// grant as one we no longer want, and queue the new provisioning and export.
//
// The order matters only in that it is all one transaction. What must not
// happen is the new task existing while the old one is still runnable: the two
// would race, and the loser would leave the gateway holding a token nobody
// has a record of.
func (s *Service) replaceOutstandingWork(ctx context.Context, tx repo.Store, employee repo.Employee) error {
	if _, err := tx.Tasks().SupersedeOpenForEmployee(ctx, employee.ID, employee.AuthEpoch); err != nil {
		return err
	}
	if err := s.revokeGrant(ctx, tx, employee); err != nil {
		return err
	}
	if err := s.enqueueGatewayWork(ctx, tx, employee, repo.TaskGatewayProvision, ""); err != nil {
		return err
	}
	return s.enqueueEmployeeExport(ctx, tx, employee, epochMarker(employee))
}

// revokeGrant marks the employee's live gateway grant as unwanted, if there is
// one. It does not call the gateway: that is the revoke task's job, and the
// row records the intent in the meantime.
func (s *Service) revokeGrant(ctx context.Context, tx repo.Store, employee repo.Employee) error {
	grant, err := tx.Grants().Active(ctx, "", employee.ID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.Grants().Revoke(ctx, grant.ID)
	return err
}

// enqueueGatewayWork adds a gateway task for an employee at their current
// epoch.
//
// The idempotency key is derived from the employee, the epoch and the kind --
// never from the time -- so that a repeated request, a double-clicked button
// or a retry after an ambiguous failure all find the task that is already
// there instead of creating a second one.
//
// marker distinguishes work at the same epoch. Issuing a token is keyed on the
// epoch alone, because the epoch is what a token belongs to; a quota, model or
// label change does not move the epoch, and without a marker its key matched
// the provisioning that had already run and the gateway never heard about it.
// The handler converges on current state, so a second run is always safe.
func (s *Service) enqueueGatewayWork(ctx context.Context, tx repo.Store, employee repo.Employee, kind, marker string) error {
	epoch := employee.AuthEpoch
	key := fmt.Sprintf("%s:%s:%d", kind, employee.ID, epoch)
	if marker != "" {
		key += ":" + marker
	}
	_, _, err := tx.Tasks().Enqueue(ctx, repo.NewTask{
		Kind:           kind,
		IdempotencyKey: key,
		Payload: mustJSON(map[string]any{
			"employee_id": employee.ID, "windows_user": employee.WindowsUser, "epoch": epoch,
		}),
		TargetEpoch: &epoch,
		EmployeeID:  employee.ID,
	})
	return err
}

// enqueueEmployeeExport asks for this employee's delivered files to be
// rewritten.
//
// marker is what makes a second export a second task. Keying an export on the
// employee and epoch alone looked tidy and was wrong: binding a machine does
// not change the epoch, so the key matched the export that had already run
// during onboarding, Enqueue handed back that finished task, and the new
// binding was never written. Anything that should reach a desktop needs a
// marker that moves.
func (s *Service) enqueueEmployeeExport(ctx context.Context, tx repo.Store, employee repo.Employee, marker string) error {
	_, err := s.enqueueEmployeeExportTask(ctx, tx, employee, marker)
	return err
}

// enqueueEmployeeExportTask is enqueueEmployeeExport for a caller that
// follows the task.
func (s *Service) enqueueEmployeeExportTask(ctx context.Context, tx repo.Store, employee repo.Employee, marker string) (repo.Task, error) {
	epoch := employee.AuthEpoch
	task, _, err := tx.Tasks().Enqueue(ctx, repo.NewTask{
		Kind:           repo.TaskOSSExport,
		IdempotencyKey: fmt.Sprintf("oss_export:employee:%s:%s", employee.ID, marker),
		Payload: mustJSON(map[string]any{
			"employee_id": employee.ID, "windows_user": employee.WindowsUser, "epoch": epoch,
		}),
		TargetEpoch: &epoch,
		EmployeeID:  employee.ID,
	})
	return task, err
}

// enqueueDeviceExport asks for one machine's binding object to be rewritten.
//
// It is not tied to an employee epoch: unbinding leaves no employee to aim at,
// and a restart request has to reach the machine whatever epoch the person is
// on.
func (s *Service) enqueueDeviceExport(ctx context.Context, tx repo.Store, device repo.Device, marker string) error {
	_, _, err := tx.Tasks().Enqueue(ctx, repo.NewTask{
		Kind:           repo.TaskOSSExport,
		IdempotencyKey: fmt.Sprintf("oss_export:device:%s:%s", device.ID, marker),
		Payload:        mustJSON(map[string]any{"device_id": device.ID, "hostname": device.Hostname}),
		DeviceID:       device.ID,
	})
	return err
}

func (s *Service) audit(ctx context.Context, tx repo.Store, actor, requestID, action string, employee repo.Employee, before, after any) error {
	return s.auditTarget(ctx, tx, actor, requestID, action, "employee", employee.ID, before, after)
}

func (s *Service) auditTarget(ctx context.Context, tx repo.Store, actor, requestID, action, targetType, targetID string, before, after any) error {
	_, err := tx.Audit().Append(ctx, repo.AuditEvent{
		OccurredAt: s.now(),
		ActorType:  repo.ActorAdmin,
		ActorID:    actor,
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		Before:     toJSON(before),
		After:      toJSON(after),
		RequestID:  requestID,
	})
	return err
}

func (s *Service) applyProfile(ctx context.Context, tx repo.Store, employee repo.Employee, spec OnboardSpec) (repo.Employee, error) {
	// Empty fields leave what is on the roster alone: re-opening an account
	// from a form somebody did not re-type must not blank the notes.
	profile := repo.Profile{
		Name:         firstNonEmpty(spec.Name, employee.Name),
		Department:   firstNonEmpty(spec.Department, employee.Department),
		CodexAccount: firstNonEmpty(spec.CodexAccount, employee.CodexAccount),
		Email:        firstNonEmpty(spec.Email, employee.Email),
		ExternalID:   firstNonEmpty(spec.ExternalID, employee.ExternalID),
	}
	return tx.Employees().UpdateProfile(ctx, employee.ID, employee.Version, profile)
}

// applyQuota writes the limits, creating them if this is a new account and
// updating them at whatever version is current otherwise. Onboarding is the
// one place where "whatever is there" is the right expectation: the form the
// administrator filled in is the decision.
func (s *Service) applyQuota(ctx context.Context, tx repo.Store, employeeID string, q repo.Quota) error {
	current, err := tx.Quotas().Get(ctx, employeeID)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		_, err = tx.Quotas().Set(ctx, employeeID, q, 0)
	case err != nil:
		return err
	default:
		_, err = tx.Quotas().Set(ctx, employeeID, q, current.Version)
	}
	return err
}

// epochMarker is the export marker for a credential change: the epoch, which
// moves every time the token does.
func epochMarker(employee repo.Employee) string {
	return "e" + strconv.Itoa(employee.AuthEpoch)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// toJSON renders an audit value, dropping anything that will not marshal
// rather than failing the operation: losing one before-value is bad, failing
// the offboarding it describes is worse.
func toJSON(v any) []byte {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

// AllowReenrol opens a window (a day) in which a machine the console knows
// may enrol: a machine moving from the bucket to the console, a
// reinstalled disk, a lost token file. Any token it holds keeps working
// until the new enrolment replaces it.
func (s *Service) AllowReenrol(ctx context.Context, hostname, actor, requestID string) (time.Time, error) {
	until := s.now().Add(24 * time.Hour)
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		device, err := tx.Devices().ByHostname(ctx, hostname)
		if err != nil {
			return err
		}
		if err := tx.Devices().AllowReenrol(ctx, device.ID, until); err != nil {
			return err
		}
		return s.auditTarget(ctx, tx, actor, requestID, ActionAllowReenrol, "device", device.ID,
			nil, map[string]any{"hostname": device.Hostname, "until": until.UTC().Format(time.RFC3339)})
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("allow %s to enrol again: %w", hostname, err)
	}
	return until, nil
}

// revokeReenrolWindow is how long a machine whose token was revoked may
// enrol again on its own.
const revokeReenrolWindow = time.Hour

// RevokeDeviceToken ends the machine's token at once. Its next request is
// refused and its agent enrols again, inside the hour this opens -- so
// this is "make the machine start over", not "lock it
// out"; forgetting the machine is what locks it out.
func (s *Service) RevokeDeviceToken(ctx context.Context, hostname, actor, requestID string) error {
	var deviceID string
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		device, err := tx.Devices().ByHostname(ctx, hostname)
		if err != nil {
			return err
		}
		deviceID = device.ID
		n, err := tx.DeviceTokens().Revoke(ctx, device.ID)
		if err != nil {
			return err
		}
		// The console knows this machine, so its enrolment is refused unless
		// a window is open. Open a short one: the agent is woken below and
		// enrols again within seconds, which closes it.
		until := s.now().Add(revokeReenrolWindow)
		if err := tx.Devices().AllowReenrol(ctx, device.ID, until); err != nil {
			return err
		}
		return s.auditTarget(ctx, tx, actor, requestID, ActionRevokeToken, "device", device.ID,
			nil, map[string]any{"hostname": device.Hostname, "revoked": n, "reenrol_until": until.UTC().Format(time.RFC3339)})
	})
	if err != nil {
		return fmt.Errorf("revoke the token of %s: %w", hostname, err)
	}
	s.wake(deviceID)
	return nil
}
