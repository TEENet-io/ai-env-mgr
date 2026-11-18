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
	page := template.Must(template.New("manual").Parse(`<!doctype html><html lang="zh-CN"><meta charset="utf-8"><title>Skill 獨立測試</title><link rel="stylesheet" href="/static/app.css"><body><main class="sheet">
<h1>Skill 獨立測試</h1><p>這是獨立測試後台。此處只測 Skill 派發，不更改現有模型登錄、瀏覽器策略或 AppLocker。</p>
<p><a href="/skills">進入 Skill 管理 →</a>　<a href="/test/download">下載 Windows 測試 Agent</a>　<a href="/overview">機器狀態</a></p>
<p>測試 Skill：<a href="/test/teenet-delivery-check-v1.zip">下載 v1</a>　<a href="/test/teenet-delivery-check-v2.zip">下載 v2（測試更新）</a>。首次選擇“導入 Skill ZIP”，業務名稱填“公司技能下發測試”，英文標識會自動識別爲 <code>teenet-delivery-check</code>；更新時進入該 Skill 的“上傳新版”。</p>
<p>新裝 Codex 尚無模型配置時：<a href="/test/codex-model-setup.zip">下載模型接入包（管理員登錄後可下載）</a>。使用綁定員工賬號解壓，先退出 Codex，再運行包內 setup-codex-skill-test.ps1。此包連接生產模型網關，生成獨立的 Codex Skill Test 桌面快捷方式。</p>
<p><a href="/test/skill-live-refresh.zip">下載無感刷新測試包</a>：先停止舊測試 Agent，關閉 Codex 一次，解壓後運行 install-skill-live-refresh.ps1，再運行包內新 Agent。安裝適配後，更新、回退和撤回不再自動關閉 Codex。<a href="/test/teenet-delivery-check-v3.zip">v3 正文更新測試包</a>。</p>
<h2>1. 在 Windows 上運行</h2><p>下載後，用管理員終端運行以下命令；保留當前線上 Agent 服務。</p><pre>.\agent-skills-test.exe skills-test --console {{.URL}}</pre>
<h2>2. 創建測試員工並綁定電腦</h2><p>Agent 連上後刷新此頁。用戶名填寫員工日常登錄 b13 的 Windows 用戶名，可在該員工終端運行 whoami 查看。</p>
<form method="post" action="/test/bind"><input type="hidden" name="csrf" value="{{.CSRF}}"><label>測試電腦<select name="machine" required>{{range .Devices}}<option>{{.Hostname}}</option>{{end}}</select></label><label>員工 Windows 用戶名<input name="windows_user" required placeholder="例如 weipeng"></label><button>創建並綁定</button></form>
<h2>3. 派發與回收</h2><p>進入 Skill 管理，新增或導入 Skill，再選擇剛綁定的員工。檢查 b13 新會話中的 Skill；隨後測試更新、回退和撤回。新版 Agent 與刷新適配會自動重掃當前桌面進程，不主動中斷任務；後台區分文件同步和等待客戶端刷新。</p>
<p>結束後先在後台撤回；也可在管理員終端執行 <code>.\agent-skills-test.exe skills-test --cleanup</code>，只清理測試 Agent 管理的 Skill。</p>
<h2>已連接的電腦</h2><ul>{{range .Devices}}<li>{{.Hostname}} · {{.AgentVersion}}</li>{{else}}<li>尚未連接；先啓動 Windows 測試 Agent。</li>{{end}}</ul>
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
			return "", errors.New("請填寫普通員工的 Windows 用戶名")
		}
		e, err := s.dbm.store.Employees().ByWindowsUser(r.Context(), user)
		if errors.Is(err, repo.ErrNotFound) {
			e, err = s.dbm.store.Employees().Create(r.Context(), repo.NewEmployee{WindowsUser: user, Name: user, Department: "Skill 測試"})
		}
		if err != nil {
			return "", err
		}
		_, err = s.dbm.ops.BindMachine(r.Context(), r.FormValue("machine"), e.ID, "isolated skill test", sess.actor, "manual-test")
		return "已綁定，可以進入 Skill 管理派發", err
	}))
	mux.HandleFunc("/test/download", func(w http.ResponseWriter, r *http.Request) {
		if agentFile == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="agent-skills-test.exe"`)
		http.ServeFile(w, r, filepath.Clean(agentFile))
	})
	for _, version := range []string{"v1", "v2", "v3"} {
		name := "teenet-delivery-check-" + version + ".zip"
		mux.HandleFunc("/test/"+name, func(w http.ResponseWriter, r *http.Request) {
			if agentFile == "" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
			w.Header().Set("Cache-Control", "no-store")
			http.ServeFile(w, r, filepath.Join(filepath.Dir(agentFile), name))
		})
	}
	for _, name := range []string{"codex-model-setup.zip", "skill-live-refresh.zip"} {
		mux.HandleFunc("/test/"+name, s.requireSession(func(w http.ResponseWriter, r *http.Request, sess *session) {
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if agentFile == "" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
			w.Header().Set("Cache-Control", "no-store, private")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			http.ServeFile(w, r, filepath.Join(filepath.Dir(agentFile), name))
		}))
	}

	mux.Handle("/", s.Handler())
	return mux
}
