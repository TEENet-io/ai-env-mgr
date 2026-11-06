package adminweb

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/ops"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// channelRow is one supplier in the channel operations view.
type channelRow struct {
	ID, Label   string
	Models      []string // the names employees can see, from this channel
	Deployments []channelDeployment
	Testable    bool
	Paused      bool
	Pause       repo.ChannelPause
}

type channelDeployment struct {
	ID, Name    string
	Visible     bool
	Blocked     bool
	ChatCapable bool
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
		chatCapable := m.Info.Mode == "" || strings.EqualFold(m.Info.Mode, "chat")
		row.Deployments = append(row.Deployments, channelDeployment{ID: m.Info.ID, Name: m.Name, Visible: m.Info.CatalogVisible, Blocked: m.Info.Blocked, ChatCapable: chatCapable})
		if m.Info.ID != "" && chatCapable && !m.Info.Blocked {
			row.Testable = true
		}
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

type channelTrialResponse struct {
	OK               bool   `json:"ok"`
	Error            string `json:"error,omitempty"`
	Warning          string `json:"warning,omitempty"`
	Input            string `json:"input,omitempty"`
	Output           string `json:"output,omitempty"`
	ModelName        string `json:"model_name,omitempty"`
	ModelID          string `json:"model_id,omitempty"`
	ElapsedMS        int64  `json:"elapsed_ms,omitempty"`
	PromptTokens     int    `json:"prompt_tokens,omitempty"`
	CompletionTokens int    `json:"completion_tokens,omitempty"`
	FinishReason     string `json:"finish_reason,omitempty"`
}

func writeChannelTrialJSON(w http.ResponseWriter, status int, out channelTrialResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(out)
}

// handleChannelTrial is a real, bounded chat request; it never puts prompts
// or responses in URLs, audit records, or the console log.
func (s *Server) handleChannelTrial(w http.ResponseWriter, r *http.Request, sess *session) {
	if r.Method != http.MethodPost {
		writeChannelTrialJSON(w, http.StatusMethodNotAllowed, channelTrialResponse{Error: "僅支持 POST"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := parseChannelTrialForm(r); err != nil {
		writeChannelTrialJSON(w, http.StatusBadRequest, channelTrialResponse{Error: "無法讀取請求"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.csrf)) != 1 {
		writeChannelTrialJSON(w, http.StatusForbidden, channelTrialResponse{Error: "表單已過期，請刷新頁面重試"})
		return
	}
	channel, modelID, prompt := formValue(r, "channel"), formValue(r, "model_id"), formValue(r, "prompt")
	if channel == "" || modelID == "" || prompt == "" {
		writeChannelTrialJSON(w, http.StatusBadRequest, channelTrialResponse{Error: "請選擇部署並輸入測試內容"})
		return
	}
	if utf8.RuneCountInString(prompt) > 2000 {
		writeChannelTrialJSON(w, http.StatusBadRequest, channelTrialResponse{Error: "測試內容不能超過 2000 個字符"})
		return
	}
	gw, err := s.gateway()
	if err != nil {
		writeChannelTrialJSON(w, http.StatusServiceUnavailable, channelTrialResponse{Error: "網關未配置"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), gatewayModelsTimeout)
	models, err := gw.AllModels(ctx)
	cancel()
	if err != nil {
		writeChannelTrialJSON(w, http.StatusBadGateway, channelTrialResponse{Error: "無法讀取網關部署清單"})
		return
	}
	var target *litellm.Model
	for i := range models {
		if models[i].Info.ID == modelID && litellm.ChannelOf(models[i]) == channel {
			target = &models[i]
			break
		}
	}
	if target == nil {
		writeChannelTrialJSON(w, http.StatusBadRequest, channelTrialResponse{Error: "部署與渠道不匹配，請刷新頁面"})
		return
	}
	if target.Info.Mode != "" && !strings.EqualFold(target.Info.Mode, "chat") {
		writeChannelTrialJSON(w, http.StatusBadRequest, channelTrialResponse{Error: "該部署不是聊天模型，不能使用文本試調用"})
		return
	}
	gw.HTTP.Timeout = 55 * time.Second
	ctx, cancel = context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	disableThinking := channelTrialDisablesThinking(*target)
	trialNote := ""
	if disableThinking {
		trialNote = "本次連通性測試已關閉 Gemini 思考；員工調用仍使用網關預設參數"
	}
	started := time.Now()
	trial, trialErr := gw.TryChat(ctx, modelID, prompt, litellm.ChatTrialOptions{DisableThinking: disableThinking})
	elapsed := time.Since(started)
	result := "ok"
	status := http.StatusOK
	if trialErr != nil {
		result = "error"
		status = http.StatusBadGateway
		var apiErr *litellm.APIError
		if errors.As(trialErr, &apiErr) {
			status = apiErr.Status
		}
	}
	detail, _ := json.Marshal(map[string]any{
		"model_name": target.Name, "model_id": modelID,
		"elapsed_ms": elapsed.Milliseconds(), "http_status": status,
		"prompt_chars":  utf8.RuneCountInString(prompt),
		"prompt_tokens": trial.PromptTokens, "completion_tokens": trial.CompletionTokens,
	})
	_, auditErr := s.dbm.store.Audit().Append(r.Context(), repo.AuditEvent{
		ActorType: repo.ActorAdmin, ActorID: sess.actor, Action: "channel_trial",
		TargetType: "channel", TargetID: channel, Detail: detail, Result: result,
		RequestID: s.clientKey(r),
	})
	if trialErr != nil {
		message := "試調用失敗，請到 LiteLLM 查看該部署日誌"
		if errors.Is(trialErr, litellm.ErrNoVisibleOutput) {
			message = "模型已響應，但未生成可顯示文本"
			if trial.FinishReason == "length" {
				message = "模型已響應，但輸出額度耗盡，未生成正文"
			}
		} else if errors.Is(trialErr, context.DeadlineExceeded) {
			message = "試調用超時，請到 LiteLLM 查看該部署日誌"
		} else if status >= 400 && status < 600 && status != http.StatusBadGateway {
			message = fmt.Sprintf("試調用失敗（HTTP %d），請到 LiteLLM 查看該部署日誌", status)
		}
		out := channelTrialResponse{
			Error: message, Input: prompt, ModelName: target.Name, ModelID: modelID,
			ElapsedMS: elapsed.Milliseconds(), PromptTokens: trial.PromptTokens,
			CompletionTokens: trial.CompletionTokens, FinishReason: trial.FinishReason,
			Warning: trialNote,
		}
		if auditErr != nil {
			if out.Warning != "" {
				out.Warning += "；"
			}
			out.Warning += "本次檢測未能寫入審計，請勿盲目重試，以免重複計費"
		}
		writeChannelTrialJSON(w, http.StatusBadGateway, out)
		return
	}
	out := channelTrialResponse{
		OK: true, Input: prompt, Output: trial.Output, ModelName: target.Name, ModelID: trial.ModelID,
		ElapsedMS: elapsed.Milliseconds(), PromptTokens: trial.PromptTokens,
		CompletionTokens: trial.CompletionTokens, FinishReason: trial.FinishReason,
		Warning: trialNote,
	}
	if auditErr != nil {
		if out.Warning != "" {
			out.Warning += "；"
		}
		out.Warning += "模型已返回，但本次檢測未能寫入審計"
	}
	writeChannelTrialJSON(w, http.StatusOK, out)
}

func channelTrialDisablesThinking(m litellm.Model) bool {
	if litellm.ChannelOf(m) != litellm.ChannelGoogle {
		return false
	}
	model := strings.ToLower(m.Params.Model)
	if i := strings.IndexByte(model, '/'); i >= 0 {
		model = model[i+1:]
	}
	if model == "" {
		model = strings.ToLower(m.Name)
	}
	return strings.HasPrefix(model, "gemini-2.5-flash")
}

func parseChannelTrialForm(r *http.Request) error {
	// Browser FormData uses multipart/form-data. ParseForm alone leaves its
	// fields empty, so the CSRF check would always reject those requests.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return r.ParseMultipartForm(16 << 10)
	}
	return r.ParseForm()
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
