package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
	"github.com/TEENet-io/ai-env-mgr/internal/secrets"
)

// Gateway is what the provisioning handlers need from LiteLLM. It is an
// interface so the handlers can be tested without a gateway, and so a second
// gateway type is a new implementation rather than a rewrite.
type Gateway interface {
	UpsertUser(ctx context.Context, spec litellm.UserSpec) error
	GenerateKey(ctx context.Context, alias, userID string, models []string, metadata map[string]string) (litellm.Key, error)
	FindKeyByAlias(ctx context.Context, alias string) (litellm.Key, bool, error)
	DeleteKeyByAlias(ctx context.Context, alias string) error
	UpdateKey(ctx context.Context, handle string, models []string) error
	DeleteUser(ctx context.Context, userID string) error
}

// GatewayUserID is the gateway's id for an employee.
//
// It is derived from the Windows user name and deliberately does not carry the
// epoch: the user owns the spend history, and minting a new one on every
// re-issue would scatter one person's usage across a row of accounts.
func GatewayUserID(windowsUser string) string { return "emp-" + strings.ToLower(windowsUser) }

// KeyAlias is the gateway's name for one issued token. Unlike the user id it
// carries the epoch, so that a re-issue is a new alias rather than a
// collision, and a fragment of the employee's own id, so that a deleted
// account's name reused by a new person starts its aliases afresh instead of
// running into the old account's history. Aliases already issued keep their
// old spelling; nothing looks one up by rebuilding it.
func KeyAlias(windowsUser, employeeID string, epoch int) string {
	id := strings.ReplaceAll(employeeID, "-", "")
	if len(id) > 8 {
		id = id[:8]
	}
	return fmt.Sprintf("emp-%s-%s-e%d", strings.ToLower(windowsUser), id, epoch)
}

// taskPayload is what ops writes into a task. It carries references only.
type taskPayload struct {
	EmployeeID  string `json:"employee_id"`
	WindowsUser string `json:"windows_user"`
	Epoch       int    `json:"epoch"`
}

// GatewayProvision makes the gateway match what the console has decided for
// one employee: a user with the right limits, exactly one live token, and no
// leftovers from previous epochs.
//
// It is written as a convergence rather than a sequence of steps, because it
// runs at least once and may run after a previous attempt failed anywhere in
// the middle. Asking "what is there now" and fixing the difference is the only
// version of this that is safe to retry.
type GatewayProvision struct {
	Store   repo.Store
	Gateway Gateway
	Keyring secrets.Keyring
	// Catalog is read only while a channel is paused, to hold employees
	// allowed "every model" to the models of the other channels. Nil: a
	// paused channel is not enforced on the gateway (the picker still
	// hides it).
	Catalog ModelCatalog
}

// effectiveModels is the allowlist the gateway should hold for an employee.
// An explicit list is theirs, as chosen. "Every model" (no list) is sent as
// an explicit empty list -- the gateway drops a missing field instead of
// clearing it, so leaving it out after a pause would keep the pause's list
// for good -- except while a channel is paused, when it becomes every model
// the unpaused channels serve.
func (h GatewayProvision) effectiveModels(ctx context.Context, models []string) ([]string, error) {
	if len(models) > 0 {
		return models, nil
	}
	channels, _, err := repo.LoadGatewayChannels(ctx, h.Store.Settings())
	if err != nil {
		return nil, err
	}
	if len(channels.Paused) == 0 || h.Catalog == nil {
		return []string{}, nil
	}
	available, err := h.Catalog.Models(ctx)
	if err != nil {
		return nil, ClassError("gateway_catalog", err)
	}
	names := litellm.Names(litellm.WithoutChannels(available, channels.PausedSet()))
	if len(names) == 0 {
		return nil, Permanent(errors.New("every channel is paused; there is no model left to allow"))
	}
	return names, nil
}

// Run provisions one employee.
func (h GatewayProvision) Run(ctx context.Context, task repo.Task) (Result, error) {
	payload, employee, err := h.load(ctx, task)
	if err != nil {
		return Result{}, err
	}
	if !employee.Active() {
		// Offboarding queues its own revoke; provisioning somebody who has
		// left would undo it.
		return Result{}, Permanent(fmt.Errorf("employee %s is offboarded", employee.WindowsUser))
	}

	quota, err := h.Store.Quotas().Get(ctx, employee.ID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return Result{}, Permanent(fmt.Errorf("employee %s has no quota", employee.WindowsUser))
		}
		return Result{}, err
	}
	models, err := h.Store.Employees().Models(ctx, employee.ID)
	if err != nil {
		return Result{}, err
	}
	if models, err = h.effectiveModels(ctx, models); err != nil {
		return Result{}, err
	}

	userID := GatewayUserID(employee.WindowsUser)
	budget, err := strconv.ParseFloat(quota.MonthlyBudget, 64)
	if err != nil {
		// The column cannot hold anything else, so this is a bug rather than
		// a fact about the data; either way, retrying cannot help.
		return Result{}, Permanent(fmt.Errorf("quota %q is not a number: %w", quota.MonthlyBudget, err))
	}
	// The limits live on the user rather than the key, so that re-issuing a
	// token does not reset the month's spend to zero.
	if err := h.Gateway.UpsertUser(ctx, litellm.UserSpec{
		UserID:     userID,
		Alias:      employee.Name,
		Department: employee.Department,
		Quota: litellm.Quota{
			MonthlyBudgetUSD: budget, RPM: quota.RPM, TPM: quota.TPM, Parallel: quota.Parallel,
		},
		Models: models,
	}); err != nil {
		return Result{}, gatewayError("upsert_user", err)
	}

	if err := h.retireOldKeys(ctx, employee); err != nil {
		return Result{}, err
	}

	alias, err := h.aliasFor(ctx, employee, payload.Epoch)
	if err != nil {
		return Result{}, err
	}
	if done, err := h.alreadyIssued(ctx, employee, alias, models); err != nil || done {
		return Result{Note: "already issued"}, err
	}

	// We hold no token for this alias. If the gateway has one, it is from an
	// attempt that got as far as minting a key and no further -- and its
	// plaintext is gone for good, since /key/generate returns it once. The
	// only way back to a known state is to replace it.
	if _, found, err := h.Gateway.FindKeyByAlias(ctx, alias); err != nil {
		return Result{}, gatewayError("find_key", err)
	} else if found {
		if err := h.Gateway.DeleteKeyByAlias(ctx, alias); err != nil {
			return Result{}, gatewayError("delete_key", err)
		}
	}

	key, err := h.Gateway.GenerateKey(ctx, alias, userID, models, map[string]string{
		"windows_user": employee.WindowsUser,
		"epoch":        strconv.Itoa(payload.Epoch),
	})
	if err != nil {
		return Result{}, gatewayError("generate_key", err)
	}

	sealed, keyVersion, err := h.Keyring.Seal(ctx, []byte(key.Key),
		secrets.AAD("credential_versions", employee.ID, repo.PurposeCodexGateway))
	if err != nil {
		// The token exists on the gateway and we cannot store it. Take it back
		// out rather than leaving a live token nobody has a record of.
		if delErr := h.Gateway.DeleteKeyByAlias(ctx, alias); delErr != nil {
			return Result{}, fmt.Errorf("seal the token: %w (and the key %s could not be withdrawn: %v)", err, alias, delErr)
		}
		return Result{}, fmt.Errorf("seal the token: %w", err)
	}

	err = h.Store.InTx(ctx, func(tx repo.Store) error {
		// Re-read inside the transaction: the employee may have been
		// offboarded while the gateway call was in flight, and storing this
		// token would hand working credentials to somebody who has left.
		current, err := tx.Employees().ByID(ctx, employee.ID)
		if err != nil {
			return err
		}
		if current.AuthEpoch != payload.Epoch {
			return errSuperseded
		}
		credential, err := tx.Credentials().Store(ctx, repo.NewCredential{
			EmployeeID: employee.ID, Epoch: payload.Epoch,
			Purpose: repo.PurposeCodexGateway, Ciphertext: sealed, KeyVersion: keyVersion,
		})
		if err != nil {
			return err
		}
		grant, err := tx.Grants().Create(ctx, repo.NewGrant{
			EmployeeID: employee.ID, Epoch: payload.Epoch,
			ExternalUser: userID, KeyAlias: alias, Models: models, CredentialID: credential.ID,
		})
		if err != nil {
			return err
		}
		// Observed, not assumed: the gateway has just told us it made this.
		if _, err := tx.Grants().RecordActual(ctx, grant.ID, repo.ActualActive, ""); err != nil {
			return err
		}
		// The export queued beside this task may already have run and found
		// no token to deliver. Nothing else would bring it back, so queue it
		// again now that there is one; the export converges, so one that has
		// not run yet costs nothing extra. It goes in the same transaction as
		// the credential: stored-but-never-delivered is a state a retry
		// cannot see, because on the next run the token counts as issued.
		_, _, err = tx.Tasks().Enqueue(ctx, repo.NewTask{
			Kind:           repo.TaskOSSExport,
			IdempotencyKey: "oss_export:employee:" + employee.ID + ":issued:" + alias,
			Payload:        task.Payload,
			TargetEpoch:    &payload.Epoch,
			EmployeeID:     employee.ID,
		})
		return err
	})
	if errors.Is(err, errSuperseded) {
		if delErr := h.Gateway.DeleteKeyByAlias(ctx, alias); delErr != nil {
			return Result{}, fmt.Errorf("the employee moved on while provisioning, and the key %s could not be withdrawn: %w", alias, delErr)
		}
		return Result{}, Permanent(fmt.Errorf("the employee moved to a newer epoch while this ran"))
	}
	if err != nil {
		return Result{}, err
	}
	return Result{ExternalRef: alias}, nil
}

// aliasFor names the token for this epoch. An account that already holds a
// live grant at this epoch keeps that grant's alias, whatever shape it has:
// grants made before aliases carried the employee id are named emp-<user>-e<n>,
// and looking them up under the new name would find nothing, mint a second
// token and then trip over the one-active-grant rule.
func (h GatewayProvision) aliasFor(ctx context.Context, employee repo.Employee, epoch int) (string, error) {
	grant, err := h.Store.Grants().Active(ctx, "", employee.ID)
	if err == nil && grant.Epoch == epoch {
		return grant.KeyAlias, nil
	}
	if err != nil && !errors.Is(err, repo.ErrNotFound) {
		return "", err
	}
	return KeyAlias(employee.WindowsUser, employee.ID, epoch), nil
}

var errSuperseded = errors.New("superseded")

// alreadyIssued reports whether this alias is fully provisioned: a grant, a
// live credential and an observed state to match. A retry after a successful
// run must not mint again -- but it must still push the current allowlist to
// the token, because on the gateway the key's own model list takes precedence
// over the user's, and a model change is exactly what brings a task back here
// at an epoch that already has a token.
func (h GatewayProvision) alreadyIssued(ctx context.Context, employee repo.Employee, alias string, models []string) (bool, error) {
	grant, err := h.Store.Grants().ByKeyAlias(ctx, "", alias)
	if errors.Is(err, repo.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if grant.Desired != repo.GrantActive {
		return false, nil
	}
	credential, err := h.Store.Credentials().Live(ctx, employee.ID, repo.PurposeCodexGateway)
	if errors.Is(err, repo.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if credential.ID != grant.CredentialID {
		return false, nil
	}

	if !sameSet(grant.Models, models) {
		key, found, err := h.Gateway.FindKeyByAlias(ctx, alias)
		if err != nil {
			return false, gatewayError("find_key", err)
		}
		if !found {
			// The token we hold is not on the gateway: fall through to
			// re-issue rather than update something that is not there.
			return false, nil
		}
		if err := h.Gateway.UpdateKey(ctx, key.Handle(), models); err != nil {
			return false, gatewayError("update_key", err)
		}
		if _, err := h.Store.Grants().SetModels(ctx, grant.ID, models); err != nil {
			return false, err
		}
	}
	if grant.Actual != repo.ActualActive {
		if _, err := h.Store.Grants().RecordActual(ctx, grant.ID, repo.ActualActive, ""); err != nil {
			return false, err
		}
	}
	return true, nil
}

// sameSet compares two model lists regardless of order.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, m := range a {
		seen[m]++
	}
	for _, m := range b {
		if seen[m] == 0 {
			return false
		}
		seen[m]--
	}
	return true
}

// retireOldKeys deletes the tokens of grants we no longer want.
//
// A key the gateway still serves for a grant we consider revoked is a token
// nobody thinks exists, which is the worst of the states this whole design is
// arranged to avoid.
func (h GatewayProvision) retireOldKeys(ctx context.Context, employee repo.Employee) error {
	grants, err := h.Store.Grants().ByEmployee(ctx, employee.ID)
	if err != nil {
		return err
	}
	for _, grant := range grants {
		if grant.Desired != repo.GrantRevoked {
			continue
		}
		if grant.Actual == repo.ActualRevoked || grant.Actual == repo.ActualMissing {
			continue
		}
		if err := h.Gateway.DeleteKeyByAlias(ctx, grant.KeyAlias); err != nil {
			return gatewayError("delete_key", err)
		}
		if _, err := h.Store.Grants().RecordActual(ctx, grant.ID, repo.ActualRevoked, ""); err != nil {
			return err
		}
	}
	return nil
}

func (h GatewayProvision) load(ctx context.Context, task repo.Task) (taskPayload, repo.Employee, error) {
	var payload taskPayload
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		return payload, repo.Employee{}, Permanent(fmt.Errorf("unreadable task payload: %w", err))
	}
	if payload.EmployeeID == "" {
		return payload, repo.Employee{}, Permanent(errors.New("task payload names no employee"))
	}
	employee, err := h.Store.Employees().ByID(ctx, payload.EmployeeID)
	if errors.Is(err, repo.ErrNotFound) {
		return payload, repo.Employee{}, Permanent(fmt.Errorf("employee %s no longer exists", payload.EmployeeID))
	}
	if err != nil {
		return payload, repo.Employee{}, err
	}
	if employee.AuthEpoch != payload.Epoch {
		// Claim already skips these; this is the belt to that braces, for a
		// task handed to a handler by anything else.
		return payload, employee, Permanent(fmt.Errorf(
			"this task is for epoch %d and the employee is on %d", payload.Epoch, employee.AuthEpoch))
	}
	return payload, employee, nil
}

// GatewayRevoke takes an employee's tokens off the gateway.
//
// It does not delete the gateway user. The user owns the spend history and the
// audit trail of what this person actually used, and deleting it also deletes
// every key under it -- which was verified against the live gateway on
// 2026-09-18 and is the reason offboarding revokes keys instead.
type GatewayRevoke struct {
	Store   repo.Store
	Gateway Gateway
}

// Run revokes every token the console no longer wants for this employee.
func (h GatewayRevoke) Run(ctx context.Context, task repo.Task) (Result, error) {
	var payload taskPayload
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		return Result{}, Permanent(fmt.Errorf("unreadable task payload: %w", err))
	}
	if payload.EmployeeID == "" {
		return Result{}, Permanent(errors.New("task payload names no employee"))
	}

	grants, err := h.Store.Grants().ByEmployee(ctx, payload.EmployeeID)
	if err != nil {
		return Result{}, err
	}
	revoked := 0
	for _, grant := range grants {
		if grant.Desired != repo.GrantRevoked {
			continue
		}
		if grant.Actual == repo.ActualRevoked || grant.Actual == repo.ActualMissing {
			continue
		}
		// Deleting a key that is not there is success: this runs at least
		// once, and the second run must not report a failure.
		if err := h.Gateway.DeleteKeyByAlias(ctx, grant.KeyAlias); err != nil {
			return Result{}, gatewayError("delete_key", err)
		}
		if _, err := h.Store.Grants().RecordActual(ctx, grant.ID, repo.ActualRevoked, ""); err != nil {
			return Result{}, err
		}
		revoked++
	}
	return Result{Note: fmt.Sprintf("revoked %d token(s)", revoked)}, nil
}

// GatewayDelete removes an employee's gateway user once the account has been
// deleted from the console. Any token still standing is revoked first, so a
// key cannot outlive the user that owned it.
type GatewayDelete struct {
	Store   repo.Store
	Gateway Gateway
}

// Run revokes what is left and deletes the user. Both halves are safe to
// repeat: a missing key or user is the outcome wanted.
func (h GatewayDelete) Run(ctx context.Context, task repo.Task) (Result, error) {
	var payload taskPayload
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		return Result{}, Permanent(fmt.Errorf("unreadable task payload: %w", err))
	}
	if payload.EmployeeID == "" || payload.WindowsUser == "" {
		return Result{}, Permanent(errors.New("task payload names no employee"))
	}
	grants, err := h.Store.Grants().ByEmployee(ctx, payload.EmployeeID)
	if err != nil {
		return Result{}, err
	}
	revoked := 0
	for _, grant := range grants {
		if grant.Actual == repo.ActualRevoked || grant.Actual == repo.ActualMissing {
			continue
		}
		if err := h.Gateway.DeleteKeyByAlias(ctx, grant.KeyAlias); err != nil {
			return Result{}, gatewayError("delete_key", err)
		}
		if _, err := h.Store.Grants().RecordActual(ctx, grant.ID, repo.ActualRevoked, "account deleted"); err != nil {
			return Result{}, err
		}
		revoked++
	}
	// The gateway user is named after the Windows user, not the employee
	// record. If the name has been given to a new account since this delete
	// was queued, the user on the gateway is theirs now: their provisioning
	// re-created it, and deleting it would cut off the wrong person.
	userID := GatewayUserID(payload.WindowsUser)
	if current, err := h.Store.Employees().ByWindowsUser(ctx, payload.WindowsUser); err == nil && current.ID != payload.EmployeeID {
		return Result{Note: fmt.Sprintf("revoked %d token(s); gateway user %s kept, the name is in use again", revoked, userID)}, nil
	} else if err != nil && !errors.Is(err, repo.ErrNotFound) {
		return Result{}, err
	}
	if err := h.Gateway.DeleteUser(ctx, userID); err != nil {
		return Result{}, gatewayError("delete_user", err)
	}
	return Result{Note: fmt.Sprintf("revoked %d token(s), removed gateway user %s", revoked, userID)}, nil
}

// gatewayError labels a gateway failure and decides whether trying again could
// help.
//
// A 4xx is the gateway saying the request itself is wrong, and the tenth
// identical request will be wrong too. A 5xx, a timeout or a connection
// refused is a fact about this moment.
func gatewayError(class string, err error) error {
	var apiErr *litellm.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Status == 404:
			// Gone is what a delete wanted and what a lookup can live with.
			return nil
		case apiErr.Status == 429, apiErr.Status >= 500:
			return ClassError("upstream_"+strconv.Itoa(apiErr.Status), err)
		case apiErr.Status >= 400:
			return Permanent(ClassError("gateway_rejected", err))
		}
	}
	return ClassError(class, err)
}
