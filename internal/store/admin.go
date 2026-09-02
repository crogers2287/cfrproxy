package store

import (
	"crypto/sha256"
	"crypto/subtle"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Admin credential verification with a short positive cache.
//
// bcrypt at DefaultCost costs ~60-80 ms of CPU per comparison. The WebUI polls
// several endpoints a second and the data-plane gate also accepts admin
// credentials, so verifying on every request was both the dashboard's latency
// floor and a free CPU-exhaustion target on any exposed port (~15 req/s
// saturates a core). A verified pair is remembered by the SHA-256 of
// user + password + the stored hash, so changing the password invalidates
// every cached entry without bookkeeping.
const (
	adminCacheTTL = 5 * time.Minute
	adminCacheMax = 256
)

type adminCache struct {
	mu sync.Mutex
	ok map[[32]byte]time.Time
}

// VerifyAdmin reports whether user/pass match the stored admin credentials.
func (s *Store) VerifyAdmin(user, pass string) bool {
	wantUser := s.Setting("admin_user")
	hash := s.Setting("admin_pass_hash")
	if hash == "" || subtle.ConstantTimeCompare([]byte(user), []byte(wantUser)) != 1 {
		return false
	}
	key := sha256.Sum256([]byte(user + "\x00" + pass + "\x00" + hash))
	s.admin.mu.Lock()
	exp, hit := s.admin.ok[key]
	s.admin.mu.Unlock()
	if hit && time.Now().Before(exp) {
		return true
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)) != nil {
		return false
	}
	s.admin.mu.Lock()
	if s.admin.ok == nil || len(s.admin.ok) >= adminCacheMax {
		s.admin.ok = map[[32]byte]time.Time{}
	}
	s.admin.ok[key] = time.Now().Add(adminCacheTTL)
	s.admin.mu.Unlock()
	return true
}
