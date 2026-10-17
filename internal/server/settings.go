package server

import (
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"homestack/internal/state"
)

var reHostName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

// validLinkHost accepts an IP address or a host name: no scheme, port or path.
func validLinkHost(h string) bool {
	return net.ParseIP(h) != nil || (len(h) <= 253 && reHostName.MatchString(h))
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	set := s.store.Settings()
	var username string
	s.store.Read(func(st *state.State) {
		if st.Admin != nil {
			username = st.Admin.Username
		}
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"stacksDir":        set.StacksDir,
		"sessionDays":      set.SessionDays,
		"updateCheckHours": set.UpdateCheckHours,
		"pruneAfterUpdate": set.PruneAfterUpdate,
		"theme":            set.Theme,
		"vmDomainsDir":     set.VMDomainsDir,
		"vmIsosDir":        set.VMIsosDir,
		"vmTemplatesDir":   set.VMTemplatesDir,
		"appHost":          set.AppHost,
		"username":         username,
		"stateFile":        s.store.Path(),
		"version":          s.cfg.Version,
		"source":           s.cfg.Source,
	})
}

func (s *Server) putGeneralSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StacksDir        *string `json:"stacksDir"`
		SessionDays      *int    `json:"sessionDays"`
		UpdateCheckHours *int    `json:"updateCheckHours"`
		PruneAfterUpdate *bool   `json:"pruneAfterUpdate"`
		Theme            *string `json:"theme"`
		VMDomainsDir     *string `json:"vmDomainsDir"`
		VMIsosDir        *string `json:"vmIsosDir"`
		VMTemplatesDir   *string `json:"vmTemplatesDir"`
		AppHost          *string `json:"appHost"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.AppHost != nil {
		h := strings.Trim(strings.TrimSpace(*req.AppHost), "[]")
		if h != "" && !validLinkHost(h) {
			writeErr(w, http.StatusBadRequest, "Use just an address or a name for app links, like 192.168.1.20 or server.lan (no http:// or port).")
			return
		}
		req.AppHost = &h
	}
	if req.StacksDir != nil {
		dir := filepath.Clean(strings.TrimSpace(*req.StacksDir))
		if !filepath.IsAbs(dir) {
			writeErr(w, http.StatusBadRequest, "Use a full path for the stacks folder, like /opt/stacks.")
			return
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			writeErr(w, http.StatusBadRequest, "Homestack can't create or open that folder: "+err.Error())
			return
		}
		req.StacksDir = &dir
	}
	if req.SessionDays != nil && (*req.SessionDays < 1 || *req.SessionDays > 365) {
		writeErr(w, http.StatusBadRequest, "Pick a session length between 1 and 365 days.")
		return
	}
	if req.UpdateCheckHours != nil && (*req.UpdateCheckHours < 0 || *req.UpdateCheckHours > 168) {
		writeErr(w, http.StatusBadRequest, "Pick an update check interval up to 168 hours.")
		return
	}
	if req.Theme != nil && *req.Theme != "system" && *req.Theme != "light" && *req.Theme != "dark" {
		writeErr(w, http.StatusBadRequest, "Unknown theme.")
		return
	}
	for _, p := range []*string{req.VMDomainsDir, req.VMIsosDir, req.VMTemplatesDir} {
		if p != nil {
			*p = filepath.Clean(strings.TrimSpace(*p))
			if !filepath.IsAbs(*p) {
				writeErr(w, http.StatusBadRequest, "Use full paths for the VM folders, like /mnt/user/domains.")
				return
			}
		}
	}
	intervalChanged := false
	err := s.store.Update(func(st *state.State) error {
		set := &st.Settings
		if req.StacksDir != nil {
			set.StacksDir = *req.StacksDir
		}
		if req.SessionDays != nil {
			set.SessionDays = *req.SessionDays
		}
		if req.UpdateCheckHours != nil {
			intervalChanged = set.UpdateCheckHours != *req.UpdateCheckHours
			set.UpdateCheckHours = *req.UpdateCheckHours
		}
		if req.PruneAfterUpdate != nil {
			set.PruneAfterUpdate = *req.PruneAfterUpdate
		}
		if req.Theme != nil {
			set.Theme = *req.Theme
		}
		if req.VMDomainsDir != nil {
			set.VMDomainsDir = *req.VMDomainsDir
		}
		if req.VMIsosDir != nil {
			set.VMIsosDir = *req.VMIsosDir
		}
		if req.VMTemplatesDir != nil {
			set.VMTemplatesDir = *req.VMTemplatesDir
		}
		// Guard: VM folders and template images must not mix.
		if filepath.Clean(set.VMTemplatesDir) == filepath.Clean(set.VMDomainsDir) {
			return errTemplatesDir
		}
		if req.AppHost != nil {
			set.AppHost = *req.AppHost
		}
		return nil
	})
	if errors.Is(err, errTemplatesDir) {
		writeErr(w, http.StatusBadRequest, "Use a separate folder for VM templates, like /mnt/user/domains/templates.")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Couldn't save settings.")
		return
	}
	if intervalChanged {
		s.updates.wake()
	}
	s.getSettings(w, r)
}
