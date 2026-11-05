package adminweb

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode"

	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"
	"github.com/TEENet-io/ai-env-mgr/internal/ops"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// Screenshots and attachments on a problem report. The design allows images
// on every problem kind and PDF on "other" as well, each up to 5 MB.
const (
	maxTicketFileSize = 5 << 20
	maxTicketFiles    = 5
	maxTicketForm     = maxTicketFiles*maxTicketFileSize + 1<<20
	ticketFilePrefix  = "agent_workdir/_tickets/"
	// ticketToolUnsure is always on offer: the design lets somebody who cannot
	// find their tool say so.
	ticketToolUnsure = "不确定"
)

// parseTicketForm reads a new-ticket form, plain or with files, and checks
// its CSRF token. The caller must call r.MultipartForm.RemoveAll when set.
func parseTicketForm(w http.ResponseWriter, r *http.Request, csrf string) bool {
	limit := int64(32 << 10)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		limit = maxTicketForm
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := parseUpload(r); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "文件太大：单个不超过 5 MB，最多 5 个", http.StatusRequestEntityTooLarge)
			return false
		}
		http.Error(w, "表单无法读取，请刷新页面", http.StatusBadRequest)
		return false
	}
	if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(csrf)) != 1 {
		http.Error(w, "表单已过期，请刷新页面", http.StatusForbidden)
		return false
	}
	return true
}

func removeTicketForm(r *http.Request) {
	if r.MultipartForm != nil {
		r.MultipartForm.RemoveAll()
	}
}

type ticketUpload struct {
	name, contentType string
	data              []byte
}

// ticketUploads reads and checks the files of a request. The type is decided
// from the content, never from what the browser or the name claims: these
// files are later handed to other people.
func ticketUploads(r *http.Request, requestKind string) ([]ticketUpload, error) {
	if r.MultipartForm == nil {
		return nil, nil
	}
	spec := dailyRequests[requestKind]
	var files []*multipart.FileHeader
	for _, file := range r.MultipartForm.File["files"] {
		if file.Filename != "" || file.Size > 0 {
			files = append(files, file)
		}
	}
	if len(files) == 0 {
		return nil, nil
	}
	if spec.Change {
		return nil, errors.New("这类申请不能上传文件")
	}
	if len(files) > maxTicketFiles {
		return nil, fmt.Errorf("最多上传 %d 个文件", maxTicketFiles)
	}
	out := make([]ticketUpload, 0, len(files))
	for _, file := range files {
		name := ticketFileName(file.Filename)
		if file.Size > maxTicketFileSize {
			return nil, fmt.Errorf("%s 超过 5 MB", name)
		}
		f, err := file.Open()
		if err != nil {
			return nil, errors.New("无法读取上传的文件")
		}
		data, err := io.ReadAll(io.LimitReader(f, maxTicketFileSize+1))
		f.Close()
		if err != nil || len(data) == 0 || len(data) > maxTicketFileSize {
			return nil, fmt.Errorf("%s 无法读取、为空或超过 5 MB", name)
		}
		contentType := http.DetectContentType(data)
		switch contentType {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
		case "application/pdf":
			if requestKind != "other-problem" {
				return nil, fmt.Errorf("%s 不是图片：这类问题只能上传截图", name)
			}
		default:
			return nil, fmt.Errorf("%s 不是支持的文件类型", name)
		}
		out = append(out, ticketUpload{name, contentType, data})
	}
	return out, nil
}

// ticketFileName keeps only what is safe to show and to echo in a header.
func ticketFileName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	cleaned := strings.Map(func(c rune) rune {
		if unicode.IsControl(c) {
			return -1
		}
		return c
	}, name)
	runes := []rune(strings.TrimSpace(cleaned))
	if len(runes) > 100 {
		runes = runes[len(runes)-100:]
	}
	if len(runes) == 0 || string(runes) == "." || string(runes) == "/" {
		return "文件"
	}
	return string(runes)
}

// attachTicketFiles stores the uploads and records them in the request
// details, after those kept from before an edit, and names them in the
// description, so the revision history says what each version carried.
func (s *Server) attachTicketFiles(r *http.Request, uploads []ticketUpload, kept []any, description string, details []byte) (string, []byte, error) {
	if len(uploads) == 0 && len(kept) == 0 {
		return description, details, nil
	}
	if len(uploads)+len(kept) > maxTicketFiles {
		return "", nil, fmt.Errorf("一张工单最多 %d 个文件，已有 %d 个", maxTicketFiles, len(kept))
	}
	if s.dbm.objects == nil {
		return "", nil, errors.New("文件存储不可用，请去掉文件后再提交")
	}
	var data map[string]any
	if err := json.Unmarshal(details, &data); err != nil {
		return "", nil, err
	}
	all := append([]any(nil), kept...)
	names := make([]string, 0, len(all)+len(uploads))
	for _, item := range kept {
		if file, ok := item.(map[string]any); ok {
			name, _ := file["name"].(string)
			names = append(names, name)
		}
	}
	for _, upload := range uploads {
		id := make([]byte, 16)
		if _, err := rand.Read(id); err != nil {
			return "", nil, err
		}
		fileID := hex.EncodeToString(id)
		if err := s.dbm.objects.Put(ticketFilePrefix+fileID, upload.data); err != nil {
			log.Printf("adminweb: store ticket file: %v", err)
			return "", nil, errors.New("文件保存失败，请稍后重试")
		}
		all = append(all, map[string]any{"id": fileID, "name": upload.name, "type": upload.contentType, "size": len(upload.data)})
		names = append(names, upload.name)
	}
	data["attachments"] = all
	out, err := json.Marshal(data)
	if err != nil {
		return "", nil, err
	}
	return description + ticketFilesLine(names), out, nil
}

func ticketFilesLine(names []string) string {
	return "\n文件：" + strings.Join(names, "、")
}

// ticketRequestText is the description of the current version without the
// line naming its files: that page lists them as links. Revisions keep it.
func ticketRequestText(description string, details map[string]any) string {
	files, _ := details["attachments"].([]any)
	if len(files) == 0 {
		return description
	}
	names := make([]string, 0, len(files))
	for _, item := range files {
		if file, ok := item.(map[string]any); ok {
			name, _ := file["name"].(string)
			names = append(names, name)
		}
	}
	return strings.TrimSuffix(description, ticketFilesLine(names))
}

// serveTicketFile sends one of a ticket's files. Access has been decided by
// finding the ticket. Only an id recorded on that ticket is ever read, and the
// file always downloads: it came from a customer and must not render on this
// origin.
func (s *Server) serveTicketFile(w http.ResponseWriter, r *http.Request, t dbstore.Ticket) {
	want := r.URL.Query().Get("file")
	files, _ := t.Details["attachments"].([]any)
	for _, item := range files {
		file, ok := item.(map[string]any)
		if !ok || file["id"] != want || want == "" {
			continue
		}
		id, _ := file["id"].(string)
		data, _, err := s.dbm.objects.Get(ticketFilePrefix + id)
		if err != nil {
			log.Printf("adminweb: read ticket file: %v", err)
			http.Error(w, "文件暂时无法读取", http.StatusBadGateway)
			return
		}
		contentType, _ := file["type"].(string)
		name, _ := file["name"].(string)
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
		w.Write(data)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) handleTicketFile(w http.ResponseWriter, r *http.Request, sess *session) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	t, err := s.dbm.db.Tickets().Get(r.Context(), r.URL.Query().Get("id"), "")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.serveTicketFile(w, r, t)
}

func (s *Server) handleCustomerTicketFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	c, _, ok := s.customerRequired(w, r)
	if !ok {
		return
	}
	t, err := s.dbm.db.Tickets().GetForContact(r.Context(), r.URL.Query().Get("id"), c.ID, c.Role)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.serveTicketFile(w, r, t)
}

// ticketTools is the list a tool report chooses from.
func (s *Server) ticketTools(ctx context.Context) ([]string, error) {
	settings, _, err := repo.LoadTicketToolSettings(ctx, s.dbm.store.Settings())
	return settings.Names, err
}

// checkTicketTool holds a tool report to the list. An edit may keep the tool
// it already names, which may have left the list since.
func (s *Server) checkTicketTool(r *http.Request, previous string) error {
	if formValue(r, "request_kind") != "tool-problem" {
		return nil
	}
	tool := formValue(r, "tool")
	if tool == ticketToolUnsure || (tool != "" && tool == previous) {
		return nil
	}
	names, err := s.ticketTools(r.Context())
	if err != nil {
		return errors.New("无法读取工具清单")
	}
	for _, name := range names {
		if name == tool {
			return nil
		}
	}
	return errors.New("请从清单中选择工具；找不到时选“不确定”")
}

func (s *Server) actionTicketTools(sess *session, r *http.Request) error {
	before, version, err := repo.LoadTicketToolSettings(r.Context(), s.dbm.store.Settings())
	if err != nil {
		return err
	}
	if v := formInt(r, "version"); v != version {
		return errors.New("这页的设置在你打开后已被别人改过，请刷新后重试")
	}
	seen := map[string]bool{ticketToolUnsure: true}
	after := repo.TicketToolSettings{Names: []string{}}
	for _, line := range strings.Split(formValue(r, "names"), "\n") {
		name := strings.TrimSpace(line)
		if name == "" || seen[name] {
			continue
		}
		if !validTicketText(name, 200) {
			return fmt.Errorf("工具名称无效或过长：%s", name)
		}
		seen[name] = true
		after.Names = append(after.Names, name)
	}
	value, _ := json.Marshal(after)
	return s.dbm.ops.SaveSetting(r.Context(), repo.SettingTicketTools, value, version, ops.ActionTicketTools, before, after, sess.actor, s.clientKey(r))
}
