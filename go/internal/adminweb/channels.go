package adminweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/ops"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// channelRow is one supplier in the channel operations view.
type channelRow struct {
	ID, Label   string
	Models      []string // the names employees can see, from this channel
	Deployments []channelDeployment
	Paused      bool
	Pause       repo.ChannelPause
}

type channelDeployment struct {
	ID, Name string
	Visible  bool
	Blocked  bool
}

// channelOrder puts the suppliers in a fixed order; unknown ones last.
var channelOrder = map[string]int{litellm.ChannelAWS: 1, litellm.ChannelGoogle: 2, litellm.ChannelAzure: 3, litellm.ChannelOpenAI: 4}

// loadChannels groups the gateway's visible models by supplier and marks
// the paused ones. Database mode only: the pause lives in its settings.
func (s *Server) loadChannels(ctx context.Context, data *pageData) {
	s.loadChannelsFrom(ctx, data, data.GatewayModels)
}

func (s *Server) loadChannelsFrom(ctx context.Context, data *pageData, models []litellm.Model) {
	if s.dbm == nil {
		return
	}
	state, version, err := repo.LoadGatewayChannels(ctx, s.dbm.store.Settings())
	if err != nil {
		data.Error = "could not read the channels: " + err.Error()
		return
	}
	byID := map[string]*channelRow{}
	for _, m := range models {
		id := litellm.ChannelOf(m)
		row, ok := byID[id]
		if !ok {
			row = &channelRow{ID: id, Label: litellm.ChannelLabel(id)}
			byID[id] = row
		}
		row.Deployments = append(row.Deployments, channelDeployment{ID: m.Info.ID, Name: m.Name, Visible: m.Info.CatalogVisible, Blocked: m.Info.Blocked})
		if m.Info.CatalogVisible && !m.Info.Blocked {
			row.Models = append(row.Models, m.Name)
		}
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
		sort.Slice(row.Deployments, func(i, j int) bool {
			if row.Deployments[i].Name != row.Deployments[j].Name {
				return row.Deployments[i].Name < row.Deployments[j].Name
			}
			return row.Deployments[i].ID < row.Deployments[j].ID
		})
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

// handleChannels is the top-level supplier operations page. Admins see every
// deployment, including backups omitted from the employee model picker.
func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "channels")
	data.GatewayURL = s.opts.GatewayURL
	gw, err := s.gateway()
	if err != nil {
		data.GatewayUnusable = err.Error()
	} else {
		data.GatewayEnabled = true
		ctx, cancel := context.WithTimeout(r.Context(), gatewayModelsTimeout)
		defer cancel()
		models, err := gw.AllModels(ctx)
		if err != nil {
			data.GatewayUnusable = "無法讀取網關部署清單：" + err.Error()
		} else {
			if sess.admin.Role != repo.RoleAdmin {
				visible := models[:0:0]
				for _, m := range models {
					if m.Info.CatalogVisible && !m.Info.Blocked {
						visible = append(visible, m)
					}
				}
				models = visible
			}
			s.loadChannelsFrom(r.Context(), &data, models)
		}
	}
	if len(data.GatewayChannels) == 0 {
		s.loadChannelsFrom(r.Context(), &data, nil)
	}
	s.render(w, "channels.html", http.StatusOK, data)
}

// actionChannelTest checks one exact deployment. A model name is deliberately
// insufficient: the same alias may route through multiple suppliers.
func (s *Server) actionChannelTest(sess *session, r *http.Request) (string, error) {
	channel := formValue(r, "channel")
	modelID := formValue(r, "model_id")
	if channel == "" || modelID == "" {
		return "", fmt.Errorf("請選擇要測試的部署")
	}
	gw, err := s.gateway()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(r.Context(), gatewayModelsTimeout)
	models, err := gw.AllModels(ctx)
	cancel()
	if err != nil {
		return "", fmt.Errorf("無法讀取網關部署清單：%w", err)
	}
	var target *litellm.Model
	for i := range models {
		if models[i].Info.ID == modelID && litellm.ChannelOf(models[i]) == channel {
			target = &models[i]
			break
		}
	}
	if target == nil {
		return "", fmt.Errorf("該部署不屬於所選渠道，請刷新頁面")
	}
	ctx, cancel = context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	started := time.Now()
	result, err := gw.CheckDeployment(ctx, modelID)
	label := litellm.ChannelLabel(channel) + " · " + target.Name
	if err != nil {
		logAudit(s.clientKey(r), "channel test failed %s %s", channel, modelID)
		var apiErr *litellm.APIError
		status := 0
		if errors.As(err, &apiErr) {
			status = apiErr.Status
		}
		if auditErr := s.recordChannelTest(sess, r, channel, *target, "error", time.Since(started), status); auditErr != nil {
			return "", fmt.Errorf("渠道檢測失敗，且無法保存審計記錄：%w", auditErr)
		}
		if status != 0 {
			return "", fmt.Errorf("%s 檢測請求失敗（HTTP %d）", label, apiErr.Status)
		}
		return "", fmt.Errorf("%s 檢測請求失敗：%w", label, err)
	}
	if result.HealthyCount == 0 {
		logAudit(s.clientKey(r), "channel test unhealthy %s %s", channel, modelID)
		if err := s.recordChannelTest(sess, r, channel, *target, "failed", time.Since(started), 503); err != nil {
			return "", fmt.Errorf("渠道檢測未通過，且無法保存審計記錄：%w", err)
		}
		return "", fmt.Errorf("%s 檢測未通過；請到 LiteLLM 查看該部署的錯誤詳情", label)
	}
	logAudit(s.clientKey(r), "channel test healthy %s %s", channel, modelID)
	if err := s.recordChannelTest(sess, r, channel, *target, "ok", time.Since(started), 200); err != nil {
		return "", fmt.Errorf("渠道檢測通過，但無法保存審計記錄：%w", err)
	}
	return fmt.Sprintf("%s 健康檢查通過（%.1f 秒）；若網關啟用後台檢查，此結果可能來自緩存", label, time.Since(started).Seconds()), nil
}

func (s *Server) recordChannelTest(sess *session, r *http.Request, channel string, model litellm.Model, result string, elapsed time.Duration, status int) error {
	detail, err := json.Marshal(map[string]any{
		"model_name":  model.Name,
		"model_id":    model.Info.ID,
		"elapsed_ms":  elapsed.Milliseconds(),
		"http_status": status,
	})
	if err != nil {
		return err
	}
	_, err = s.dbm.store.Audit().Append(r.Context(), repo.AuditEvent{
		ActorType:  repo.ActorAdmin,
		ActorID:    sess.actor,
		Action:     "channel_test",
		TargetType: "channel",
		TargetID:   channel,
		Detail:     detail,
		Result:     result,
		RequestID:  s.clientKey(r),
	})
	return err
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
			return "", fmt.Errorf("輸入渠道代號 %s 確認暫停", channel)
		}
	}
	queued, err := s.dbm.ops.SetChannelPaused(r.Context(), channel, pause, formValue(r, "reason"), formInt(r, "version"), sess.actor, s.clientKey(r))
	if err != nil {
		return "", err
	}
	verb := "已恢復"
	if pause {
		verb = "已暫停"
	}
	logAudit(s.clientKey(r), "%s channel %s", map[bool]string{true: "paused", false: "resumed"}[pause], channel)
	notice := fmt.Sprintf("%s %s；%d 位\"全部模型\"員工的網關權限已排隊更新", verb, label, queued)

	// Rewrite everybody's picker now. If the gateway cannot be read, the
	// pause still stands (the exports read it when they run); say so.
	gw, err := s.gateway()
	if err != nil {
		return notice + "。模型清單沒有下發：" + err.Error() + "；稍後在\"模型下發\"點全部下發", nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), gatewayModelsTimeout)
	defer cancel()
	models, err := gw.Models(ctx)
	if err != nil {
		return notice + "。模型清單沒有下發（讀不到網關）；稍後在\"模型下發\"點全部下發", nil
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
			return notice + "。模型清單沒有下發：" + err.Error(), nil
		}
		return "", err
	}
	return fmt.Sprintf("%s；已給 %d 位員工重新生成模型清單，進度見\"模型下發\"", notice, n), nil
}

// handleModels is 员工账号 › 模型下发: the gateway's visible models and
// the delivery that puts them in employee pickers.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "users")
	data.Tab = "models"
	s.loadGatewayPanel(r.Context(), &data)
	s.loadChannels(r.Context(), &data)
	s.loadModelDelivery(r.Context(), &data)
	s.render(w, "models.html", http.StatusOK, data)
}
