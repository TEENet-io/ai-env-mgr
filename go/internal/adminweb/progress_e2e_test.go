package adminweb

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// End to end over a real HTTP transfer: the counting reader has to make the
// job's numbers move, and finish exactly on the payload size.
func TestDownloadProgressMovesOverRealHTTP(t *testing.T) {
	const size = 40 << 20 // 40 MiB, enough for several progress steps
	payload := make([]byte, size)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "41943040")
		w.Write(payload)
	}))
	defer srv.Close()

	var seen []int64
	var total int64
	data, err := downloadFromURL(srv.URL+"/asset.exe", "", func(done, tot int64) {
		seen = append(seen, done)
		total = tot
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != size {
		t.Fatalf("downloaded %d bytes, want %d", len(data), size)
	}
	if total != size {
		t.Fatalf("total reported as %d, want %d", total, size)
	}
	if len(seen) < 3 {
		t.Fatalf("only %d progress reports for %d MB", len(seen), size>>20)
	}
	if seen[len(seen)-1] != size {
		t.Fatalf("last report was %d, want the full %d", seen[len(seen)-1], size)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("progress went backwards: %d then %d", seen[i-1], seen[i])
		}
	}
	t.Logf("%d reports, from %d MB to %d MB", len(seen), seen[0]>>20, seen[len(seen)-1]>>20)
}

// A server that declares no length must not produce a fake percentage.
func TestDownloadProgressWithoutContentLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		// Chunked: no Content-Length reaches the client.
		for i := 0; i < 8; i++ {
			w.Write(make([]byte, 1<<20))
			w.(http.Flusher).Flush()
			time.Sleep(time.Millisecond)
		}
	}))
	defer srv.Close()

	var total int64 = 999
	if _, err := downloadFromURL(srv.URL+"/a.exe", "", func(done, tot int64) { total = tot }); err != nil {
		t.Fatal(err)
	}
	if total > 0 {
		t.Fatalf("total = %d; an undeclared length must stay unknown", total)
	}
	j := &job{Total: total}
	if j.Measured() {
		t.Fatal("an unknown total was drawn as a real bar")
	}
}
