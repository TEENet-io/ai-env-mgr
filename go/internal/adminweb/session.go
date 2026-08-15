// Package adminweb serves the administrator console over HTTP.
//
// The console holds no credentials of its own. An administrator signs in by
// entering the OSS credentials they would otherwise put in admin.config.json;
// those live in this process's memory for the lifetime of the session and are
// never written to disk, never placed in a cookie, and never logged. Restarting
// the server therefore signs everyone out, which is the intended trade: there
// is nothing at rest for an attacker who reaches the host to steal.
//
// This matters more than usual here. The credentials can write _agent/, and
// every machine in the fleet downloads and executes what it finds there, so a
// stolen admin credential is remote code execution on every employee desktop.
package adminweb

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
)

// sessionCookie is the cookie carrying the session id. The id is a random
// token and nothing else: the credentials it stands for stay on the server.
const sessionCookie = "aem_session"

type session struct {
	mgr      *admincore.Manager
	bucket   string // shown in the UI so an operator can tell environments apart
	endpoint string
	created  time.Time
	lastSeen time.Time
}

// sessionStore keeps live sessions in memory, expiring them once idle.
type sessionStore struct {
	mu       sync.Mutex
	byID     map[string]*session
	idleTTL  time.Duration
	absTTL   time.Duration
	nowFunc  func() time.Time // overridable in tests
	maxCount int
}

func newSessionStore(idleTTL, absTTL time.Duration) *sessionStore {
	return &sessionStore{
		byID:    make(map[string]*session),
		idleTTL: idleTTL,
		absTTL:  absTTL,
		nowFunc: time.Now,
		// A cap keeps an attacker who can reach the login page from growing
		// this map without bound; real deployments have a handful of admins.
		maxCount: 256,
	}
}

func (s *sessionStore) now() time.Time { return s.nowFunc() }

// newID returns a 256-bit random session id. crypto/rand failing is not
// something to paper over with a weaker id, so the error is returned.
func newID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// create registers a signed-in administrator and returns the session id.
func (s *sessionStore) create(mgr *admincore.Manager, bucket, endpoint string) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapLocked()
	if len(s.byID) >= s.maxCount {
		return "", errTooManySessions
	}
	now := s.now()
	s.byID[id] = &session{mgr: mgr, bucket: bucket, endpoint: endpoint, created: now, lastSeen: now}
	return id, nil
}

// get returns the session for an id, refreshing its idle timer. A missing or
// expired session returns nil, which callers treat as "not signed in".
func (s *sessionStore) get(id string) *session {
	if id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return nil
	}
	now := s.now()
	if s.expiredLocked(sess, now) {
		delete(s.byID, id)
		return nil
	}
	sess.lastSeen = now
	return sess
}

func (s *sessionStore) destroy(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, id)
}

func (s *sessionStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byID)
}

// expiredLocked reports whether a session has gone idle for too long, or has
// simply been alive too long. The absolute cap is there because an idle timer
// alone lets a stolen cookie live forever as long as it keeps being used.
func (s *sessionStore) expiredLocked(sess *session, now time.Time) bool {
	return now.Sub(sess.lastSeen) > s.idleTTL || now.Sub(sess.created) > s.absTTL
}

func (s *sessionStore) reapLocked() {
	now := s.now()
	for id, sess := range s.byID {
		if s.expiredLocked(sess, now) {
			delete(s.byID, id)
		}
	}
}
