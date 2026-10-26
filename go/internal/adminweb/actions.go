package adminweb

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ops"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// requirePost gates a state-changing handler on POST plus a matching CSRF
// token, then redirects back to the page it came from.
//
// The redirect is not cosmetic: it turns the POST into a GET, so a refresh
// cannot replay the action. Several of these change what every machine in the
// fleet does, and repeating one by accident is a real cost.
func (s *Server) requirePost(back string, next func(*session, *http.Request) error) http.HandlerFunc {
	return s.requirePostNotice(back, func(sess *session, r *http.Request) (string, error) {
		return "", next(sess, r)
	})
}

// requirePostNotice is requirePost for an action whose result needs more
// saying than "已保存": the handler returns the line to show, and it is
// carried through the redirect in the query string like an error is.
//
// It exists for actions that do not take effect when the button is pressed.
// "Saved" is a lie for a request an agent will pick up minutes from now, and
// an administrator who is not told that goes looking for a change that cannot
// have happened yet.
func (s *Server) requirePostNotice(back string, next func(*session, *http.Request) (string, error)) http.HandlerFunc {
	return s.requireSession(func(w http.ResponseWriter, r *http.Request, sess *session) {
		if r.Method != http.MethodPost {
			http.Redirect(w, r, back, http.StatusSeeOther)
			return
		}
		// parseUpload handles both a plain form and a multipart upload, so a
		// file-carrying POST still has its CSRF token parsed before the check.
		if err := parseUpload(r); err != nil {
			s.redirectWithError(w, r, back, "could not read the form")
			return
		}
		// Constant-time so a token cannot be recovered by timing the compare.
		if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.csrf)) != 1 {
			log.Printf("adminweb: rejected a POST to %s with a bad CSRF token from %s", r.URL.Path, s.clientKey(r))
			s.redirectWithError(w, r, back, "the form expired; reload the page and try again")
			return
		}
		notice, err := next(sess, r)
		if err != nil {
			log.Printf("adminweb: %s: %v", r.URL.Path, err)
			s.redirectWithError(w, r, back, err.Error())
			return
		}
		dest := withQuery(back, "ok", "1")
		if notice != "" {
			dest = withQuery(dest, "msg", notice)
		}
		http.Redirect(w, r, dest, http.StatusSeeOther)
	})
}

// requirePostBack is requirePost with the return page chosen by the form
// (see backToAccount): the same action is posted from the list and from a
// detail page, and each should land back where it started.
func (s *Server) requirePostBack(back func(*http.Request) string, next func(*session, *http.Request) error) http.HandlerFunc {
	return s.requireSession(func(w http.ResponseWriter, r *http.Request, sess *session) {
		if r.Method != http.MethodPost {
			http.Redirect(w, r, "/users", http.StatusSeeOther)
			return
		}
		if err := parseUpload(r); err != nil {
			s.redirectWithError(w, r, "/users", "could not read the form")
			return
		}
		dest := back(r)
		if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.csrf)) != 1 {
			log.Printf("adminweb: rejected a POST to %s with a bad CSRF token from %s", r.URL.Path, s.clientKey(r))
			s.redirectWithError(w, r, dest, "the form expired; reload the page and try again")
			return
		}
		if err := next(sess, r); err != nil {
			log.Printf("adminweb: %s: %v", r.URL.Path, err)
			s.redirectWithError(w, r, dest, err.Error())
			return
		}
		http.Redirect(w, r, withQuery(dest, "ok", "1"), http.StatusSeeOther)
	})
}

// withQuery appends a key=value pair to a redirect target, using "&" instead
// of "?" when the target already carries a query string (e.g.
// "/users/detail?user=alice"). Getting this wrong turns the appended pair
// into part of the previous parameter's value instead of a new one -- see
// redirectWithError.
func withQuery(dest, key, value string) string {
	sep := "?"
	if strings.Contains(dest, "?") {
		sep = "&"
	}
	return dest + sep + key + "=" + url.QueryEscape(value)
}

// redirectWithError carries a message through the redirect in the query
// string. Nothing here is secret -- these are validation messages, never the
// credentials themselves.
func (s *Server) redirectWithError(w http.ResponseWriter, r *http.Request, back, msg string) {
	http.Redirect(w, r, withQuery(back, "err", msg), http.StatusSeeOther)
}

func formValue(r *http.Request, name string) string {
	return strings.TrimSpace(r.PostFormValue(name))
}

// --- machines ---

func (s *Server) actionMachineBind(sess *session, r *http.Request) error {
	machine, user := formValue(r, "machine"), formValue(r, "user")
	if machine == "" || user == "" {
		return fmt.Errorf("both a machine and a user are required")
	}
	return sess.be.BindMachine(r.Context(), machine, user, formValue(r, "note"))
}

// actionMachineRestartCodex asks one machine's agent to end its employee's
// Codex on its next sync.
//
// The notice names the interval because nothing visible happens when the
// button is pressed: the request sits in the binding until the agent next
// reads it. Without the number, an administrator watching the row for a
// change would conclude the button was broken.
func (s *Server) actionMachineRestartCodex(sess *session, r *http.Request) (string, error) {
	machine := formValue(r, "machine")
	if machine == "" {
		return "", fmt.Errorf("a machine is required")
	}
	if err := sess.be.RequestCodexRestart(r.Context(), machine); err != nil {
		return "", err
	}
	minutes := model.DefaultSyncInterval
	if p, err := sess.be.Policy(r.Context()); err == nil {
		minutes = model.ClampInterval(p.SyncIntervalMinutes, model.DefaultSyncInterval)
	}
	return fmt.Sprintf("已下发，Agent 下个同步周期执行（当前间隔 %d 分钟）", minutes), nil
}

func (s *Server) actionMachineSync(sess *session, r *http.Request) (string, error) {
	machine := formValue(r, "machine")
	if machine == "" {
		return "", fmt.Errorf("a machine is required")
	}
	if err := sess.be.RequestSync(r.Context(), machine); err != nil {
		return "", err
	}
	return "已请求；agent 1.2.16 起每分钟检查一次，一分钟内开始同步。更早的 agent 仍按同步间隔执行", nil
}

func (s *Server) actionMachineUnbind(sess *session, r *http.Request) error {
	machine := formValue(r, "machine")
	if machine == "" {
		return fmt.Errorf("a machine is required")
	}
	return sess.be.UnbindMachine(r.Context(), machine)
}

// --- website blocking ---

func (s *Server) actionSites(sess *session, r *http.Request) error {
	add := splitDomains(formValue(r, "add"))
	remove := splitDomains(formValue(r, "remove"))
	if len(add) == 0 && len(remove) == 0 {
		return fmt.Errorf("nothing to add or remove")
	}
	return sess.be.MutateDomains(r.Context(), add, remove)
}

// splitDomains accepts the several separators an operator might paste in:
// newlines from a list, commas from a spreadsheet, spaces from typing.
func splitDomains(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ' ' || r == '\t' || r == ';'
	})
	var out []string
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// splitPaths splits on line and list separators only; Windows paths may
// contain spaces, so unlike domains a space is not a separator.
func splitPaths(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' || r == ',' || r == ';' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func (s *Server) actionAppLocker(sess *session, r *http.Request) error {
	add, remove := splitPaths(formValue(r, "add")), splitPaths(formValue(r, "remove"))
	if len(add) == 0 && len(remove) == 0 {
		return fmt.Errorf("nothing to add or remove")
	}
	return sess.be.MutateAppLockerAllowPaths(r.Context(), add, remove)
}

// actionAppLockerMode switches AppLocker enforcement on every machine. The
// value is passed through unchanged so the manager's validation is the only
// gate; a spelling it does not know is refused rather than guessed at.
func (s *Server) actionAppLockerMode(sess *session, r *http.Request) error {
	return sess.be.SetAppLockerMode(r.Context(), formValue(r, "mode"))
}

func (s *Server) actionBlockEnabled(sess *session, r *http.Request) error {
	return sess.be.SetBlockEnabled(r.Context(), formValue(r, "enabled") == "1")
}

// --- settings ---

func (s *Server) actionSyncInterval(sess *session, r *http.Request) error {
	minutes, err := strconv.Atoi(formValue(r, "minutes"))
	if err != nil {
		return fmt.Errorf("the interval must be a whole number of minutes")
	}
	return sess.be.SetSyncInterval(r.Context(), minutes)
}

// actionQuotaDefaults saves the quota new accounts are pre-filled with. It
// does not touch any existing account.
func (s *Server) actionQuotaDefaults(sess *session, r *http.Request) error {
	q, err := parseQuotaForm(r.PostForm)
	if err != nil {
		return err
	}
	return sess.be.SaveQuotaDefaults(r.Context(), q)
}

func (s *Server) actionCollect(sess *session, r *http.Request) error {
	enabled := formValue(r, "enabled") == "1"
	var since *string
	var quiet *int
	if v := formValue(r, "since"); v != "" {
		since = &v
	}
	if v := formValue(r, "quiet"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("the quiet period must be a whole number of seconds")
		}
		quiet = &n
	}
	return sess.be.SetCollect(r.Context(), enabled, since, quiet)
}

// actionCollectCleanup removes only collected session objects older than the
// explicitly confirmed cutoff. Policy, status, credentials and installers
// are different prefixes and can never be touched by this operation.
func (s *Server) actionCollectCleanup(sess *session, r *http.Request) error {
	if s.dbm == nil {
		return fmt.Errorf("采集数据清理需要数据库模式")
	}
	days, err := strconv.Atoi(formValue(r, "days"))
	if err != nil || days < 1 || days > 3650 {
		return fmt.Errorf("保留天数必须在 1 到 3650 天之间")
	}
	if err := confirmMatches(r, "confirm", fmt.Sprintf("DELETE %d", days)); err != nil {
		return err
	}
	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	return s.jobs.start("data_cleanup", fmt.Sprintf("%dd", days), func(setStep func(string), setProgress func(done, total int64)) error {
		infos, err := s.dbm.objects.ListInfo(ossclient.Root)
		if err != nil {
			return err
		}
		var candidates []ossclient.ObjectInfo
		for _, info := range infos {
			if _, _, ok := ossclient.DataCollectUser(info.Key); ok && info.LastModified.Before(cutoff) {
				candidates = append(candidates, info)
			}
		}
		setStep(fmt.Sprintf("删除 %d 个过期采集文件", len(candidates)))
		setProgress(0, int64(len(candidates)))
		for i, info := range candidates {
			if err := s.dbm.objects.Delete(info.Key); err != nil {
				return fmt.Errorf("delete %s: %w", info.Key, err)
			}
			setProgress(int64(i+1), int64(len(candidates)))
		}
		logAudit(s.clientKey(r), "deleted %d collected objects older than %d days", len(candidates), days)
		return nil
	})
}

func (s *Server) actionDataRetention(sess *session, r *http.Request) error {
	if s.dbm == nil {
		return fmt.Errorf("数据保留策略需要数据库模式")
	}
	before, version, err := repo.LoadDataRetentionSettings(r.Context(), s.dbm.store.Settings())
	if err != nil {
		return err
	}
	after := repo.DataRetentionSettings{Enabled: formValue(r, "enabled") == "1", Days: formInt(r, "days")}
	if err := after.Validate(); err != nil {
		return err
	}
	if formInt(r, "version") != version {
		return errors.New("这页的设置已被别人修改，请刷新后重试")
	}
	value, _ := json.Marshal(after)
	return s.dbm.ops.SaveSetting(r.Context(), repo.SettingDataRetention, value, version, ops.ActionDataRetention, before, after, sess.actor, s.clientKey(r))
}
