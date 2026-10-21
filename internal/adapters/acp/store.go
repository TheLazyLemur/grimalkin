package acp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

// display is one thing the client was shown. Text is the prompt's, the
// reply's or a tool's output; the other fields are a tool call's.
type display struct {
	Kind    string          `json:"kind"` // one of the display kinds
	Text    string          `json:"text,omitempty"`
	ToolID  string          `json:"tool_id,omitempty"`
	Name    string          `json:"name,omitempty"`    // displayToolStart
	Summary string          `json:"summary,omitempty"` // displayToolStart
	Input   json.RawMessage `json:"input,omitempty"`   // displayToolStart
	Failed  bool            `json:"failed,omitempty"`  // displayToolEnd
}

const (
	displayPrompt    = "prompt"
	displayReply     = "reply"
	displayToolStart = "tool_start"
	displayToolEnd   = "tool_end"
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

// readSession reads a session file. A last line that isn't JSON is a write
// the process died in: it is cut off the file, so what is appended next
// doesn't join it, and the session loads without it. A bad line anywhere else
// is an error.
func readSession(path string) (saved, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return saved{}, err
	}
	var records []record
	for start := 0; start < len(data); {
		line, next := data[start:], len(data)
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			line, next = line[:i], start+i+1
		}
		lineStart := start
		start = next
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		n := len(records) + 1
		var r record
		if err := json.Unmarshal(line, &r); err != nil {
			if next == len(data) && !json.Valid(line) {
				if err := os.Truncate(path, int64(lineStart)); err != nil {
					return saved{}, fmt.Errorf("cut the torn last line: %w", err)
				}
				break
			}
			return saved{}, fmt.Errorf("record %d: %w", n, err)
		}
		if (r.Display == nil) == (r.Messages == nil) || r.Turn < 1 {
			return saved{}, fmt.Errorf("record %d: want a turn and either display or messages", n)
		}
		if err := r.Display.validate(); err != nil {
			return saved{}, fmt.Errorf("record %d: %w", n, err)
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

// validate says what is wrong with a display record read from a file. A nil
// display is fine: the record is a messages one.
func (d *display) validate() error {
	if d == nil {
		return nil
	}
	switch d.Kind {
	case displayPrompt, displayReply:
	case displayToolStart:
		if d.ToolID == "" || d.Name == "" {
			return errors.New("a tool_start has no tool_id or name")
		}
	case displayToolEnd:
		if d.ToolID == "" {
			return errors.New("a tool_end has no tool_id")
		}
	default:
		return fmt.Errorf("unknown display kind %q", d.Kind)
	}
	return nil
}
