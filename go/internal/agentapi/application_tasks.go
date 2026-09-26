package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

func (c *Client) ClaimApplicationTask(ctx context.Context) (*model.ApplicationTask, error) {
	req, err := c.request(ctx, http.MethodPost, "/agent/v1/application-tasks/claim", nil, "")
	if err != nil {
		return nil, err
	}
	res, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	var task model.ApplicationTask
	if err := json.NewDecoder(res.Body).Decode(&task); err != nil {
		return nil, err
	}
	return &task, nil
}

func (c *Client) RenewApplicationTask(ctx context.Context, task model.ApplicationTask, progress string) error {
	return c.updateApplicationTask(ctx, task, "renew", map[string]string{"leaseToken": task.LeaseToken, "progress": progress})
}

func (c *Client) FinishApplicationTask(ctx context.Context, task model.ApplicationTask, state, lastError string) error {
	return c.updateApplicationTask(ctx, task, "finish", map[string]string{"leaseToken": task.LeaseToken, "state": state, "lastError": lastError})
}

// ApplicationTaskManifest reads the approved manifest belonging to the live
// lease. The lease token is sent in a header, never in a URL or JSON body.
func (c *Client) ApplicationTaskManifest(ctx context.Context, taskID, leaseToken string) (model.Application, error) {
	var out model.Application
	req, err := c.taskResourceRequest(ctx, taskID, leaseToken, "manifest")
	if err != nil {
		return out, err
	}
	res, err := c.do(req)
	if err != nil {
		return out, err
	}
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}

// ApplicationTaskURL asks for a signed package URL belonging to the live
// lease. A cancelled or expired task cannot obtain a new download link.
func (c *Client) ApplicationTaskURL(ctx context.Context, taskID, leaseToken string) (link, sha256hex string, err error) {
	req, err := c.taskResourceRequest(ctx, taskID, leaseToken, "download")
	if err != nil {
		return "", "", err
	}
	client := *c.http()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusFound, http.StatusTemporaryRedirect:
		return res.Header.Get("Location"), res.Header.Get("X-Artifact-SHA256"), nil
	case http.StatusUnauthorized:
		return "", "", ErrUnauthorized
	case http.StatusNotFound:
		return "", "", ErrNotFound
	case http.StatusConflict:
		return "", "", ErrTaskLost
	default:
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return "", "", fmt.Errorf("application task download: %s: %s", res.Status, strings.TrimSpace(string(msg)))
	}
}

func (c *Client) taskResourceRequest(ctx context.Context, taskID, leaseToken, resource string) (*http.Request, error) {
	req, err := c.request(ctx, http.MethodGet, "/agent/v1/application-tasks/"+url.PathEscape(taskID)+"/"+resource, nil, "")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(leaseToken) == "" {
		return nil, errors.New("application task lease token is empty")
	}
	req.Header.Set("X-Application-Lease-Token", leaseToken)
	return req, nil
}

func (c *Client) updateApplicationTask(ctx context.Context, task model.ApplicationTask, action string, in map[string]string) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := c.request(ctx, http.MethodPost, "/agent/v1/application-tasks/"+url.PathEscape(task.ID)+"/"+action, data, "application/json")
	if err != nil {
		return err
	}
	res, err := c.do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}
