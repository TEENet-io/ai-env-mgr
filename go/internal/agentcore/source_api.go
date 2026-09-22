package agentcore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/agentapi"
	"github.com/TEENet-io/ai-env-mgr/internal/deviceconfig"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// APISource is the console's device API as a Source. One configuration
// document carries the policy and the binding; it is fetched once per
// cycle (or handed over by Wait) and served to the two calls from memory.
type APISource struct {
	Client  *agentapi.Client
	Timeout time.Duration // per call; default 30s
	// Download fetches a signed link: the artifact and upload transfers,
	// which carry no token. Default: a plain client with a long timeout.
	Download *http.Client

	mu           sync.Mutex
	cfg          *deviceconfig.Config
	unauthorized bool
}

// NewAPISource builds a source over a client.
func NewAPISource(c *agentapi.Client) *APISource {
	return &APISource{Client: c}
}

func (a *APISource) ctx() (context.Context, context.CancelFunc) {
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return context.WithTimeout(context.Background(), timeout)
}

// note remembers a refused token so the agent can enrol again.
func (a *APISource) note(err error) error {
	if errors.Is(err, agentapi.ErrUnauthorized) {
		a.mu.Lock()
		a.unauthorized = true
		a.mu.Unlock()
	}
	return err
}

// NeedsEnrol reports whether the console has refused the token since the
// last Reset.
func (a *APISource) NeedsEnrol() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.unauthorized
}

// Reset installs a fresh token after re-enrolment.
func (a *APISource) Reset(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Client.Token = token
	a.unauthorized = false
	a.cfg = nil
}

// ETag is the etag of the configuration last seen, "" before the first.
func (a *APISource) ETag() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg == nil {
		return ""
	}
	return a.cfg.ETag
}

// refresh fetches the configuration unless the console says it is the one
// already held.
func (a *APISource) refresh() (*deviceconfig.Config, error) {
	a.mu.Lock()
	have := ""
	if a.cfg != nil {
		have = a.cfg.ETag
	}
	a.mu.Unlock()
	ctx, cancel := a.ctx()
	defer cancel()
	cfg, unchanged, err := a.Client.Config(ctx, have)
	if err != nil {
		return nil, a.note(err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !unchanged {
		a.cfg = cfg
	}
	if a.cfg == nil {
		return nil, errors.New("the console answered 'unchanged' before it had said anything")
	}
	return a.cfg, nil
}

func etagOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

func (a *APISource) Policy() ([]byte, string, error) {
	cfg, err := a.refresh()
	if err != nil {
		return nil, "", err
	}
	if cfg.Forgotten {
		return nil, "", errors.New("the console has forgotten this machine")
	}
	if !cfg.HasPolicy {
		return nil, "", errors.New("no policy has been published")
	}
	data, err := json.Marshal(cfg.Policy)
	if err != nil {
		return nil, "", err
	}
	return data, etagOf(data), nil
}

// Binding serves the binding from the configuration Policy fetched. It is
// asked right after Policy in every cycle, so this is the same document.
func (a *APISource) Binding(_ string) ([]byte, string, bool, error) {
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	if cfg == nil {
		var err error
		if cfg, err = a.refresh(); err != nil {
			return nil, "", false, err
		}
	}
	if cfg.Forgotten || !cfg.HasBinding {
		return nil, "", false, nil
	}
	data, err := json.Marshal(cfg.Binding)
	if err != nil {
		return nil, "", false, err
	}
	return data, etagOf(data), true, nil
}

func (a *APISource) Credentials(_ string, ifNoneMatch string) ([]byte, string, bool, bool, error) {
	ctx, cancel := a.ctx()
	defer cancel()
	data, etag, exists, unchanged, err := a.Client.Credentials(ctx, ifNoneMatch)
	return data, etag, exists, unchanged, a.note(err)
}

func (a *APISource) download() *http.Client {
	if a.Download != nil {
		return a.Download
	}
	return &http.Client{Timeout: 30 * time.Minute}
}

// open resolves the signed link for a target and starts the download.
func (a *APISource) open(product string, target model.ReleaseTarget) (io.ReadCloser, error) {
	ctx, cancel := a.ctx()
	defer cancel()
	link, _, err := a.Client.ArtifactURL(ctx, product, target.Version)
	if err != nil {
		return nil, a.note(err)
	}
	res, err := a.download().Get(link)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("download %s %s: %s", product, target.Version, res.Status)
	}
	return res.Body, nil
}

func (a *APISource) ArtifactBytes(product string, target model.ReleaseTarget) ([]byte, error) {
	body, err := a.open(product, target)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(io.LimitReader(body, 512<<20))
}

// ArtifactToFile streams to a temporary file beside dest and renames it
// into place only when complete, so a partial download is never mistaken
// for a finished one.
func (a *APISource) ArtifactToFile(product string, target model.ReleaseTarget, dest string) (string, error) {
	body, err := a.open(product, target)
	if err != nil {
		return "", err
	}
	defer body.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return "", err
	}
	tmp := dest + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (a *APISource) ReportStatus(_ string, data []byte) error {
	ctx, cancel := a.ctx()
	defer cancel()
	return a.note(a.Client.PostStatus(ctx, data))
}

func (a *APISource) UploadLog(_ string, tail []byte) error {
	ctx, cancel := a.ctx()
	defer cancel()
	return a.note(a.Client.PostLog(ctx, tail))
}

// Changed is answered by Wait instead: the console tells the agent.
func (a *APISource) Changed(string, string, string) (bool, string) { return false, "" }

// Wait holds until the console says the configuration changed, or its
// patience runs out (changed false). The new configuration is kept, so the
// cycle that follows asks the console for nothing it already has.
func (a *APISource) Wait(ctx context.Context) (bool, error) {
	cfg, changed, err := a.Client.Wait(ctx, a.ETag())
	if err != nil {
		return false, a.note(err)
	}
	if changed {
		a.mu.Lock()
		a.cfg = cfg
		a.mu.Unlock()
	}
	return changed, nil
}

// Put uploads one collected session file through a signed link. It is the
// Putter the Collector wants; the key is the bucket key it would have used,
// which names the user and the file.
func (a *APISource) Put(key string, data []byte) error {
	user, rel, ok := ossclient.DataCollectUser(key)
	if !ok {
		return fmt.Errorf("collect: %q is not a session file key", key)
	}
	ctx, cancel := a.ctx()
	defer cancel()
	link, contentType, err := a.Client.CollectUploadURL(ctx, user, rel)
	if err != nil {
		return a.note(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, link, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = int64(len(data))
	res, err := a.download().Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("collect: upload %s: %s", rel, res.Status)
	}
	return nil
}
