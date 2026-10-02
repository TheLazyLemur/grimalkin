package skills

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// maxDescription caps description plus when_to_use in the catalog, as
// Claude Code does.
const maxDescription = 1536

// frontmatter holds the fields grimalkin acts on. Others, from the spec or
// from Claude Code, are ignored.
type frontmatter struct {
	Name                   string   `yaml:"name"`
	Description            string   `yaml:"description"`
	WhenToUse              string   `yaml:"when_to_use"`
	Arguments              argNames `yaml:"arguments"`
	DisableModelInvocation bool     `yaml:"disable-model-invocation"`
	UserInvocable          *bool    `yaml:"user-invocable"`
}

// argNames is a space-separated string or a list.
type argNames []string

func (a *argNames) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*a = strings.Fields(node.Value)
		return nil
	case yaml.SequenceNode:
		var names []string
		if err := node.Decode(&names); err != nil {
			return err
		}
		*a = names
		return nil
	}
	return errors.New("arguments: want a string or a list")
}

var nameRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func validName(name string) bool { return len(name) <= 64 && nameRE.MatchString(name) }

// parse splits SKILL.md into frontmatter and trimmed body. Frontmatter is
// optional. YAML that only fails because of an unquoted ": " in a value is
// retried with that value quoted, since other clients accept it.
func parse(content string) (frontmatter, string, error) {
	content = strings.TrimPrefix(content, "\uFEFF")
	content = strings.ReplaceAll(content, "\r\n", "\n")
	rest, ok := strings.CutPrefix(content, "---\n")
	if !ok {
		return frontmatter{}, strings.TrimSpace(content), nil
	}
	var block, body string
	if strings.HasPrefix(rest, "---\n") || rest == "---" {
		block, body = "", strings.TrimPrefix(rest, "---")
	} else {
		var found bool
		block, body, found = strings.Cut(rest, "\n---\n")
		if !found {
			block, found = strings.CutSuffix(rest, "\n---")
			if !found {
				return frontmatter{}, "", errors.New("frontmatter has no closing ---")
			}
			body = ""
		}
	}
	var f frontmatter
	if err := yaml.Unmarshal([]byte(block), &f); err != nil {
		f = frontmatter{}
		if retryErr := yaml.Unmarshal([]byte(quoteColonValues(block)), &f); retryErr != nil {
			return frontmatter{}, "", fmt.Errorf("frontmatter: %w", err)
		}
	}
	return f, strings.TrimSpace(body), nil
}

var plainValueRE = regexp.MustCompile(`^([A-Za-z_][\w-]*):[ \t]+([^'"\s\[{|>&*!#%@].*)$`)

// quoteColonValues single-quotes top-level plain values that contain ": ".
func quoteColonValues(block string) string {
	lines := strings.Split(block, "\n")
	for i, line := range lines {
		m := plainValueRE.FindStringSubmatch(line)
		if m == nil || !strings.Contains(m[2], ": ") {
			continue
		}
		lines[i] = m[1] + ": '" + strings.ReplaceAll(strings.TrimRight(m[2], " \t"), "'", "''") + "'"
	}
	return strings.Join(lines, "\n")
}
