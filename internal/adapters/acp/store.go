package acp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"grimalkin/internal/core"
)

// A session is saved as one JSONL file of records in the order they
// happened. A turn's display records are what the client was sent; its
// messages record is written when the core keeps it, so a turn with no
// messages record was discarded or never finished, and is not replayed.
type record struct {
	Turn     int            `json:"turn"`
	Display  *display       `json:"display,omitempty"`
	Messages []core.Message `json:"messages,omitempty"`
}

// display is one thing the client was shown. Tool calls get kinds of their
// own when the adapter shows them.
type display struct {
	Kind string `json:"kind"` // displayPrompt or displayReply
	Text string `json:"text"`
}

const (
	displayPrompt = "prompt"
	displayReply  = "reply"
)

// sessionIDPattern is the shape NewSession makes. A client's id becomes a
// path, so nothing else is let near the filesystem.
var sessionIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// sessionPath is where a session is saved, creating its directory.
func sessionPath(id string) (string, error) {
	if !sessionIDPattern.MatchString(id) {
		panic("session path for an id NewSession could not have made: " + id)
	}
	state := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(state) { // the XDG spec says to ignore a relative one
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		state = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(state, "grimalkin", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".jsonl"), nil
}

// appendRecords writes records to the end of the session file in one write,
// so a turn's records land together.
func appendRecords(path string, records ...record) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	_, err = f.Write(buf.Bytes())
	return errors.Join(err, f.Close())
}

// saved is a session as read back: the kept conversation, the display
// records of its kept turns in order, and the last turn number used.
type saved struct {
	messages []core.Message
	shown    []display
	lastTurn int
}

func readSession(path string) (saved, error) {
	f, err := os.Open(path)
	if err != nil {
		return saved{}, err
	}
	defer f.Close()
	var records []record
	dec := json.NewDecoder(f)
	for {
		var r record
		err := dec.Decode(&r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return saved{}, fmt.Errorf("record %d: %w", len(records)+1, err)
		}
		if (r.Display == nil) == (r.Messages == nil) || r.Turn < 1 {
			return saved{}, fmt.Errorf("record %d: want a turn and either display or messages", len(records)+1)
		}
		if r.Display != nil && r.Display.Kind != displayPrompt && r.Display.Kind != displayReply {
			return saved{}, fmt.Errorf("record %d: unknown display kind %q", len(records)+1, r.Display.Kind)
		}
		records = append(records, r)
	}
	var s saved
	kept := map[int]bool{}
	for _, r := range records {
		s.lastTurn = max(s.lastTurn, r.Turn)
		if r.Messages != nil {
			kept[r.Turn] = true
			s.messages = append(s.messages, r.Messages...)
		}
	}
	for _, r := range records {
		if r.Display != nil && kept[r.Turn] {
			s.shown = append(s.shown, *r.Display)
		}
	}
	return s, nil
}
