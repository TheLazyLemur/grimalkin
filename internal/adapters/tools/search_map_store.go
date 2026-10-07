package tools

// The repo map is kept in the project, in .grimalkin/map.json, so a later
// session sends Jev only the files that are new or changed. The file is
// repository content like any other: a clone can bring one along. So it is
// used only if everything in it could have come from a build here, since its
// roles and concepts are printed for the model.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"time"
)

const (
	searchMapDir  = ".grimalkin"
	searchMapPath = searchMapDir + "/map.json"
	// searchMapVersion is raised when the roles, the questions or what a
	// file's tags are made from change, so older files are built again.
	searchMapVersion  = 1
	maxSearchMapBytes = 16 * 1024 * 1024
)

// searchMapSaveEvery is how often a running build is saved, so quitting
// loses little of it. A variable so tests can shorten it.
var searchMapSaveEvery = 30 * time.Second

type searchMapStore struct {
	Version int                       `json:"version"`
	Model   string                    `json:"model"`
	Vocab   []string                  `json:"vocab"`
	Files   map[string]searchMapEntry `json:"files"`
}

// searchMapWord is the shape of every word searchWords can harvest.
var searchMapWord = regexp.MustCompile(`^[a-z]{4,}$`)

// searchMapHash identifies what a file's tags were made from: its first
// chunk, the only part of it Jev saw.
func searchMapHash(f searchFile) string {
	sum := sha256.Sum256([]byte(f.chunks[0].Content))
	return hex.EncodeToString(sum[:])
}

// loadMap reads the stored map. It returns nil when there is none, when it
// was made by another version or for another model, or when anything in it
// is not what a build writes.
func (s *Search) loadMap(root *os.Root) *searchMapStore {
	f, err := root.Open(searchMapPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSearchMapBytes+1))
	var stored searchMapStore
	if err != nil || len(data) > maxSearchMapBytes || json.Unmarshal(data, &stored) != nil {
		return nil
	}
	if stored.Version != searchMapVersion || stored.Model != s.model || len(stored.Vocab) < 2 || len(stored.Vocab) > searchMapConcepts {
		return nil
	}
	vocab := map[string]bool{}
	for _, word := range stored.Vocab {
		if !searchMapWord.MatchString(word) {
			return nil
		}
		vocab[word] = true
	}
	for _, entry := range stored.Files {
		if _, ok := searchRoles[entry.Role]; !ok {
			return nil
		}
		for concept, p := range entry.Concepts {
			if !vocab[concept] || !(p >= 0 && p <= 1) {
				return nil
			}
		}
	}
	return &stored
}

// saveMap writes the map as it stands. The directory ignores itself, so the
// project's own .gitignore is left alone.
func (s *Search) saveMap() error {
	m := &s.repoMap
	m.mu.Lock()
	if len(m.entries) == 0 {
		// Nothing worth replacing an earlier map with.
		m.mu.Unlock()
		return nil
	}
	data, err := json.Marshal(searchMapStore{Version: searchMapVersion, Model: s.model, Vocab: m.vocab, Files: m.entries})
	m.mu.Unlock()
	if err != nil {
		return err
	}
	// os.Root keeps a symlinked .grimalkin from sending the writes elsewhere.
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(searchMapDir, 0o755); err != nil {
		return err
	}
	if _, err := root.Lstat(searchMapDir + "/.gitignore"); errors.Is(err, os.ErrNotExist) {
		if err := root.WriteFile(searchMapDir+"/.gitignore", []byte("*\n"), 0o644); err != nil {
			return err
		}
	}
	// Written whole, then moved into place, so a reader never sees half.
	if err := root.WriteFile(searchMapPath+".tmp", data, 0o644); err != nil {
		return err
	}
	return root.Rename(searchMapPath+".tmp", searchMapPath)
}
