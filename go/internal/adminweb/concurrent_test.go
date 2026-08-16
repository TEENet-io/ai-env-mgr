package adminweb

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
)

// The session store and the limiter are touched by every request, so they are
// only ever used concurrently in production. The other tests here drive one
// goroutine at a time, which means `go test -race` never actually exercises
// that -- these do.

func TestSessionStoreUnderConcurrentUse(t *testing.T) {
	store := newSessionStore(30*time.Minute, 12*time.Hour)
	const workers = 32

	var wg sync.WaitGroup
	ids := make(chan string, workers*4)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				id, err := store.create(&admincore.Manager{Store: newFakeStore()}, "bucket", "endpoint")
				if err != nil {
					t.Errorf("create: %v", err)
					return
				}
				ids <- id
			}
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]bool)
	for id := range ids {
		if seen[id] {
			t.Fatalf("session id %q was handed out twice", id)
		}
		seen[id] = true
	}
	if len(seen) != workers*4 {
		t.Fatalf("got %d sessions, want %d", len(seen), workers*4)
	}

	// Read and destroy from many goroutines at once.
	for id := range seen {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if store.get(id) == nil {
				t.Errorf("session %q vanished", id)
			}
			store.destroy(id)
			if store.get(id) != nil {
				t.Errorf("session %q survived destroy", id)
			}
		}(id)
	}
	wg.Wait()
	if n := store.count(); n != 0 {
		t.Fatalf("%d sessions left behind", n)
	}
}

func TestLimiterUnderConcurrentUse(t *testing.T) {
	// One shared key, so every goroutine contends on the same bucket and the
	// count has to come out exactly right.
	const max = 50
	l := newLoginLimiter(time.Minute, max)

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.allow("198.51.100.1") {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != max {
		t.Fatalf("%d attempts got through, want exactly %d -- the limit is not atomic", allowed, max)
	}
}

func TestConcurrentRequestsThroughTheHandler(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	cookie := signIn(t, s)
	h := s.Handler()

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := "/policy"
			if i%2 == 0 {
				path = "/machines"
			}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.AddCookie(cookie)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("%s returned %d", path, rec.Code)
			}
		}(i)
	}
	wg.Wait()
}
