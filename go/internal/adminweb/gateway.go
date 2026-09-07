package adminweb

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
)

// gatewayTimeout bounds the page's calls to the gateway.
//
// The gateway sits in another cloud, so a hang is entirely possible; without
// a bound the console would appear frozen rather than reporting that the
// gateway is unreachable.
const gatewayTimeout = 20 * time.Second

// gateway builds a client for this deployment's gateway, or explains why it
// cannot.
//
// A missing key is a deployment mistake, not a user error, so the message
// says what to set rather than merely refusing.
func (s *Server) gateway() (*litellm.Client, error) {
	if s.opts.GatewayURL == "" {
		return nil, fmt.Errorf("网关地址未配置（构建时未设置 gatewayURL）")
	}
	if s.opts.GatewayAdminKey == "" {
		return nil, fmt.Errorf("网关管理密钥未注入（设置环境变量 AIENVMGR_GATEWAY_ADMIN_KEY）")
	}
	return litellm.New(s.opts.GatewayURL, s.opts.GatewayAdminKey), nil
}

func (s *Server) handleGateway(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "gateway")
	data.GatewayURL = s.opts.GatewayURL

	gw, err := s.gateway()
	if err != nil {
		data.GatewayUnusable = err.Error()
		s.render(w, "gateway.html", http.StatusOK, data)
		return
	}
	data.GatewayEnabled = true

	ctx, cancel := context.WithTimeout(r.Context(), gatewayTimeout)
	defer cancel()

	models, err := gw.Models(ctx)
	if err != nil {
		// Report it on the page instead of failing the request: knowing the
		// gateway is unreachable is more useful than an error page, and the
		// roster half below is still worth showing.
		data.GatewayUnusable = "无法读取网关模型清单：" + err.Error()
		log.Printf("adminweb: gateway models: %v", err)
	}
	data.GatewayModels = models

	s.render(w, "gateway.html", http.StatusOK, data)
}

// actionGatewayProvision issues or re-issues one employee's token and
// delivers the Codex configuration that uses it.
func (s *Server) actionGatewayProvision(sess *session, r *http.Request) error {
	gw, err := s.gateway()
	if err != nil {
		return err
	}
	user := formValue(r, "windowsUser")
	if user == "" {
		return fmt.Errorf("a Windows user name is required")
	}

	// No models selected means "everything the gateway offers", which is the
	// common case; an explicit selection narrows it.
	models := r.Form["models"]

	ctx, cancel := context.WithTimeout(r.Context(), gatewayTimeout)
	defer cancel()
	return sess.mgr.ProvisionCodexGateway(ctx, gw, admincore.GatewayConfig{BaseURL: s.opts.GatewayURL}, user, models)
}

// actionGatewayRevoke withdraws one employee's token.
//
// It does not touch the delivered files: clearing those is the agent's job,
// driven by the roster. Revocation is the half that works whether or not the
// machine is ever seen again.
func (s *Server) actionGatewayRevoke(sess *session, r *http.Request) error {
	gw, err := s.gateway()
	if err != nil {
		return err
	}
	user := formValue(r, "windowsUser")
	if user == "" {
		return fmt.Errorf("a Windows user name is required")
	}

	ctx, cancel := context.WithTimeout(r.Context(), gatewayTimeout)
	defer cancel()
	return sess.mgr.RevokeCodexGateway(ctx, gw, user)
}

// contextWindowLabel renders a context window the way the model vendors
// quote it. The raw token count is what the gateway reports, but a column of
// six- and seven-digit numbers is read by counting digits; "256K" is not.
func contextWindowLabel(tokens int) string {
	switch {
	case tokens <= 0:
		return "—"
	case tokens >= 1<<20 && tokens%(1<<20) == 0:
		return strconv.Itoa(tokens/(1<<20)) + "M"
	case tokens >= 1000:
		return strconv.Itoa(tokens/1000) + "K"
	default:
		return strconv.Itoa(tokens)
	}
}
