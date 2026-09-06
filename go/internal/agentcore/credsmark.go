package agentcore

import (
	"encoding/json"
	"log"
	"path/filepath"
	"sort"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/creds"
)

// credsMark is what the agent remembers about the last credential delivery.
//
// Earlier builds stored the bare ETag. That recorded which archive was
// fetched but nothing about what came of it, so a file the delivering build
// had no target for -- or one removed afterwards -- stayed missing forever
// while the marker went on reporting success.
type credsMark struct {
	ETag   string            `json:"etag"`
	Placed map[string]string `json:"placed"` // absolute path -> SHA-256
}

// readCredsMark loads the marker. A marker written by an older build is bare
// text rather than JSON; it is reported as unusable so the archive is fetched
// once more and delivered by this build, which is exactly what an upgraded
// agent should do.
func (s *Syncer) readCredsMark() (credsMark, bool) {
	raw := s.readMarker(credsMarkerFile)
	if raw == "" {
		return credsMark{}, false
	}
	var m credsMark
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return credsMark{}, false
	}
	if m.ETag == "" || len(m.Placed) == 0 {
		return credsMark{}, false
	}
	return m, true
}

func (s *Syncer) writeCredsMark(etag string, placed map[string]string) {
	if etag == "" || len(placed) == 0 {
		return
	}
	data, err := json.Marshal(credsMark{ETag: etag, Placed: placed})
	if err != nil {
		return
	}
	s.writeMarker(credsMarkerFile, string(data))
}

// credsIntact reports whether the recorded delivery is still good, and says
// why in the log when it is not.
//
// The reason matters as much as the answer: a redelivery with no explanation
// looks like the agent thrashing, when in fact it is repairing a file that
// was removed, edited, or never written in the first place.
func (s *Syncer) credsIntact(m credsMark) bool {
	ok, drifted := creds.VerifyPlaced(m.Placed)
	if !ok {
		log.Printf("credentials: redelivering, %d file(s) missing or changed: %s",
			len(drifted), strings.Join(baseNames(drifted), ", "))
	}
	return ok
}

// baseNames shortens paths for the log. The full profile path adds nothing --
// it is the same directory every time -- while the file name is the part that
// answers "did the catalog land?".
func baseNames(v any) []string {
	var paths []string
	switch t := v.(type) {
	case map[string]string:
		for p := range t {
			paths = append(paths, p)
		}
	case []string:
		paths = append(paths, t...)
	}
	for i, p := range paths {
		paths[i] = filepath.Base(p)
	}
	sort.Strings(paths)
	return paths
}
