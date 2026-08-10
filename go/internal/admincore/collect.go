package admincore

import (
	"fmt"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// SetCollect turns session collection on or off in the shared policy, and
// optionally sets the debounce window and the history cutoff.
//
// since and quiet are pointers so that `collect enable` can leave either field
// untouched: a nil pointer means "keep whatever is already there", which is
// what lets an operator flip the switch without restating options they set
// earlier. The change rides the same policy.json every other setting does.
func (m *Manager) SetCollect(enabled bool, since *string, quiet *int) (model.Policy, error) {
	p, err := m.CurrentPolicy()
	if err != nil {
		return model.Policy{}, err
	}
	p.CollectEnabled = enabled
	if since != nil {
		p.CollectSince = *since
	}
	if quiet != nil {
		p.CollectQuietSeconds = *quiet
	}
	return m.publishPolicy(p)
}

// CollectStat is one employee's collected-data summary: how many session files
// have been uploaded, split by tool, and when the most recent one arrived.
type CollectStat struct {
	User   string
	Claude int
	Codex  int
	Total  int
	Latest time.Time // zero when nothing has been uploaded
}

// CollectStats reports, per employee on the roster, how much session data has
// landed under their data_collect/ prefix.
//
// It lists object metadata only -- it never downloads a conversation -- so it
// answers "is anything actually being collected" cheaply and without touching
// the sensitive contents. The returned bool is the current global
// collectEnabled, so the caller can show whether new uploads are even
// expected: a pile of objects with collection now off means old data, not a
// live feed.
func (m *Manager) CollectStats() ([]CollectStat, bool, error) {
	us, err := m.LoadUsers()
	if err != nil {
		return nil, false, err
	}
	pol, err := m.CurrentPolicy()
	if err != nil {
		return nil, false, err
	}
	stats := make([]CollectStat, 0, len(us.Users))
	for _, e := range us.Users {
		prefix := ossclient.DataCollectPrefix(e.WindowsUser)
		infos, err := m.Store.ListInfo(prefix)
		if err != nil {
			return nil, false, fmt.Errorf("list collected data for %q: %w", e.WindowsUser, err)
		}
		s := CollectStat{User: e.WindowsUser}
		for _, o := range infos {
			// The key mirrors the source tree, so the segment right after the
			// prefix tells us which tool the file came from.
			rest := strings.TrimPrefix(o.Key, prefix)
			switch {
			case strings.HasPrefix(rest, ".claude/"):
				s.Claude++
			case strings.HasPrefix(rest, ".codex/"):
				s.Codex++
			}
			s.Total++
			if o.LastModified.After(s.Latest) {
				s.Latest = o.LastModified
			}
		}
		stats = append(stats, s)
	}
	return stats, pol.CollectEnabled, nil
}
