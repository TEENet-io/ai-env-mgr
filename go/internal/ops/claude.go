package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
	"github.com/TEENet-io/ai-env-mgr/internal/secrets"
)

// ActionClaudeCredentials is the audit action for a manual Claude sign-in.
const ActionClaudeCredentials = "account.claude_credentials"

// PublishClaudeCredentials stores the files a manual Claude sign-in produced
// and queues their delivery to the employee's machines.
//
// The Codex credentials are minted by the gateway; these are pasted in by an
// administrator who signed in on the employee's behalf. They are sealed as one
// blob under their own purpose, so re-issuing the Codex token neither touches
// nor loses them, and offboarding retires both.
func (s *Service) PublishClaudeCredentials(ctx context.Context, ring secrets.Keyring, employeeID string, set model.CredentialSet, actor, requestID string) error {
	files := model.CredentialSet{}
	for _, path := range []string{model.PathClaudeCreds, model.PathClaudeConfig} {
		if data, ok := set[path]; ok && len(data) > 0 {
			files[path] = data
		}
	}
	if len(files) == 0 {
		return errors.New("publish Claude credentials: nothing to publish")
	}
	plain, err := json.Marshal(files)
	if err != nil {
		return err
	}

	err = s.store.InTx(ctx, func(tx repo.Store) error {
		employee, err := tx.Employees().ByID(ctx, employeeID)
		if err != nil {
			return err
		}
		if !employee.Active() {
			return errors.New("this account is closed")
		}
		sealed, keyVersion, err := ring.Seal(ctx, plain,
			secrets.AAD("credential_versions", employee.ID, repo.PurposeClaudeLogin))
		if err != nil {
			return fmt.Errorf("seal: %w", err)
		}
		credential, err := tx.Credentials().Store(ctx, repo.NewCredential{
			EmployeeID: employee.ID, Epoch: employee.AuthEpoch, Purpose: repo.PurposeClaudeLogin,
			Ciphertext: sealed, KeyVersion: keyVersion,
		})
		if err != nil {
			return err
		}
		if err := s.enqueueEmployeeExport(ctx, tx, employee, "claude:"+credential.ID); err != nil {
			return err
		}
		names := make([]string, 0, len(files))
		for name := range files {
			names = append(names, name)
		}
		// The file names, never the contents: audit rows are read by people.
		return s.audit(ctx, tx, actor, requestID, ActionClaudeCredentials, employee,
			nil, map[string]any{"files": names})
	})
	if err != nil {
		return fmt.Errorf("publish Claude credentials: %w", err)
	}
	return nil
}
