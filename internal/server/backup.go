package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"homestack/internal/backup"
	"homestack/internal/state"
)

// Backups (Settings → Backups, and the first-run screen). The host agent
// makes and keeps them. Opening one (it needs the passphrase) and restoring
// Homestack's own settings happens here, so a fresh install can be brought
// back before the host agent is installed; the server's parts go to the
// agent, which writes each to its fixed place.

const backupOpenFor = 30 * time.Minute

type openedBackup struct {
	token   string
	m       backup.Manifest
	files   map[string]backup.Entry
	archive []byte // decrypted, for the agent
	pass    string // to give nightly backups a key on a fresh install
	expires time.Time
}

var (
	backupMu    sync.Mutex
	backupOpen  *openedBackup // one at a time, briefly: it holds secrets
	backupFails []time.Time   // wrong passphrases lately
)

// setupOrAuth lets a request through while there's no account yet (a
// restore from the first-run screen); otherwise it needs a session.
func (s *Server) setupOrAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		none := false
		s.store.Read(func(st *state.State) { none = st.Admin == nil })
		if none {
			h(w, r)
			return
		}
		s.requireAuth(h)(w, r)
	}
}

type backupPartView struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Desc      string `json:"desc"`
	Default   bool   `json:"default"`
	Available bool   `json:"available"`
	Why       string `json:"why,omitempty"`
	Note      string `json:"note,omitempty"`
}

type openedView struct {
	Token   string           `json:"token"`
	Created time.Time        `json:"created"`
	Host    string           `json:"host"`
	Version string           `json:"version"`
	Missing []string         `json:"missing,omitempty"`
	Parts   []backupPartView `json:"parts"`
	Agent   bool             `json:"agent"`
}

func (s *Server) openedView(o *openedBackup) openedView {
	v := openedView{Token: o.token, Created: o.m.Created, Host: o.m.Host, Version: o.m.Version, Missing: o.m.Missing, Parts: []backupPartView{}, Agent: s.agentAvailable()}
	in := map[string]backup.Part{}
	for _, p := range o.m.Parts {
		in[p.ID] = p
	}
	// Homestack's own settings are ticked to begin with only on a fresh
	// install: on a running one they'd replace the settings in use and sign
	// everyone out, which is rarely what's wanted.
	fresh := false
	s.store.Read(func(st *state.State) { fresh = st.Admin == nil })
	for _, info := range backup.Parts {
		p, ok := in[info.ID]
		if !ok {
			continue
		}
		pv := backupPartView{ID: info.ID, Label: info.Label, Desc: info.Desc, Default: info.Default, Available: true, Note: p.Note}
		if !info.Host {
			pv.Default = fresh
		}
		if info.Host && !v.Agent {
			pv.Available, pv.Default = false, false
			pv.Why = "Needs the host agent."
		}
		v.Parts = append(v.Parts, pv)
	}
	return v
}

// openBackup decrypts an uploaded or saved backup and keeps it open, for
// the restore checklist.
func (s *Server) openBackup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		File       string `json:"file"` // base64
		Name       string `json:"name"` // a saved one
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2*backup.MaxSize)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "That backup is too big, or the request wasn't valid.")
		return
	}
	// A pause after several wrong passphrases (each try also costs a
	// PBKDF2 run with 600,000 rounds).
	backupMu.Lock()
	backupFails = slices.DeleteFunc(backupFails, func(t time.Time) bool { return time.Since(t) > 10*time.Minute })
	tooMany := len(backupFails) >= 10
	backupMu.Unlock()
	if tooMany {
		writeErr(w, http.StatusTooManyRequests, "Too many wrong passphrases. Try again in a few minutes.")
		return
	}

	var data []byte
	switch {
	case req.Name != "":
		if !s.agentAvailable() {
			writeErr(w, http.StatusServiceUnavailable, "The host service isn't running, so saved backups can't be read. Upload the file instead.")
			return
		}
		hr, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://agent/v1/backups/file?name="+url.QueryEscape(req.Name), nil)
		resp, err := agentStream.Do(hr)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "Couldn't reach the host service.")
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			writeErr(w, http.StatusNotFound, "There's no such backup.")
			return
		}
		data, err = io.ReadAll(io.LimitReader(resp.Body, backup.MaxSize+1))
		if err != nil {
			writeErr(w, http.StatusBadGateway, "Couldn't read the backup.")
			return
		}
	case req.File != "":
		b, err := base64.StdEncoding.DecodeString(req.File)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "That file didn't upload properly.")
			return
		}
		data = b
	default:
		writeErr(w, http.StatusBadRequest, "Pick a backup.")
		return
	}

	plain, err := backup.Open(data, req.Passphrase)
	if errors.Is(err, backup.ErrPassphrase) {
		backupMu.Lock()
		backupFails = append(backupFails, time.Now())
		backupMu.Unlock()
		writeErr(w, http.StatusConflict, "That passphrase doesn't open this backup (or the file has been changed).")
		return
	}
	if err != nil {
		writeErr(w, http.StatusConflict, sentence(err.Error()))
		return
	}
	m, files, err := backup.Unpack(plain)
	if err != nil {
		writeErr(w, http.StatusConflict, sentence(err.Error()))
		return
	}
	tok := make([]byte, 16)
	_, _ = rand.Read(tok)
	o := &openedBackup{token: hex.EncodeToString(tok), m: m, files: files, archive: plain, pass: req.Passphrase, expires: time.Now().Add(backupOpenFor)}
	backupMu.Lock()
	backupOpen = o
	backupMu.Unlock()
	writeJSON(w, http.StatusOK, s.openedView(o))
}

type restorePartResult struct {
	Part   string   `json:"part"`
	OK     bool     `json:"ok"`
	Detail string   `json:"detail"`
	Saved  []string `json:"saved,omitempty"`
	Warn   string   `json:"warn,omitempty"` // done, but something needs doing by hand
}

// restoreBackup restores the picked parts of the open backup: the
// server's through the agent, then Homestack's own (which signs everyone
// out, so it's last).
func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string   `json:"token"`
		Parts []string `json:"parts"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	backupMu.Lock()
	o := backupOpen
	backupMu.Unlock()
	if o == nil || o.token != req.Token || time.Now().After(o.expires) {
		writeErr(w, http.StatusConflict, "Open the backup again: it's only kept open for half an hour.")
		return
	}
	view := s.openedView(o)
	var host []string
	homestack := false
	for _, id := range req.Parts {
		i := slices.IndexFunc(view.Parts, func(p backupPartView) bool { return p.ID == id })
		if i < 0 || !view.Parts[i].Available {
			writeErr(w, http.StatusConflict, "That part can't be restored from this backup here: "+id)
			return
		}
		if info, _ := backup.Info(id); info.Host {
			host = append(host, id)
		} else {
			homestack = true
		}
	}
	if len(host) == 0 && !homestack {
		writeErr(w, http.StatusBadRequest, "Pick at least one part to restore.")
		return
	}

	results := []restorePartResult{}
	if len(host) > 0 {
		body := map[string]any{"archive": base64.StdEncoding.EncodeToString(o.archive), "parts": host}
		var out []restorePartResult
		if _, err := s.agentDo(r.Context(), agentStream, http.MethodPost, "/v1/backups/restore", body, &out); err != nil {
			writeErr(w, http.StatusConflict, "The host agent didn't restore anything: "+err.Error())
			return
		}
		results = append(results, out...)
		// A fresh install has no passphrase yet: nightly backups carry on
		// with this backup's.
		var bv struct {
			Ready bool `json:"ready"`
		}
		if _, err := s.agentDo(r.Context(), agentClient, http.MethodGet, "/v1/backups", nil, &bv); err == nil && !bv.Ready {
			if _, err := s.agentDo(r.Context(), agentClient, http.MethodPut, "/v1/backups/settings", map[string]string{"passphrase": o.pass}, nil); err == nil {
				results = append(results, restorePartResult{Part: "passphrase", OK: true, Detail: "Nightly backups use this backup's passphrase."})
			}
		}
	}
	signedOut := false
	if homestack {
		e, ok := o.files["homestack/homestack.json"]
		res := restorePartResult{Part: "homestack"}
		switch {
		case !ok:
			res.Detail = "This backup has no Homestack settings in it."
		default:
			if err := s.store.Restore(e.Data); err != nil {
				res.Detail = sentence(err.Error())
			} else {
				res.OK, signedOut = true, true
				res.Detail = "Your account and settings are back. Sign in with the restored account."
			}
		}
		results = append(results, res)
	}
	backupMu.Lock()
	if backupOpen == o {
		backupOpen = nil // one restore per opening; it held secrets
	}
	backupMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "signedOut": signedOut})
}

// backupFile downloads a saved backup.
func (s *Server) backupFile(w http.ResponseWriter, r *http.Request) {
	if !s.agentAvailable() {
		writeErr(w, http.StatusServiceUnavailable, "The host service isn't running.")
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://agent/v1/backups/file?name="+url.QueryEscape(r.URL.Query().Get("name")), nil)
	resp, err := agentStream.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Couldn't reach the host service.")
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Disposition", "Content-Length"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, backup.MaxSize+1))
}
