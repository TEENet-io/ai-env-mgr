package worker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/directory"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// DirectoryFetcher reads the customer's list of people.
type DirectoryFetcher interface {
	Fetch(ctx context.Context) (directory.List, error)
}

// DirectoryCopy is the console's copy of that list and what applies a
// reading to the console's own accounts (dbstore.DirectoryStore).
type DirectoryCopy interface {
	Known(ctx context.Context) (map[string]directory.Account, error)
	ApplyReading(ctx context.Context, list directory.List, plan directory.Plan) (string, error)
	RecordFailure(ctx context.Context, cause error) (time.Time, error)
	GoneWithWorkspace(ctx context.Context) ([]directory.Gone, error)
}

// DirectoryAlertAfter is how long readings may fail before somebody is
// told. A restart of the custom tool takes a minute or two; a quarter of an
// hour without the list means people who were disabled there still have
// their sessions here.
const DirectoryAlertAfter = 15 * time.Minute

// DirectorySync reads the list and applies what changed (internal/directory
// has the rules). A reading that fails changes nothing: it is recorded, and
// after DirectoryAlertAfter an alert says the list has not been read. The
// task itself still succeeds, so a long outage is one alert rather than a
// failed-task alert every five minutes.
type DirectorySync struct {
	Store     repo.Store
	Directory DirectoryFetcher
	Copy      DirectoryCopy
	Now       func() time.Time

	// Offboard, when set, closes the AI workspace account of somebody the
	// list has as disabled or deleted (ops.Service.OffboardForDirectory).
	// Unset, the alert the reading opened asks a person to.
	Offboard func(ctx context.Context, employeeID string) (repo.Employee, error)
}

func (h DirectorySync) Run(ctx context.Context, _ repo.Task) (Result, error) {
	list, err := h.Directory.Fetch(ctx)
	if err != nil {
		return h.failed(ctx, err)
	}
	known, err := h.Copy.Known(ctx)
	if err != nil {
		return Result{}, err
	}
	note, err := h.Copy.ApplyReading(ctx, list, directory.Compare(known, list))
	if err != nil {
		return Result{}, err
	}
	if h.Offboard != nil {
		closed, failed, err := h.closeGone(ctx)
		if closed > 0 {
			note += fmt.Sprintf("; %d closed", closed)
		}
		if len(failed) > 0 {
			note += "; could not close " + strings.Join(failed, ", ")
		}
		if err != nil {
			return Result{Note: note}, err
		}
	}
	return Result{Note: note}, nil
}

// closeGone closes every account the list says has no owner any more. Each
// close is a follow-up for a person: the instance and the data are still
// there, and releasing them follows the leaving process.
func (h DirectorySync) closeGone(ctx context.Context) (int, []string, error) {
	gone, err := h.Copy.GoneWithWorkspace(ctx)
	if err != nil {
		return 0, nil, err
	}
	closed := 0
	var failed []string
	for _, g := range gone {
		if _, err := h.Offboard(ctx, g.EmployeeID); err != nil {
			// The others are still closed; this one is tried again at the
			// next reading, and a person hears about it now.
			failed = append(failed, g.WindowsUser)
			if _, _, aerr := h.Store.Alerts().Open(ctx, repo.NewAlert{
				Kind: repo.AlertDirectoryFollowUp, Fingerprint: repo.AlertDirectoryFollowUp + ":close-failed:" + g.EmployeeID,
				Severity: repo.SeverityCrit, SubjectType: "directory_account", SubjectID: g.Person.ID,
				Title:  "系統聯動關戶失敗：" + g.Person.DisplayName(),
				Detail: fmt.Sprintf("AI 工作間帳號 %s 沒能自動關戶：%v。下一次讀名單時會再試；請盡快人工關戶。", g.WindowsUser, err),
			}); aerr != nil {
				return closed, failed, aerr
			}
			continue
		}
		if _, err := h.Store.Alerts().Resolve(ctx, repo.AlertDirectoryFollowUp+":close-failed:"+g.EmployeeID, "directory-sync"); err != nil {
			return closed, failed, err
		}
		closed++
		// The alert asking a person to close it is answered.
		if _, err := h.Store.Alerts().Resolve(ctx, repo.AlertDirectoryFollowUp+":disabled:"+g.Person.ID, "directory-sync"); err != nil {
			return closed, failed, err
		}
		word := "停用"
		if g.Person.Status == directory.StatusDeleted {
			word = "刪除"
		}
		if _, _, err := h.Store.Alerts().Open(ctx, repo.NewAlert{
			Kind: repo.AlertDirectoryFollowUp, Fingerprint: repo.AlertDirectoryFollowUp + ":closed:" + g.EmployeeID,
			Severity: repo.SeverityWarn, SubjectType: "directory_account", SubjectID: g.Person.ID,
			Title: "系統聯動關戶：" + g.Person.DisplayName(),
			Detail: fmt.Sprintf("%s 的定製工具帳號已%s，AI 工作間帳號 %s 已自動關戶（令牌吊銷、配置撤回、Codex 關閉）。實例和資料都還在，請按離職流程跟進實例釋放和資料交接。",
				g.Person.DisplayName(), word, g.WindowsUser),
		}); err != nil {
			return closed, failed, err
		}
	}
	// A close that failed is an alert, not a failed task: the list was read
	// and applied, and a task failing every five minutes would bury it.
	return closed, failed, nil
}

func (h DirectorySync) failed(ctx context.Context, cause error) (Result, error) {
	since, err := h.Copy.RecordFailure(ctx, cause)
	if err != nil {
		return Result{}, err
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	if now().Sub(since) >= DirectoryAlertAfter {
		if _, _, err := h.Store.Alerts().Open(ctx, repo.NewAlert{
			Kind: repo.AlertDirectorySync, Fingerprint: repo.AlertDirectorySync, Severity: repo.SeverityCrit,
			SubjectType: "directory",
			Title:       "讀不到定製工具的帳號名單",
			Detail: fmt.Sprintf("自 %s 起讀取失敗，最近一次：%v。這段時間在定製工具裡停用的帳號，在工單系統和控制台的會話還沒有結束。",
				since.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04"), cause),
		}); err != nil {
			return Result{}, err
		}
	}
	return Result{Note: "reading failed: " + cause.Error()}, nil
}
