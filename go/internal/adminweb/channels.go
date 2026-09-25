package adminweb

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/ops"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// channelRow is one supplier in the overview's 渠道 table.
type channelRow struct {
	ID, Label string
	Models    []string // the names employees can see, from this channel
	Paused    bool
	Pause     repo.ChannelPause
}

// channelOrder puts the suppliers in a fixed order; unknown ones last.
var channelOrder = map[string]int{litellm.ChannelAWS: 1, litellm.ChannelGoogle: 2, litellm.ChannelAzure: 3, litellm.ChannelOpenAI: 4}

// loadChannels groups the gateway's visible models by supplier and marks
// the paused ones. Database mode only: the pause lives in its settings.
func (s *Server) loadChannels(ctx context.Context, data *pageData) {
	if s.dbm == nil || !data.GatewayEnabled || data.GatewayUnusable != "" {
		return
	}
	state, version, err := repo.LoadGatewayChannels(ctx, s.dbm.store.Settings())
	if err != nil {
		data.Error = "could not read the channels: " + err.Error()
		return
	}
	byID := map[string]*channelRow{}
	for _, m := range data.GatewayModels {
		id := litellm.ChannelOf(m)
		row, ok := byID[id]
		if !ok {
			row = &channelRow{ID: id, Label: litellm.ChannelLabel(id)}
			byID[id] = row
		}
		row.Models = append(row.Models, m.Name)
	}
	// A paused channel stays listed even if the gateway stopped reporting
	// its models, so it can still be resumed.
	for id := range state.Paused {
		if _, ok := byID[id]; !ok {
			byID[id] = &channelRow{ID: id, Label: litellm.ChannelLabel(id)}
		}
	}
	for id, row := range byID {
		row.Pause, row.Paused = state.Paused[id]
		row.Pause.At = localTime(row.Pause.At)
		sort.Strings(row.Models)
		data.GatewayChannels = append(data.GatewayChannels, *row)
	}
	sort.Slice(data.GatewayChannels, func(i, j int) bool {
		a, b := channelOrder[data.GatewayChannels[i].ID], channelOrder[data.GatewayChannels[j].ID]
		if a == 0 {
			a = 99
		}
		if b == 0 {
			b = 99
		}
		if a != b {
			return a < b
		}
		return data.GatewayChannels[i].ID < data.GatewayChannels[j].ID
	})
	data.ChannelsVersion = version
	data.PausedChannels = state.PausedSet()
}

// deliverableModels is the gateway's visible models minus paused channels:
// what a delivery gives employees.
func (s *Server) deliverableModels(ctx context.Context, all []litellm.Model) ([]litellm.Model, error) {
	state, _, err := repo.LoadGatewayChannels(ctx, s.dbm.store.Settings())
	if err != nil {
		return nil, err
	}
	return litellm.WithoutChannels(all, state.PausedSet()), nil
}

// actionChannelPause pauses or resumes a channel, then gives everybody a
// picker without (or again with) its models. Admin only (roles.go).
func (s *Server) actionChannelPause(sess *session, r *http.Request) (string, error) {
	channel := formValue(r, "channel")
	pause := formValue(r, "pause") == "1"
	label := litellm.ChannelLabel(channel)
	if pause {
		if err := confirmMatches(r, "confirm", channel); err != nil {
			return "", fmt.Errorf("输入渠道代号 %s 确认暂停", channel)
		}
	}
	queued, err := s.dbm.ops.SetChannelPaused(r.Context(), channel, pause, formValue(r, "reason"), formInt(r, "version"), sess.actor, s.clientKey(r))
	if err != nil {
		return "", err
	}
	verb := "已恢复"
	if pause {
		verb = "已暂停"
	}
	logAudit(s.clientKey(r), "%s channel %s", map[bool]string{true: "paused", false: "resumed"}[pause], channel)
	notice := fmt.Sprintf("%s %s；%d 位\"全部模型\"员工的网关权限已排队更新", verb, label, queued)

	// Rewrite everybody's picker now. If the gateway cannot be read, the
	// pause still stands (the exports read it when they run); say so.
	gw, err := s.gateway()
	if err != nil {
		return notice + "。模型清单没有下发：" + err.Error() + "；稍后在\"模型下发\"点全部下发", nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), gatewayModelsTimeout)
	defer cancel()
	models, err := gw.Models(ctx)
	if err != nil {
		return notice + "。模型清单没有下发（读不到网关）；稍后在\"模型下发\"点全部下发", nil
	}
	models, err = s.deliverableModels(r.Context(), models)
	if err != nil {
		return "", err
	}
	_, version, err := s.dbm.ops.CatalogDeliveryState(r.Context())
	if err != nil {
		return "", err
	}
	n, err := s.dbm.ops.DeliverCatalog(r.Context(), ops.SnapshotOf(models), "", version, sess.actor, s.clientKey(r))
	if err != nil {
		if errors.Is(err, repo.ErrConflict) || strings.Contains(err.Error(), "nothing to deliver") {
			return notice + "。模型清单没有下发：" + err.Error(), nil
		}
		return "", err
	}
	return fmt.Sprintf("%s；已给 %d 位员工重新生成模型清单，进度见\"模型下发\"", notice, n), nil
}
