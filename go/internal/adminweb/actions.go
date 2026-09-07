package adminweb

import (
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// requirePost gates a state-changing handler on POST plus a matching CSRF
// token, then redirects back to the page it came from.
//
// The redirect is not cosmetic: it turns the POST into a GET, so a refresh
// cannot replay the action. Several of these change what every machine in the
// fleet does, and repeating one by accident is a real cost.
func (s *Server) requirePost(back string, next func(*session, *http.Request) error) http.HandlerFunc {
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
		if err := next(sess, r); err != nil {
			log.Printf("adminweb: %s: %v", r.URL.Path, err)
			s.redirectWithError(w, r, back, err.Error())
			return
		}
		http.Redirect(w, r, withQuery(back, "ok", "1"), http.StatusSeeOther)
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
	return sess.mgr.BindMachine(machine, user, formValue(r, "note"))
}

func (s *Server) actionMachineUnbind(sess *session, r *http.Request) error {
	machine := formValue(r, "machine")
	if machine == "" {
		return fmt.Errorf("a machine is required")
	}
	return sess.mgr.UnbindMachine(machine)
}

// --- website blocking ---

func (s *Server) actionSites(sess *session, r *http.Request) error {
	add := splitDomains(formValue(r, "add"))
	remove := splitDomains(formValue(r, "remove"))
	if len(add) == 0 && len(remove) == 0 {
		return fmt.Errorf("nothing to add or remove")
	}
	_, err := sess.mgr.MutateDomains(add, remove)
	return err
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

func (s *Server) actionBlockEnabled(sess *session, r *http.Request) error {
	_, err := sess.mgr.SetBlockEnabled(formValue(r, "enabled") == "1")
	return err
}

// --- settings ---

func (s *Server) actionSyncInterval(sess *session, r *http.Request) error {
	minutes, err := strconv.Atoi(formValue(r, "minutes"))
	if err != nil {
		return fmt.Errorf("the interval must be a whole number of minutes")
	}
	_, err = sess.mgr.SetSyncInterval(minutes)
	return err
}

// actionQuotaDefaults saves the quota new accounts are pre-filled with. It
// does not touch any existing account.
func (s *Server) actionQuotaDefaults(sess *session, r *http.Request) error {
	q, err := parseQuotaForm(r.PostForm)
	if err != nil {
		return err
	}
	return sess.mgr.SaveQuotaDefaults(q)
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
	_, err := sess.mgr.SetCollect(enabled, since, quiet)
	return err
}
