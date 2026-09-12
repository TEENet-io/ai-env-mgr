package adminweb

import (
	"context"
	"fmt"
	"log"
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

// gatewayModelsTimeout is the shorter bound for the overview's model list.
//
// That list is one panel on a page with three of them, and it is the only one
// whose answer comes from another cloud. Twenty seconds is right for an action
// an operator started and is watching; for a read that merely decorates the
// front page it is long enough to make the whole console feel broken, so the
// panel gives up quickly and says the gateway is unreachable.
const gatewayModelsTimeout = 3 * time.Second

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

// loadGatewayPanel fills the overview's gateway panel.
//
// Every failure is reported on the page rather than to the caller: knowing the
// gateway is unreachable is more useful than an error page, and the fleet and
// policy sections above it are still worth showing.
func (s *Server) loadGatewayPanel(ctx context.Context, data *pageData) {
	data.GatewayURL = s.opts.GatewayURL

	gw, err := s.gateway()
	if err != nil {
		data.GatewayUnusable = err.Error()
		return
	}
	data.GatewayEnabled = true

	mctx, cancel := context.WithTimeout(ctx, gatewayModelsTimeout)
	defer cancel()

	models, err := gw.Models(mctx)
	if err != nil {
		data.GatewayUnusable = "无法读取网关模型清单：" + err.Error()
		log.Printf("adminweb: gateway models: %v", err)
	}
	data.GatewayModels = models
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
