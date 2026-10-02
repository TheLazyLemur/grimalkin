package skills

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

const (
	commandTimeout   = 2 * time.Minute
	maxCommandOutput = 30_000
)

type renderer struct {
	arguments  string   // as typed
	positional []string // shell-split
	names      []string // from the arguments field, mapped to positions
	vars       *strings.Replacer
	projectDir string

	received bool // some placeholder took the arguments
}

type segment struct {
	text    string
	pattern string // non-empty: text is a command to run, written as pattern
}

// render expands the body in one pass over the file: command placeholders
// are found in the original text, so neither arguments nor command output
// can introduce new ones. Arguments expand first, then ${GRIMALKIN_*}
// variables, in both text and commands. If no placeholder took the
// arguments, they are appended. A failed command aborts the render.
func (r *renderer) render(ctx context.Context, body string) (string, error) {
	var out strings.Builder
	for _, seg := range splitCommands(body) {
		expanded := r.vars.Replace(r.expandArguments(seg.text))
		if seg.pattern == "" {
			out.WriteString(expanded)
			continue
		}
		output, err := runCommand(ctx, expanded, r.projectDir)
		if err != nil {
			return "", fmt.Errorf("shell command failed for pattern %q: %w", seg.pattern, err)
		}
		out.WriteString(output)
	}
	rendered := out.String()
	if r.arguments != "" && !r.received {
		rendered += "\n\nARGUMENTS: " + r.arguments
	}
	return rendered, nil
}

var (
	fenceOpenRE = regexp.MustCompile("(?m)^```![ \t]*$")
	fenceEndRE  = regexp.MustCompile("(?m)^```[ \t]*$")
	inlineRE    = regexp.MustCompile("(?m)(?:^|[ \t])(!`([^`\n]+)`)")
)

// splitCommands finds ```! fenced blocks, then inline !`cmd` placeholders
// where the ! starts a line or follows whitespace.
func splitCommands(body string) []segment {
	var segs []segment
	for body != "" {
		open := fenceOpenRE.FindStringIndex(body)
		if open == nil {
			break
		}
		afterOpen := open[1] + 1 // past the newline
		if afterOpen > len(body) {
			break
		}
		end := fenceEndRE.FindStringIndex(body[afterOpen:])
		if end == nil {
			break
		}
		command := strings.TrimSuffix(body[afterOpen:afterOpen+end[0]], "\n")
		segs = append(segs, splitInline(body[:open[0]])...)
		segs = append(segs, segment{text: command, pattern: body[open[0] : afterOpen+end[1]]})
		body = body[afterOpen+end[1]:]
	}
	return append(segs, splitInline(body)...)
}

func splitInline(text string) []segment {
	var segs []segment
	last := 0
	for _, m := range inlineRE.FindAllStringSubmatchIndex(text, -1) {
		segs = append(segs,
			segment{text: text[last:m[2]]},
			segment{text: text[m[4]:m[5]], pattern: text[m[2]:m[3]]})
		last = m[3]
	}
	if last < len(text) {
		segs = append(segs, segment{text: text[last:]})
	}
	return segs
}

// expandArguments replaces $ARGUMENTS, $ARGUMENTS[N], $N and declared $name.
// An index with no argument stays as written; a name with none expands to
// nothing. A single backslash before a placeholder makes it literal.
func (r *renderer) expandArguments(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		switch s[i] {
		case '\\':
			run := i
			for run < len(s) && s[run] == '\\' {
				run++
			}
			if run-i == 1 && run < len(s) && s[run] == '$' {
				if token, _, ok := r.placeholder(s[run+1:]); ok {
					out.WriteString("$" + token)
					i = run + 1 + len(token)
					continue
				}
			}
			out.WriteString(s[i:run])
			i = run
		case '$':
			token, value, ok := r.placeholder(s[i+1:])
			if ok && value != nil {
				out.WriteString(*value)
				r.received = true
				i += 1 + len(token)
				continue
			}
			out.WriteByte('$')
			i++
		default:
			out.WriteByte(s[i])
			i++
		}
	}
	return out.String()
}

var (
	indexedRE = regexp.MustCompile(`^ARGUMENTS\[(\d+)\]`)
	allRE     = regexp.MustCompile(`^ARGUMENTS\b`)
	digitsRE  = regexp.MustCompile(`^\d+`)
	wordRE    = regexp.MustCompile(`^\w+`)
)

// placeholder matches a placeholder at the start of s (just after the $).
// value is nil for an index with no argument, which stays as written.
func (r *renderer) placeholder(s string) (token string, value *string, ok bool) {
	index := func(digits string) *string {
		var n int
		if _, err := fmt.Sscan(digits, &n); err != nil || n >= len(r.positional) {
			return nil
		}
		return &r.positional[n]
	}
	if m := indexedRE.FindStringSubmatch(s); m != nil {
		return m[0], index(m[1]), true
	}
	if m := allRE.FindString(s); m != "" {
		return m, &r.arguments, true
	}
	if m := digitsRE.FindString(s); m != "" {
		return m, index(m), true
	}
	if m := wordRE.FindString(s); m != "" {
		if i := slices.Index(r.names, m); i >= 0 {
			value := ""
			if i < len(r.positional) {
				value = r.positional[i]
			}
			return m, &value, true
		}
	}
	return "", nil, false
}

// splitArguments splits like a POSIX shell: whitespace separates, quotes
// group and a backslash escapes, outside single quotes.
func splitArguments(s string) []string {
	var args []string
	var cur strings.Builder
	inArg := false
	var quote rune
	escaped := false
	for _, c := range s {
		switch {
		case escaped:
			cur.WriteRune(c)
			escaped = false
		case c == '\\' && quote != '\'':
			escaped, inArg = true, true
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote, inArg = c, true
		case c == ' ' || c == '\t' || c == '\n':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(c)
			inArg = true
		}
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args
}

// runCommand runs an injected command like a shell tool call, with stderr
// merged into stdout and a timeout. A non-zero exit fails, except exit 1
// from commands where it means "no match" or "differs".
func runCommand(ctx context.Context, command, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // no terminal to block on, as in the shell tool
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out

	err := cmd.Run()
	output := capOutput(strings.TrimRight(out.String(), "\n"))
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("timed out after %s\n[stderr]\n%s", commandTimeout, output)
		}
		return "", ctx.Err()
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		if code := exitErr.ExitCode(); code != 1 || !benignExit1(command) {
			return "", fmt.Errorf("exit code %d\n[stderr]\n%s", code, output)
		}
	default:
		return "", err
	}
	return output, nil
}

var benignCommands = []string{"grep", "rg", "egrep", "fgrep", "find", "diff", "test", "[", "git diff", "git grep"}

func benignExit1(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	name := fields[0]
	if name == "git" && len(fields) > 1 {
		name += " " + fields[1]
	}
	return slices.Contains(benignCommands, name)
}

// capOutput keeps the start of long output: injected context reads top down.
func capOutput(s string) string {
	if len(s) <= maxCommandOutput {
		return s
	}
	cut := maxCommandOutput
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return fmt.Sprintf("%s\n[output truncated: last %d bytes omitted]", s[:cut], len(s)-cut)
}
