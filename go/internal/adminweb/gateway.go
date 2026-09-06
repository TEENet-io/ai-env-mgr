package adminweb

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// gatewayHolder is one roster entry alongside whatever token the gateway
// holds for them.
//
// The gateway is the authority on tokens, not this console: the roster says
// who should have one, the gateway says who does. Showing both side by side
// is what makes a half-finished onboarding or offboarding visible, rather
// than each side looking fine on its own.
type gatewayHolder struct {
	WindowsUser string
	Enabled     bool     // still employed, per the roster
	HasToken    bool     // the gateway holds a token under this user's alias
	Models      []string // what that token may reach
	Spend       float64
	Orphaned    bool // a live token for someone no longer employed
}

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

	us, err := sess.mgr.LoadUsers()
	if err != nil {
		data.Error = "could not read the roster"
		log.Printf("adminweb: LoadUsers: %v", err)
		s.render(w, "gateway.html", http.StatusOK, data)
		return
	}

	keys, err := gw.ListKeys(ctx)
	if err != nil {
		if data.GatewayUnusable == "" {
			data.GatewayUnusable = "无法读取网关令牌清单：" + err.Error()
		}
		log.Printf("adminweb: gateway keys: %v", err)
	}
	data.GatewayHolders = reconcile(us.Users, keys)

	s.render(w, "gateway.html", http.StatusOK, data)
}

// reconcile pairs the roster against the gateway's tokens.
//
// Two mismatches matter and both are surfaced rather than smoothed over:
// an employee with no token (onboarding never finished, so Codex cannot
// reach a model), and a live token whose alias matches nobody on the roster
// or someone already disabled -- the offboarding case, where the token would
// otherwise keep working indefinitely.
func reconcile(users []model.UserEntry, keys []litellm.Key) []gatewayHolder {
	byAlias := make(map[string]litellm.Key, len(keys))
	for _, k := range keys {
		if k.KeyAlias != "" {
			byAlias[k.KeyAlias] = k
		}
	}

	out := make([]gatewayHolder, 0, len(users))
	claimed := map[string]bool{}
	for _, u := range users {
		alias := admincore.KeyAlias(u.WindowsUser)
		h := gatewayHolder{WindowsUser: u.WindowsUser, Enabled: u.Enabled}
		if k, ok := byAlias[alias]; ok {
			claimed[alias] = true
			h.HasToken = true
			h.Models = k.Models
			h.Spend = k.Spend
			// A live token for someone marked as left is the exact state
			// offboarding is supposed to prevent.
			h.Orphaned = !u.Enabled
		}
		out = append(out, h)
	}

	// Tokens whose alias matches nobody on the roster at all. These are the
	// ones no per-user row would ever show.
	for alias, k := range byAlias {
		if claimed[alias] || !strings.HasPrefix(alias, "emp-") {
			continue
		}
		out = append(out, gatewayHolder{
			WindowsUser: strings.TrimPrefix(alias, "emp-"),
			Enabled:     false,
			HasToken:    true,
			Models:      k.Models,
			Spend:       k.Spend,
			Orphaned:    true,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].WindowsUser < out[j].WindowsUser })
	return out
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

	var budget float64
	if raw := formValue(r, "budget"); raw != "" {
		budget, err = strconv.ParseFloat(raw, 64)
		if err != nil || budget < 0 {
			return fmt.Errorf("预算需要是一个非负数字")
		}
	}

	// No models selected means "everything the gateway offers", which is the
	// common case; an explicit selection narrows it.
	models := r.Form["models"]

	ctx, cancel := context.WithTimeout(r.Context(), gatewayTimeout)
	defer cancel()
	return sess.mgr.ProvisionCodexGateway(ctx, gw, admincore.GatewayConfig{BaseURL: s.opts.GatewayURL}, user, models, budget)
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
