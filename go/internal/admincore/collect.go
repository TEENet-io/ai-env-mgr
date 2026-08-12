package admincore

import (
	"fmt"
	"sort"
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
	// Collection is machine-wide and keyed by the Windows user whose profile a
	// session came from, which can include accounts that were never added to
	// the roster. So the stats are built from the objects themselves rather
	// than from the roster; roster employees are seeded at zero so they still
	// show before any upload arrives.
	byUser := map[string]*CollectStat{}
	ensure := func(u string) *CollectStat {
		s := byUser[u]
		if s == nil {
			s = &CollectStat{User: u}
			byUser[u] = s
		}
		return s
	}
	for _, e := range us.Users {
		ensure(e.WindowsUser)
	}

	infos, err := m.Store.ListInfo(ossclient.Root)
	if err != nil {
		return nil, false, fmt.Errorf("list collected data: %w", err)
	}
	for _, o := range infos {
		user, rel, ok := ossclient.DataCollectUser(o.Key)
		if !ok {
			continue // not a data_collect object (policy, bindings, status, ...)
		}
		s := ensure(user)
		// The key mirrors the source tree, so the first segment of rel tells us
		// which tool the file came from.
		switch {
		case strings.HasPrefix(rel, ".claude/"):
			s.Claude++
		case strings.HasPrefix(rel, ".codex/"):
			s.Codex++
		}
		s.Total++
		if o.LastModified.After(s.Latest) {
			s.Latest = o.LastModified
		}
	}

	stats := make([]CollectStat, 0, len(byUser))
	for _, s := range byUser {
		stats = append(stats, *s)
	}
	sort.Slice(stats, func(i, j int) bool { return stats[i].User < stats[j].User })
	return stats, pol.CollectEnabled, nil
}
