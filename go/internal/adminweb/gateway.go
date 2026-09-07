package adminweb

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

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
