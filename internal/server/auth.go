package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"homestack/internal/state"
)

const cookieName = "homestack_session"

type ctxKey int

const sessionIDKey ctxKey = 1

func hashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// sessionFor returns the session matching the request's cookie, if valid.
func (s *Server) sessionFor(r *http.Request) (state.Session, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return state.Session{}, false
	}
	h := hashToken(c.Value)
	var found state.Session
	ok := false
	now := time.Now()
	s.store.Read(func(st *state.State) {
		if st.Admin == nil {
			return
		}
		for _, sess := range st.Sessions {
			if sess.TokenHash == h && now.Before(sess.Expires) {
				found, ok = sess, true
				return
			}
		}
	})
	if ok && now.Sub(found.LastSeen) > 5*time.Minute {
		// Record activity occasionally, not on every request.
		ip := s.clientIP(r).String()
		_ = s.store.Update(func(st *state.State) error {
			for i := range st.Sessions {
				if st.Sessions[i].TokenHash == h {
					st.Sessions[i].LastSeen = now
					st.Sessions[i].IP = ip
				}
			}
			return nil
		})
	}
	return found, ok
}

// requireAuth wraps a handler so it only runs for a signed-in admin.
func (s *Server) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.sessionFor(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "Sign in to continue.")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), sessionIDKey, sess.ID)))
	}
}

func currentSessionID(r *http.Request) string {
	id, _ := r.Context().Value(sessionIDKey).(string)
	return id
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, maxAge time.Duration) {
	c := &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   s.cfg.SecureCookie || r.TLS != nil,
	}
	if maxAge > 0 {
		c.MaxAge = int(maxAge.Seconds())
	} else if maxAge < 0 {
		c.MaxAge = -1
	}
	// maxAge == 0: a browser-session cookie that goes away when the browser closes.
	http.SetCookie(w, c)
}

// startSession creates a session and sets the cookie.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, remember bool) error {
	token := randomToken(32)
	days := s.store.Settings().SessionDays
	life := time.Duration(days) * 24 * time.Hour
	if !remember {
		life = 12 * time.Hour
	}
	now := time.Now()
	sess := state.Session{
		ID:        randomToken(9),
		TokenHash: hashToken(token),
		Created:   now,
		LastSeen:  now,
		Expires:   now.Add(life),
		UserAgent: truncate(r.UserAgent(), 300),
		IP:        s.clientIP(r).String(),
	}
	err := s.store.Update(func(st *state.State) error {
		st.Sessions = append(st.Sessions, sess)
		// Keep the list tidy: at most 50 sessions, oldest dropped first.
		if len(st.Sessions) > 50 {
			sort.Slice(st.Sessions, func(i, j int) bool { return st.Sessions[i].LastSeen.After(st.Sessions[j].LastSeen) })
			st.Sessions = st.Sessions[:50]
		}
		return nil
	})
	if err != nil {
		return err
	}
	if remember {
		s.setSessionCookie(w, r, token, life)
	} else {
		s.setSessionCookie(w, r, token, 0)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9._@-]{2,64}$`)

// ---------- Handlers ----------

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	var setupRequired bool
	var username, theme, appHost string
	s.store.Read(func(st *state.State) {
		setupRequired = st.Admin == nil
		if st.Admin != nil {
			username = st.Admin.Username
		}
		theme = st.Settings.Theme
		appHost = st.Settings.AppHost
	})
	_, authed := s.sessionFor(r)
	resp := map[string]any{
		"setupRequired": setupRequired,
		"authenticated": authed,
		"version":       s.cfg.Version,
		"source":        s.cfg.Source,
		"theme":         theme,
	}
	if authed {
		resp["username"] = username
		resp["appHost"] = appHost
	}
	writeJSON(w, http.StatusOK, resp)
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Remember bool   `json:"remember"`
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var req credentials
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if !usernameRe.MatchString(req.Username) {
		writeErr(w, http.StatusBadRequest, "Use 2 to 64 letters, numbers, dots, dashes or underscores for the username.")
		return
	}
	if err := state.ValidatePassword(req.Password); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := state.HashPassword(req.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Couldn't create the account.")
		return
	}
	err = s.store.Update(func(st *state.State) error {
		if st.Admin != nil {
			return fmt.Errorf("already set up")
		}
		st.Admin = &state.Admin{Username: req.Username, PasswordHash: hash, Changed: time.Now()}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusConflict, "Homestack is already set up. Sign in instead.")
		return
	}
	if err := s.startSession(w, r, true); err != nil {
		writeErr(w, http.StatusInternalServerError, "Account created, but signing in failed. Try signing in.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": req.Username})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r).String()
	if wait := s.limiter.blocked(ip); wait > 0 {
		writeErr(w, http.StatusTooManyRequests, fmt.Sprintf("Too many attempts. Try again in %d minutes.", int(wait.Minutes())+1))
		return
	}
	var req credentials
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var admin *state.Admin
	s.store.Read(func(st *state.State) {
		if st.Admin != nil {
			a := *st.Admin
			admin = &a
		}
	})
	if admin == nil {
		writeErr(w, http.StatusConflict, "Create your admin account first.")
		return
	}
	// Always run the hash so response time doesn't reveal whether the
	// username was right.
	passOK := state.CheckPassword(admin.PasswordHash, req.Password)
	userOK := strings.EqualFold(strings.TrimSpace(req.Username), admin.Username)
	if !passOK || !userOK {
		s.limiter.fail(ip)
		writeErr(w, http.StatusUnauthorized, "That username or password isn't right.")
		return
	}
	s.limiter.reset(ip)
	if err := s.startSession(w, r, req.Remember); err != nil {
		writeErr(w, http.StatusInternalServerError, "Couldn't sign in. Try again.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": admin.Username})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		h := hashToken(c.Value)
		_ = s.store.Update(func(st *state.State) error {
			st.Sessions = filterSessions(st.Sessions, func(x state.Session) bool { return x.TokenHash != h })
			return nil
		})
	}
	s.setSessionCookie(w, r, "", -1)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func filterSessions(in []state.Session, keep func(state.Session) bool) []state.Session {
	out := in[:0]
	for _, x := range in {
		if keep(x) {
			out = append(out, x)
		}
	}
	return out
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	cur := currentSessionID(r)
	type item struct {
		ID        string    `json:"id"`
		Created   time.Time `json:"created"`
		LastSeen  time.Time `json:"lastSeen"`
		Expires   time.Time `json:"expires"`
		UserAgent string    `json:"userAgent"`
		IP        string    `json:"ip"`
		Current   bool      `json:"current"`
	}
	out := []item{}
	now := time.Now()
	s.store.Read(func(st *state.State) {
		for _, x := range st.Sessions {
			if now.After(x.Expires) {
				continue
			}
			out = append(out, item{x.ID, x.Created, x.LastSeen, x.Expires, x.UserAgent, x.IP, x.ID == cur})
		}
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Current != out[j].Current {
			return out[i].Current
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) revokeSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_ = s.store.Update(func(st *state.State) error {
		st.Sessions = filterSessions(st.Sessions, func(x state.Session) bool { return x.ID != id })
		return nil
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) revokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	cur := currentSessionID(r)
	_ = s.store.Update(func(st *state.State) error {
		st.Sessions = filterSessions(st.Sessions, func(x state.Session) bool { return x.ID == cur })
		return nil
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := state.ValidatePassword(req.New); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := state.HashPassword(req.New)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Couldn't change the password.")
		return
	}
	cur := currentSessionID(r)
	err = s.store.Update(func(st *state.State) error {
		if st.Admin == nil || !state.CheckPassword(st.Admin.PasswordHash, req.Current) {
			return fmt.Errorf("Your current password isn't right.")
		}
		st.Admin.PasswordHash = hash
		st.Admin.Changed = time.Now()
		// Sign out every other device.
		st.Sessions = filterSessions(st.Sessions, func(x state.Session) bool { return x.ID == cur })
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) changeUsername(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if !usernameRe.MatchString(req.Username) {
		writeErr(w, http.StatusBadRequest, "Use 2 to 64 letters, numbers, dots, dashes or underscores.")
		return
	}
	_ = s.store.Update(func(st *state.State) error {
		if st.Admin != nil {
			st.Admin.Username = req.Username
		}
		return nil
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": req.Username})
}

// sessionJanitor removes expired sessions once an hour.
func (s *Server) sessionJanitor(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		now := time.Now()
		var expired bool
		s.store.Read(func(st *state.State) {
			for _, x := range st.Sessions {
				if now.After(x.Expires) {
					expired = true
				}
			}
		})
		if expired {
			_ = s.store.Update(func(st *state.State) error {
				st.Sessions = filterSessions(st.Sessions, func(x state.Session) bool { return now.Before(x.Expires) })
				return nil
			})
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---------- Brute-force protection ----------

// loginLimiter locks an IP out for 5 minutes after 5 wrong passwords.
type loginLimiter struct {
	mu    sync.Mutex
	fails map[string]*failRecord
}

type failRecord struct {
	count int
	until time.Time
	last  time.Time
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{fails: map[string]*failRecord{}} }

func (l *loginLimiter) blocked(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if f, ok := l.fails[ip]; ok && time.Now().Before(f.until) {
		return time.Until(f.until)
	}
	return 0
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for k, f := range l.fails { // forget old records
		if now.Sub(f.last) > time.Hour {
			delete(l.fails, k)
		}
	}
	f := l.fails[ip]
	if f == nil {
		f = &failRecord{}
		l.fails[ip] = f
	}
	f.count++
	f.last = now
	if f.count >= 5 {
		f.until = now.Add(5 * time.Minute)
		f.count = 0
	}
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}
