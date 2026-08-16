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

// releaseToken reads the credential for fetching a private release asset from
// the form, and from nowhere else.
//
// The CLI accepts GITHUB_TOKEN from its environment, which suits a tool run on
// the administrator's own machine. This console is reachable from the internet
// and deliberately holds nothing at rest -- the OSS AccessKey lives in session
// memory for exactly that reason -- so a token parked in its environment would
// be the one secret on that disk, and would quietly turn "an administrator
// entered this" into "the server had it all along". Typed per publish, it
// exists for one request and is gone.
func releaseToken(r *http.Request) string {
	return formValue(r, "token")
}

// payloadSource validates the request now and returns a closure that produces
// the bytes later.
//
// The split matters: a browser upload lives in the request body, which is torn
// down when the handler returns, so it has to be read here. A URL is fetched
// by the background job instead, which is the whole point -- that is the part
// that takes minutes.
func (s *Server) payloadSource(r *http.Request) (func(onProgress func(done, total int64)) ([]byte, error), error) {
	if url := formValue(r, "url"); url != "" {
		// The token is used for that one fetch and then dropped; it is never
		// stored.
		token := releaseToken(r)
		return func(onProgress func(done, total int64)) ([]byte, error) {
			return downloadFromURL(url, token, onProgress)
		}, nil
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
	data, err := io.ReadAll(io.LimitReader(f, maxUploadBytes+1))
	if err != nil {
		return nil, err
	}
	// Already in hand, so the bar for this step is simply complete.
	return func(onProgress func(done, total int64)) ([]byte, error) {
		if onProgress != nil {
			onProgress(int64(len(data)), int64(len(data)))
		}
		return data, nil
	}, nil
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
	// Read the upload before returning: the request body is gone once this
	// handler does, so it cannot be deferred to the background job.
	fetch, err := s.payloadSource(r)
	if err != nil {
		return err
	}
	mgr, client := sess.mgr, s.clientKey(r)
	return s.jobs.start("agent", version, func(setStep func(string), setProgress func(done, total int64)) error {
		setStep("获取二进制")
		data, err := fetch(setProgress)
		if err != nil {
			return err
		}
		setStep("上传到 OSS")
		sum, err := mgr.PublishAgentUpdate(version, data, setProgress)
		if err != nil {
			return err
		}
		// Worth a line in the log: this is what reaches every machine.
		logAudit(client, "published agent %s (%d bytes, sha256 %s)", version, len(data), sum)
		return nil
	})
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
	fetch, err := s.payloadSource(r)
	if err != nil {
		return err
	}
	mgr, client := sess.mgr, s.clientKey(r)
	return s.jobs.start("codex", version, func(setStep func(string), setProgress func(done, total int64)) error {
		setStep("下载安装包")
		data, err := fetch(setProgress)
		if err != nil {
			return err
		}
		setStep("上传到 OSS")
		sum, err := mgr.PublishCodexUpdate(version, data, setProgress)
		if err != nil {
			return err
		}
		logAudit(client, "published Codex %s to the fleet (%d bytes, sha256 %s)", version, len(data), sum)
		return nil
	})
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
