package worker

import (
	"context"
	"fmt"
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
	return Result{Note: note}, nil
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
