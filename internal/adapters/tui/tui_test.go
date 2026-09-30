package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

type fakeAgent struct {
	prompts   []string
	cancelled int
}

func (f *fakeAgent) RunTurn(prompt string) { f.prompts = append(f.prompts, prompt) }
func (f *fakeAgent) Cancel()               { f.cancelled++ }

type nopClipboard struct{}

func (nopClipboard) Copy(string) {}

func typeText(m *model, text string) {
	for _, char := range text {
		m.Update(tea.KeyPressMsg{Code: char, Text: string(char)})
	}
}

func TestCtrlCCancelsARunningTurnAndQuitsOnceItHasFinished(t *testing.T) {
	// given
	// ... a prompt sent to the agent
	agent := &fakeAgent{}
	m := newModel(agent, NewStore(), nopClipboard{}, "fake")
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	typeText(m, "hi")
	_, runTurn := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	// when
	// ... Ctrl-C is pressed during the turn, the turn finishes, and Ctrl-C is pressed again
	_, duringTurn := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m.Update(runTurn())
	_, afterTurn := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})

	// then
	// ... the turn ran with the prompt, the first Ctrl-C cancelled it without quitting, and the second quit
	if len(agent.prompts) != 1 || agent.prompts[0] != "hi" {
		t.Errorf("prompts = %q", agent.prompts)
	}
	if agent.cancelled != 1 || duringTurn != nil {
		t.Errorf("cancelled %d times, command during turn %v", agent.cancelled, duringTurn)
	}
	if afterTurn == nil {
		t.Fatal("no command after the turn")
	}
	if _, quit := afterTurn().(tea.QuitMsg); !quit {
		t.Errorf("after the turn Ctrl-C gave %T, want tea.QuitMsg", afterTurn())
	}
}
