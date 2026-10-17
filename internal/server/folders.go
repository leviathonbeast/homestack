package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"homestack/internal/state"
)

// Container folders on the Containers page (see state.ContainerFolder). The
// page sends the whole list back on every change; it's small.

var (
	reFolderIcon  = regexp.MustCompile(`^[a-z][a-z-]{0,23}$`)
	reFolderColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	reCtrName     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
)

func (s *Server) getFolders(w http.ResponseWriter, r *http.Request) {
	var out []state.ContainerFolder
	s.store.Read(func(st *state.State) { out = append(out, st.Folders...) })
	if out == nil {
		out = []state.ContainerFolder{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) putFolders(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Folders []state.ContainerFolder `json:"folders"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	folders, err := cleanFolders(req.Folders)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.Update(func(st *state.State) error {
		st.Folders = folders
		return nil
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "Couldn't save the folders.")
		return
	}
	writeJSON(w, http.StatusOK, folders)
}

// cleanFolders checks the list from the page. A container listed in two
// folders stays in the first.
func cleanFolders(in []state.ContainerFolder) ([]state.ContainerFolder, error) {
	if len(in) > 100 {
		return nil, fmt.Errorf("That's more folders than the page can show.")
	}
	ids := map[string]bool{}
	taken := map[string]bool{}
	out := make([]state.ContainerFolder, 0, len(in))
	for _, f := range in {
		f.Name = strings.TrimSpace(f.Name)
		if f.Name == "" || len(f.Name) > 60 {
			return nil, fmt.Errorf("Give each folder a name (up to 60 characters).")
		}
		if f.ID == "" || len(f.ID) > 32 || ids[f.ID] {
			b := make([]byte, 6)
			_, _ = rand.Read(b)
			f.ID = hex.EncodeToString(b)
		}
		ids[f.ID] = true
		if f.Icon != "" && !reFolderIcon.MatchString(f.Icon) {
			f.Icon = ""
		}
		if f.Color != "" && !reFolderColor.MatchString(f.Color) {
			f.Color = ""
		}
		f.Match = strings.TrimSpace(f.Match)
		if len(f.Match) > 200 {
			return nil, fmt.Errorf("%s: the name pattern is too long.", f.Name)
		}
		if f.Match != "" {
			if _, err := regexp.Compile(f.Match); err != nil {
				return nil, fmt.Errorf("%s: the name pattern isn't a valid regular expression.", f.Name)
			}
		}
		names := []string{}
		for _, c := range f.Containers {
			c = strings.TrimPrefix(strings.TrimSpace(c), "/")
			if !reCtrName.MatchString(c) || taken[c] {
				continue
			}
			taken[c] = true
			names = append(names, c)
		}
		if len(names) > 500 {
			return nil, fmt.Errorf("%s has too many containers.", f.Name)
		}
		f.Containers = names
		out = append(out, f)
	}
	return out, nil
}
