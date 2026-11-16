package adminweb

// This opt-in rig serves the real console and device API against a dedicated
// local database. It exists only in the test binary, never in admin releases.
import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/config"
	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type skillsManualBucket struct{ *fakeStore }

func (skillsManualBucket) Put(string, []byte) error {
	return errors.New("object writes are disabled in the skill test console")
}
func (skillsManualBucket) Delete(string) error {
	return errors.New("object writes are disabled in the skill test console")
}
func (skillsManualBucket) PutFile(string, string, func(int64, int64)) error {
	return errors.New("object writes are disabled in the skill test console")
}
func (skillsManualBucket) Copy(string, string) error {
	return errors.New("object writes are disabled in the skill test console")
}

func TestManualSkillsConsole(t *testing.T) {
	if os.Getenv("SKILLS_MANUAL_CONSOLE") != "on" {
		t.Skip("opt-in interactive skill test console")
	}
	dsn := os.Getenv("SKILLS_MANUAL_PG_DSN")
	if dsn == "" {
		t.Fatal("SKILLS_MANUAL_PG_DSN is required")
	}
	ctx := context.Background()
	// Fixed, separate database; unlike integration helpers this never resets a
	// schema and cannot select the production database supplied by the caller.
	dsn, err := dbstore.TestDatabaseDSN(ctx, dsn, "aienv_skills_manual")
	if err != nil {
		t.Fatal(err)
	}
	listen := os.Getenv("SKILLS_MANUAL_LISTEN")
	if listen == "" {
		listen = "127.0.0.1:18864"
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		t.Fatal("manual console must listen on a loopback IP")
	}
	keyFile := os.Getenv("SKILLS_MANUAL_KEY_FILE")
	if keyFile == "" {
		t.Fatal("SKILLS_MANUAL_KEY_FILE is required")
	}
	objects := skillsManualBucket{newFakeStore()}
	s, err := NewWithStore(Options{Listen: listen, BehindProxy: true, Version: "skills-test", Bucket: "test-only", Endpoint: "http://127.0.0.1:1", PublicHost: os.Getenv("SKILLS_MANUAL_PUBLIC_URL"), Database: &DatabaseOptions{DSN: dsn, MasterKeyFile: keyFile, OSSAccessKeyID: "test-only", OSSAccessKeySecret: "test-only", Worker: false}}, func(config.Config) (store, error) { return objects, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer s.dbm.db.Close()
	if _, err := s.dbm.store.Policies().Current(ctx); errors.Is(err, repo.ErrNotFound) {
		data, _ := json.Marshal(model.Policy{SyncIntervalMinutes: 5})
		if _, err := s.dbm.store.Policies().Publish(ctx, data, "skill test only", "test-rig"); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	handler := skillsManualHandler(s, os.Getenv("SKILLS_MANUAL_PUBLIC_URL"), os.Getenv("SKILLS_MANUAL_AGENT_FILE"))
	server := &http.Server{Addr: listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	t.Logf("manual skill console listening on %s; credentials beside %s", listen, keyFile)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
}

func skillsManualHandler(s *Server, origin, agentFile string) http.Handler {
	page := template.Must(template.New("manual").Parse(`<!doctype html><html lang="zh-CN"><meta charset="utf-8"><title>Skill 独立测试</title><link rel="stylesheet" href="/static/app.css"><body><main class="sheet">
<h1>Skill 独立测试</h1><p>这是独立测试后台。此处只测 Skill 派发，不更改现有模型登录、浏览器策略或 AppLocker。</p>
<p><a href="/skills">进入 Skill 管理 →</a>　<a href="/test/download">下载 Windows 测试 Agent</a>　<a href="/overview">机器状态</a></p>
<h2>1. 在 Windows 上运行</h2><p>下载后，用管理员终端运行以下命令；保留当前线上 Agent 服务。</p><pre>.\agent-skills-test.exe skills-test --console {{.URL}}</pre>
<h2>2. 创建测试员工并绑定电脑</h2><p>Agent 连上后刷新此页。用户名填写员工日常登录 b13 的 Windows 用户名，可在该员工终端运行 whoami 查看。</p>
<form method="post" action="/test/bind"><input type="hidden" name="csrf" value="{{.CSRF}}"><label>测试电脑<select name="machine" required>{{range .Devices}}<option>{{.Hostname}}</option>{{end}}</select></label><label>员工 Windows 用户名<input name="windows_user" required placeholder="例如 weipeng"></label><button>创建并绑定</button></form>
<h2>3. 派发与回收</h2><p>进入 Skill 管理，新增或导入 Skill，再选择刚绑定的员工。检查 b13 新会话中的 Skill；随后测试更新、回退和撤回。更新或撤回会关闭相关员工的 Codex，需要重新打开。</p>
<p>结束后先在后台撤回；也可在管理员终端执行 <code>.\agent-skills-test.exe skills-test --cleanup</code>，只清理测试 Agent 管理的 Skill。</p>
<h2>已连接的电脑</h2><ul>{{range .Devices}}<li>{{.Hostname}} · {{.AgentVersion}}</li>{{else}}<li>尚未连接；先启动 Windows 测试 Agent。</li>{{end}}</ul>
</main></body></html>`))
	mux := http.NewServeMux()
	mux.HandleFunc("/test", s.requireSession(func(w http.ResponseWriter, r *http.Request, sess *session) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		devices, err := s.dbm.store.Devices().List(r.Context(), repo.DeviceFilter{})
		if err != nil {
			http.Error(w, "read test devices failed", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := page.Execute(w, struct {
			URL, CSRF string
			Devices   []repo.Device
		}{origin, sess.csrf, devices}); err != nil {
			s.events.Ops("error", "manual_skill_page", "render test setup failed", map[string]any{"error": err.Error()})
		}
	}))
	mux.HandleFunc("/test/bind", s.requirePostNotice("/test", func(sess *session, r *http.Request) (string, error) {
		user := strings.TrimSpace(r.FormValue("windows_user"))
		if !regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`).MatchString(user) || strings.EqualFold(user, "administrator") {
			return "", errors.New("请填写普通员工的 Windows 用户名")
		}
		e, err := s.dbm.store.Employees().ByWindowsUser(r.Context(), user)
		if errors.Is(err, repo.ErrNotFound) {
			e, err = s.dbm.store.Employees().Create(r.Context(), repo.NewEmployee{WindowsUser: user, Name: user, Department: "Skill 测试"})
		}
		if err != nil {
			return "", err
		}
		_, err = s.dbm.ops.BindMachine(r.Context(), r.FormValue("machine"), e.ID, "isolated skill test", sess.actor, "manual-test")
		return "已绑定，可以进入 Skill 管理派发", err
	}))
	mux.HandleFunc("/test/download", func(w http.ResponseWriter, r *http.Request) {
		if agentFile == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="agent-skills-test.exe"`)
		http.ServeFile(w, r, filepath.Clean(agentFile))
	})
	mux.Handle("/", s.Handler())
	return mux
}
