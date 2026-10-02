package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"grimalkin/internal/core"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeSkill(t *testing.T, dir, name, content string) string {
	t.Helper()
	writeFile(t, filepath.Join(dir, name, skillFile), content)
	return filepath.Join(dir, name)
}

func load(t *testing.T, dirs []string, projectDir string) (*Library, []string) {
	t.Helper()
	lib, warnings, err := Load(dirs, projectDir)
	if err != nil {
		t.Fatal(err)
	}
	return lib, warnings
}

func render(t *testing.T, body, arguments string) string {
	t.Helper()
	dir := t.TempDir()
	writeSkill(t, dir, "s", "---\ndescription: d\narguments: [issue, branch]\n---\n"+body)
	lib, _ := load(t, []string{dir}, dir)
	out, err := lib.Render(context.Background(), "s", arguments)
	if err != nil {
		t.Fatal(err)
	}
	inner := strings.TrimPrefix(out, "<skill_content name=\"s\">\n")
	inner, _, _ = strings.Cut(inner, "\n\nSkill directory: ")
	return inner
}

func TestSkillsAreReadFromTheConfiguredDirsWithEarlierDirsWinning(t *testing.T) {
	// given
	// ... a project dir given relative to the project, and a user dir with a same-named skill
	project, user := t.TempDir(), t.TempDir()
	writeSkill(t, filepath.Join(project, ".agents/skills"), "review", "---\nname: review\ndescription: Project review\n---\nbody")
	writeSkill(t, user, "review", "---\nname: review\ndescription: User review\n---\nbody")
	writeSkill(t, user, "lore", "---\ndescription: Old system\nwhen_to_use: When touching legacy code\nuser-invocable: false\n---\n")
	writeSkill(t, user, "deploy", "---\ndescription: Ship\ndisable-model-invocation: true\n---\n")
	writeFile(t, filepath.Join(user, "README.md"), "not a skill")

	// when
	// ... the library loads, project first
	lib, warnings := load(t, []string{".agents/skills", user, "missing"}, project)

	// then
	// ... the project's review wins, with a warning, and the flags carry through
	want := []core.Skill{
		{Name: "review", Description: "Project review", ModelInvocable: true, UserInvocable: true},
		{Name: "deploy", Description: "Ship", UserInvocable: true},
		{Name: "lore", Description: "Old system When touching legacy code", ModelInvocable: true},
	}
	if got := lib.List(); !slices.Equal(got, want) {
		t.Errorf("skills = %+v\nwant %+v", got, want)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], `"review"`) || !strings.Contains(warnings[0], "shadowed") {
		t.Errorf("warnings = %q", warnings)
	}
}

func TestAFileGivenAsASkillDirIsAnError(t *testing.T) {
	// given
	// ... a skill dir that is a file
	path := filepath.Join(t.TempDir(), "skills")
	writeFile(t, path, "")

	// when
	// ... the library loads
	_, _, err := Load([]string{path}, "/")

	// then
	// ... it fails
	if err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Errorf("err = %v", err)
	}
}

func TestLenientParsing(t *testing.T) {
	cases := []struct {
		name, dir, content string
		want               *core.Skill // nil: skipped
		warning            string
	}{
		{"no frontmatter uses the dir name and first line", "notes", "\n# Notes for the team\nmore", &core.Skill{Name: "notes", Description: "# Notes for the team", ModelInvocable: true, UserInvocable: true}, ""},
		{"an unquoted colon is retried quoted", "pdf", "---\ndescription: Use when: the user has PDFs\n---\n", &core.Skill{Name: "pdf", Description: "Use when: the user has PDFs", ModelInvocable: true, UserInvocable: true}, ""},
		{"a name off the spec loads with a warning", "Big_One", "---\ndescription: d\n---\n", &core.Skill{Name: "Big_One", Description: "d", ModelInvocable: true, UserInvocable: true}, "breaks the spec"},
		{"a name unlike its dir loads under the dir name with a warning", "dir", "---\nname: other\ndescription: d\n---\n", &core.Skill{Name: "dir", Description: "d", ModelInvocable: true, UserInvocable: true}, "doesn't match"},
		{"no description at all is skipped", "empty", "---\nname: empty\n---\n", nil, "no description"},
		{"broken yaml is skipped", "broken", "---\ndescription: [unclosed\n---\n", nil, "frontmatter"},
		{"a dir name with a space is skipped", "two words", "---\ndescription: d\n---\n", nil, "can't be typed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// given
			// ... one skill
			dir := t.TempDir()
			writeSkill(t, dir, tc.dir, tc.content)

			// when
			// ... the library loads
			lib, warnings := load(t, []string{dir}, dir)

			// then
			// ... it loads or is skipped, with the expected warning
			got := lib.List()
			if tc.want == nil && len(got) != 0 || tc.want != nil && (len(got) != 1 || got[0] != *tc.want) {
				t.Errorf("skills = %+v, want %+v", got, tc.want)
			}
			if tc.warning == "" && len(warnings) != 0 || tc.warning != "" && (len(warnings) != 1 || !strings.Contains(warnings[0], tc.warning)) {
				t.Errorf("warnings = %q, want one containing %q", warnings, tc.warning)
			}
		})
	}
}

func TestArgumentExpansion(t *testing.T) {
	cases := []struct{ name, body, arguments, want string }{
		{"all arguments as typed", "Fix $ARGUMENTS now", `12  "a b"`, `Fix 12  "a b" now`},
		{"indexed with shell quoting", "$0 / $ARGUMENTS[1] / $1", `x "a b" 'c'`, "x / a b / a b"},
		{"an index with no argument stays", "$0 and $3", "x", "x and $3"},
		{"named arguments map to positions", "issue $issue on $branch", "42", "issue 42 on "},
		{"undeclared names stay", "$HOME and $issues", "42", "$HOME and $issues\n\nARGUMENTS: 42"},
		{"unused arguments are appended", "No placeholders", "a b", "No placeholders\n\nARGUMENTS: a b"},
		{"only a missing index doesn't count as used", "Only $5", "a", "Only $5\n\nARGUMENTS: a"},
		{"no arguments, nothing appended", "Plain", "", "Plain"},
		{"a backslash escapes a placeholder", `costs \$1.00, \$issue, \$ARGUMENTS`, "a", "costs $1.00, $issue, $ARGUMENTS\n\nARGUMENTS: a"},
		{"a double backslash doesn't", `\\$0`, "a", `\\a`},
		{"a backslash before other $ stays", `\$x`, "", `\$x`},
		{"argument values aren't expanded again", "Summarize $0", `"$ARGUMENTS from $1"`, "Summarize $ARGUMENTS from $1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// given
			// ... a skill declaring arguments issue and branch

			// when
			// ... it renders with the arguments
			got := render(t, tc.body, tc.arguments)

			// then
			// ... placeholders expand as Claude Code expands them
			if got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestVariablesExpandEvenInsideArguments(t *testing.T) {
	// given
	// ... a skill using both variables, and an argument that names one
	dir, project := t.TempDir(), t.TempDir()
	skillDir := writeSkill(t, dir, "s", "---\ndescription: d\n---\n${GRIMALKIN_SKILL_DIR} ${GRIMALKIN_PROJECT_DIR} $0 ${GRIMALKIN_SESSION_ID}")
	lib, _ := load(t, []string{dir}, project)

	// when
	// ... it renders
	out, err := lib.Render(context.Background(), "s", "${GRIMALKIN_SKILL_DIR}/x")

	// then
	// ... both expand everywhere and unknown variables stay
	if err != nil {
		t.Fatal(err)
	}
	want := skillDir + " " + project + " " + skillDir + "/x ${GRIMALKIN_SESSION_ID}"
	if !strings.Contains(out, want) {
		t.Errorf("out = %q, want it to contain %q", out, want)
	}
}

func TestInjectedCommandsRunBeforeTheModelSeesTheSkill(t *testing.T) {
	cases := []struct{ name, body, arguments, want string }{
		{"inline at line start", "Diff:\n!`printf 'a\\nb\\n'`\nEnd", "", "Diff:\na\nb\nEnd"},
		{"inline after whitespace", "- branch: !`echo main`", "", "- branch: main"},
		{"not after other characters", "KEY=!`echo no`", "", "KEY=!`echo no`"},
		{"stderr is merged", "!`echo out; echo err >&2`", "", "out\nerr"},
		{"fenced block", "Env:\n```!\necho one\necho two\n```\nDone", "", "Env:\none\ntwo\nDone"},
		{"arguments and variables expand in the command", "!`echo $0-$issue`", "7", "7-7"},
		{"runs in the project dir", "!`basename \"$PWD\"`", "", "project"},
		{"output isn't scanned again", "!`printf '!\\140echo hi\\140'`", "", "!`echo hi`"},
		{"arguments can't add commands", "$ARGUMENTS", "!`echo hi`", "!`echo hi`"},
		{"exit 1 from grep is a result", "!`grep zzz /dev/null`", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// given
			// ... a skill with injected commands, in a project dir
			dir := t.TempDir()
			project := filepath.Join(t.TempDir(), "project")
			if err := os.Mkdir(project, 0o755); err != nil {
				t.Fatal(err)
			}
			writeSkill(t, dir, "s", "---\ndescription: d\narguments: issue\n---\n"+tc.body)
			lib, _ := load(t, []string{dir}, project)

			// when
			// ... it renders
			out, err := lib.Render(context.Background(), "s", tc.arguments)

			// then
			// ... each command's output replaced it
			if err != nil {
				t.Fatal(err)
			}
			got, _, _ := strings.Cut(strings.TrimPrefix(out, "<skill_content name=\"s\">\n"), "\n\nSkill directory: ")
			if got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestAFailedCommandAbortsTheRender(t *testing.T) {
	for _, command := range []string{"echo boom >&2; exit 3", "grep -q x /nonexistent-file", "false"} {
		t.Run(command, func(t *testing.T) {
			// given
			// ... a skill whose command fails
			dir := t.TempDir()
			writeSkill(t, dir, "s", "---\ndescription: d\n---\nBefore !`"+command+"`")
			lib, _ := load(t, []string{dir}, dir)

			// when
			// ... it renders
			_, err := lib.Render(context.Background(), "s", "")

			// then
			// ... the error names the pattern
			if err == nil || !strings.Contains(err.Error(), "shell command failed for pattern \"!`"+command) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestRenderWrapsTheBodyAndListsBundledFiles(t *testing.T) {
	// given
	// ... a skill with bundled files, a hidden one among them
	dir := t.TempDir()
	skillDir := writeSkill(t, dir, "pdf", "---\ndescription: d\nmetadata:\n  author: me\n---\n\n# PDF\nUse scripts/extract.py\n")
	writeFile(t, filepath.Join(skillDir, "scripts/extract.py"), "")
	writeFile(t, filepath.Join(skillDir, "references/SPEC.md"), "")
	writeFile(t, filepath.Join(skillDir, ".git/config"), "")
	lib, _ := load(t, []string{dir}, dir)

	// when
	// ... it renders
	out, err := lib.Render(context.Background(), "pdf", "")

	// then
	// ... the frontmatter is gone and the files are listed, not read
	if err != nil {
		t.Fatal(err)
	}
	want := "<skill_content name=\"pdf\">\n# PDF\nUse scripts/extract.py\n\nSkill directory: " + skillDir +
		"\nRelative paths in this skill are relative to the skill directory.\n\n<skill_resources>\n<file>references/SPEC.md</file>\n<file>scripts/extract.py</file>\n</skill_resources>\n</skill_content>"
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

func TestRenderReadsTheFileAfresh(t *testing.T) {
	// given
	// ... a loaded skill whose file then changes
	dir := t.TempDir()
	writeSkill(t, dir, "s", "---\ndescription: d\n---\nold")
	lib, _ := load(t, []string{dir}, dir)
	writeSkill(t, dir, "s", "---\ndescription: d\n---\nnew")

	// when
	// ... it renders
	out, err := lib.Render(context.Background(), "s", "")

	// then
	// ... the model gets the new body
	if err != nil || !strings.Contains(out, "\nnew\n") {
		t.Errorf("out = %q, err = %v", out, err)
	}
}

func TestSplitArguments(t *testing.T) {
	got := splitArguments(`a "b c" 'd "e"' f\ g "h\"i" ''`)
	want := []string{"a", "b c", `d "e"`, "f g", `h"i`, ""}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCancellingStopsAnInjectedCommand(t *testing.T) {
	// given
	// ... a skill with a slow command and a context already cancelled
	dir := t.TempDir()
	writeSkill(t, dir, "s", "---\ndescription: d\n---\n!`sleep 30`")
	lib, _ := load(t, []string{dir}, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// when
	// ... it renders
	_, err := lib.Render(ctx, "s", "")

	// then
	// ... it stops with the cancellation
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}
