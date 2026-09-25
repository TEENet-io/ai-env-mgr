package agentapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

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
