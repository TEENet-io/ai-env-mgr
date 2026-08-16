package adminweb

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxUploadBytes caps a browser upload. The agent binary is ~10 MB and staged
// files are small; the Codex installer is ~700 MB and is fetched by URL
// instead, because pushing it through a browser and a CDN is both slow and
// commonly blocked by an upload limit on the way.
const maxUploadBytes = 64 << 20

// confirmMatches enforces the typed confirmation on an irreversible or
// fleet-wide action.
//
// A checkbox is not enough for these. Publishing makes every machine download
// and run a binary, and forgetting a machine cannot be undone -- typing the
// exact version or hostname is what makes the operator read what they are
// about to do rather than clicking through it.
func confirmMatches(r *http.Request, field, want string) error {
	got := strings.TrimSpace(r.PostFormValue(field))
	if got == "" {
		return fmt.Errorf("type %q to confirm", want)
	}
	if got != want {
		return fmt.Errorf("the confirmation %q does not match %q", got, want)
	}
	return nil
}

// payload returns the bytes to publish: either an upload, or a download the
// server performs itself.
func (s *Server) payload(r *http.Request) ([]byte, error) {
	if url := formValue(r, "url"); url != "" {
		// Same helper the CLI uses, including the token a private GitHub
		// release needs. The token is used for this request and then dropped;
		// it is never stored.
		return downloadFromURL(url, formValue(r, "token"))
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		return nil, fmt.Errorf("give a URL or choose a file")
	}
	defer f.Close()
	if hdr.Size > maxUploadBytes {
		return nil, fmt.Errorf("that file is %d MB; uploads are capped at %d MB, publish it by URL instead",
			hdr.Size>>20, maxUploadBytes>>20)
	}
	return io.ReadAll(io.LimitReader(f, maxUploadBytes+1))
}

// parseUpload accepts a multipart form, keeping most of the body on disk
// rather than in memory.
func parseUpload(r *http.Request) error {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return r.ParseMultipartForm(8 << 20)
	}
	return r.ParseForm()
}

// --- agent ---

func (s *Server) actionAgentPublish(sess *session, r *http.Request) error {
	version := formValue(r, "version")
	if version == "" {
		return fmt.Errorf("a version is required")
	}
	if err := confirmMatches(r, "confirm", version); err != nil {
		return err
	}
	data, err := s.payload(r)
	if err != nil {
		return err
	}
	sum, err := sess.mgr.PublishAgentUpdate(version, data)
	if err != nil {
		return err
	}
	// Worth a line in the log: this is the action that reaches every machine.
	logAudit(s.clientKey(r), "published agent %s (%d bytes, sha256 %s)", version, len(data), sum)
	return nil
}

func (s *Server) actionAgentCancel(sess *session, r *http.Request) error {
	if err := sess.mgr.CancelAgentUpdate(); err != nil {
		return err
	}
	logAudit(s.clientKey(r), "cleared the agent update target")
	return nil
}

// --- codex ---

func (s *Server) actionCodexPublish(sess *session, r *http.Request) error {
	version := formValue(r, "version")
	if version == "" {
		return fmt.Errorf("a version is required")
	}
	if err := confirmMatches(r, "confirm", version); err != nil {
		return err
	}
	rollout := 10
	if v := formValue(r, "rollout"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("the rollout must be a number between 0 and 100")
		}
		rollout = n
	}
	data, err := s.payload(r)
	if err != nil {
		return err
	}
	sum, err := sess.mgr.PublishCodexUpdate(version, data, rollout)
	if err != nil {
		return err
	}
	logAudit(s.clientKey(r), "published Codex %s to %d%% (%d bytes, sha256 %s)", version, rollout, len(data), sum)
	return nil
}

func (s *Server) actionCodexRollout(sess *session, r *http.Request) error {
	pct, err := strconv.Atoi(formValue(r, "rollout"))
	if err != nil {
		return fmt.Errorf("the rollout must be a number between 0 and 100")
	}
	if err := sess.mgr.SetCodexRollout(pct); err != nil {
		return err
	}
	logAudit(s.clientKey(r), "set the Codex rollout to %d%%", pct)
	return nil
}

func (s *Server) actionCodexCancel(sess *session, r *http.Request) error {
	if err := sess.mgr.CancelCodexUpdate(); err != nil {
		return err
	}
	logAudit(s.clientKey(r), "cleared the Codex target")
	return nil
}

// --- machines ---

func (s *Server) actionMachineForget(sess *session, r *http.Request) error {
	machine := formValue(r, "machine")
	if machine == "" {
		return fmt.Errorf("a machine is required")
	}
	if err := confirmMatches(r, "confirm", machine); err != nil {
		return err
	}
	if err := sess.mgr.ForgetMachine(machine); err != nil {
		return err
	}
	logAudit(s.clientKey(r), "forgot machine %s", machine)
	return nil
}

// --- staged files ---

func (s *Server) actionFilePut(sess *session, r *http.Request) error {
	ttl, err := parseHours(formValue(r, "hours"))
	if err != nil {
		return err
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		return fmt.Errorf("choose a file")
	}
	defer f.Close()
	if hdr.Size > maxUploadBytes {
		return fmt.Errorf("that file is %d MB; uploads are capped at %d MB", hdr.Size>>20, maxUploadBytes>>20)
	}
	name := formValue(r, "name")
	if name == "" {
		name = hdr.Filename
	}
	data, err := io.ReadAll(io.LimitReader(f, maxUploadBytes+1))
	if err != nil {
		return err
	}
	if _, err := sess.mgr.PutFile(name, data, ttl); err != nil {
		return err
	}
	logAudit(s.clientKey(r), "staged file %s (%d bytes, link valid %s)", name, len(data), ttl)
	return nil
}

func (s *Server) actionFileRemove(sess *session, r *http.Request) error {
	name := formValue(r, "name")
	if name == "" {
		return fmt.Errorf("a name is required")
	}
	if err := sess.mgr.RemoveFile(name); err != nil {
		return err
	}
	logAudit(s.clientKey(r), "removed staged file %s", name)
	return nil
}

func parseHours(v string) (time.Duration, error) {
	if v == "" {
		return 24 * time.Hour, nil
	}
	h, err := strconv.Atoi(v)
	if err != nil || h <= 0 {
		return 0, fmt.Errorf("the link lifetime must be a positive number of hours")
	}
	return time.Duration(h) * time.Hour, nil
}
