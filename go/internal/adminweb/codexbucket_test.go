package adminweb

import (
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// fakeBucket answers every read with its own name, so a test can see which
// bucket a key went to.
type fakeBucket struct{ name string }

func (f fakeBucket) ListInfo(prefix string) ([]ossclient.ObjectInfo, error) {
	return []ossclient.ObjectInfo{{Key: f.name + ":" + prefix}}, nil
}
func (f fakeBucket) HashObject(key string) (string, int64, error) { return f.name, 1, nil }
func (f fakeBucket) Head(key string) (string, bool, error)        { return f.name, true, nil }
func (f fakeBucket) SignedURL(key string, _ time.Duration) (string, error) {
	return f.name + "/" + key, nil
}
func (f fakeBucket) SignedPutURL(key string, _ time.Duration, _ string) (string, error) {
	return f.name + "/put/" + key, nil
}
func (f fakeBucket) SignedDataURL(key string, _ time.Duration) (string, error) {
	return f.name + "/data/" + key, nil
}
func (f fakeBucket) SignedDataPutURL(key string, _ time.Duration, _ string) (string, error) {
	return f.name + "/dataput/" + key, nil
}

func TestCodexComesFromTheSharedBucketAndEverythingElseFromOurOwn(t *testing.T) {
	own, shared := fakeBucket{"own"}, fakeBucket{"shared"}
	packages := packageRouter{main: own, codex: shared}
	codexKey := ossclient.CodexInstallerKey("26.901.51231-b8")
	agentKey := ossclient.AgentVersionKey("1.5.24")

	if l, _ := packages.ListInfo(ossclient.CodexPrefix); l[0].Key != "shared:"+ossclient.CodexPrefix {
		t.Fatalf("codex listing went to %s", l[0].Key)
	}
	if l, _ := packages.ListInfo(ossclient.AgentPrefix); l[0].Key != "own:"+ossclient.AgentPrefix {
		t.Fatalf("agent listing went to %s", l[0].Key)
	}
	if sum, _, _ := packages.HashObject(codexKey); sum != "shared" {
		t.Fatalf("codex checksum read from %s", sum)
	}
	if etag, _, _ := packages.Head(agentKey); etag != "own" {
		t.Fatalf("agent head read from %s", etag)
	}

	links := signerRouter{main: own, codex: shared}
	for _, c := range []struct{ got, want string }{
		{must(links.SignedURL(codexKey, time.Minute)), "shared/" + codexKey},
		{must(links.SignedDataURL(codexKey, time.Minute)), "shared/data/" + codexKey},
		{must(links.SignedURL(agentKey, time.Minute)), "own/" + agentKey},
		{must(links.SignedDataURL(agentKey, time.Minute)), "own/data/" + agentKey},
		// Agents never upload into the shared bucket, not even under _codex/.
		{must(links.SignedPutURL(codexKey, time.Minute, "")), "own/put/" + codexKey},
		{must(links.SignedDataPutURL(codexKey, time.Minute, "")), "own/dataput/" + codexKey},
	} {
		if c.got != c.want {
			t.Errorf("got %s, want %s", c.got, c.want)
		}
	}
}

func must(s string, err error) string {
	if err != nil {
		panic(err)
	}
	return s
}
