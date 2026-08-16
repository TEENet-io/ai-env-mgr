package adminweb

import (
	"encoding/json"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// testManager reads what a handler wrote, straight out of the fake store.
type testManager struct{ fs *fakeStore }

func (t *testManager) policy() (model.Policy, error) {
	var p model.Policy
	err := json.Unmarshal(t.fs.objects[ossclient.PolicyKey()], &p)
	return p, err
}
