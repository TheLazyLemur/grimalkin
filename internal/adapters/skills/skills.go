// Package skills is the skill library: Agent Skills (agentskills.io)
// directories found under the configured skill dirs, rendered on activation
// the way Claude Code renders them.
package skills

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"grimalkin/internal/core"
)

const skillFile = "SKILL.md"

type skill struct {
	core.Skill
	dir string // absolute
}

type Library struct {
	projectDir string
	skills     []skill
}

// Load reads the catalog from dirs, in order: a skill in an earlier dir
// shadows one with the same name in a later dir. A leading ~ is the home
// directory and a relative dir is relative to projectDir. A dir that doesn't
// exist is skipped; one that exists but isn't a directory is an error.
// Skills that can't be loaded are skipped with a warning.
func Load(dirs []string, projectDir string) (*Library, []string, error) {
	lib := &Library{projectDir: projectDir}
	var warnings []string
	where := map[string]string{}
	for _, dir := range dirs {
		abs, err := resolveDir(dir, projectDir)
		if err != nil {
			return nil, nil, err
		}
		info, err := os.Stat(abs)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("skill dir %s: %w", dir, err)
		}
		if !info.IsDir() {
			return nil, nil, fmt.Errorf("skill dir %s is not a directory", dir)
		}
		entries, err := os.ReadDir(abs)
		if err != nil {
			return nil, nil, fmt.Errorf("skill dir %s: %w", dir, err)
		}
		for _, entry := range entries {
			skillDir := filepath.Join(abs, entry.Name())
			if strings.HasPrefix(entry.Name(), ".") || !isSkillDir(skillDir) {
				continue
			}
			s, problems, err := loadSkill(skillDir)
			for _, p := range problems {
				warnings = append(warnings, fmt.Sprintf("skill %s: %s", skillDir, p))
			}
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("skill %s skipped: %v", skillDir, err))
				continue
			}
			if first, ok := where[s.Name]; ok {
				warnings = append(warnings, fmt.Sprintf("skill %q in %s is shadowed by %s", s.Name, skillDir, first))
				continue
			}
			where[s.Name] = skillDir
			lib.skills = append(lib.skills, s)
		}
	}
	return lib, warnings, nil
}

func resolveDir(dir, projectDir string) (string, error) {
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("skill dir %s: %w", dir, err)
		}
		dir = filepath.Join(home, dir[1:])
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(projectDir, dir)
	}
	return filepath.Clean(dir), nil
}

func isSkillDir(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, skillFile))
	return err == nil && info.Mode().IsRegular()
}

// loadSkill reads a skill's catalog entry. problems are worth telling the
// user about but don't stop the skill loading.
func loadSkill(dir string) (s skill, problems []string, err error) {
	f, body, err := readSkill(dir)
	if err != nil {
		return skill{}, nil, err
	}
	// The directory names the skill, as in Claude Code.
	name := filepath.Base(dir)
	if strings.ContainsFunc(name, func(r rune) bool { return r <= ' ' }) {
		return skill{}, nil, fmt.Errorf("name %q can't be typed as /name", name)
	}
	if !validName(name) {
		problems = append(problems, fmt.Sprintf("name %q breaks the spec: 1-64 of a-z, 0-9 and single inner hyphens", name))
	}
	if f.Name != "" && f.Name != name {
		problems = append(problems, fmt.Sprintf("frontmatter name %q doesn't match its directory; using %q", f.Name, name))
	}
	description := strings.TrimSpace(strings.Join(nonEmpty(f.Description, f.WhenToUse), " "))
	if description == "" {
		description = firstLine(body)
	}
	if description == "" {
		return skill{}, nil, errors.New("no description")
	}
	return skill{
		Skill: core.Skill{
			Name:           name,
			Description:    truncateRunes(description, maxDescription),
			ModelInvocable: !f.DisableModelInvocation,
			UserInvocable:  f.UserInvocable == nil || *f.UserInvocable,
		},
		dir: dir,
	}, problems, nil
}

func readSkill(dir string) (frontmatter, string, error) {
	data, err := os.ReadFile(filepath.Join(dir, skillFile))
	if err != nil {
		return frontmatter{}, "", err
	}
	return parse(string(data))
}

func (l *Library) List() []core.Skill {
	list := make([]core.Skill, len(l.skills))
	for i, s := range l.skills {
		list[i] = s.Skill
	}
	return list
}

// Render reads the skill's SKILL.md afresh, expands it and wraps it in a
// <skill_content> block naming its directory and bundled files.
func (l *Library) Render(ctx context.Context, name, arguments string) (string, error) {
	var s *skill
	for i := range l.skills {
		if l.skills[i].Name == name {
			s = &l.skills[i]
			break
		}
	}
	if s == nil {
		return "", fmt.Errorf("unknown skill %q", name)
	}
	f, body, err := readSkill(s.dir)
	if err != nil {
		return "", err
	}
	r := renderer{
		arguments:  arguments,
		positional: splitArguments(arguments),
		names:      f.Arguments,
		vars: strings.NewReplacer(
			"${GRIMALKIN_SKILL_DIR}", s.dir,
			"${GRIMALKIN_PROJECT_DIR}", l.projectDir,
		),
		projectDir: l.projectDir,
	}
	rendered, err := r.render(ctx, body)
	if err != nil {
		return "", err
	}
	return wrap(name, s.dir, rendered), nil
}

const maxResources = 50

func wrap(name, dir, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<skill_content name=%q>\n%s\n\nSkill directory: %s\nRelative paths in this skill are relative to the skill directory.\n", name, body, dir)
	files := resources(dir)
	if len(files) > 0 {
		b.WriteString("\n<skill_resources>\n")
		for i, f := range files {
			if i == maxResources {
				fmt.Fprintf(&b, "(%d more files not listed)\n", len(files)-maxResources)
				break
			}
			fmt.Fprintf(&b, "<file>%s</file>\n", f)
		}
		b.WriteString("</skill_resources>\n")
	}
	b.WriteString("</skill_content>")
	return b.String()
}

// resources lists the skill's bundled files, without reading them. Hidden
// files and directories are left out.
func resources(dir string) []string {
	var files []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path != dir && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		if d.IsDir() || rel == skillFile {
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files
}

func nonEmpty(values ...string) []string {
	var kept []string
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			kept = append(kept, strings.TrimSpace(v))
		}
	}
	return kept
}

func firstLine(body string) string {
	for line := range strings.Lines(body) {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit-1]) + "…"
}
