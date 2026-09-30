package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestNewlineChordsAreTranslatedApartFromEnter(t *testing.T) {
	// given
	// ... Enter and the three newline chords as Bubble Tea reports them
	presses := []tea.KeyPressMsg{
		{Code: tea.KeyEnter},
		{Code: tea.KeyEnter, Mod: tea.ModShift},
		{Code: tea.KeyEnter, Mod: tea.ModAlt},
		{Code: 'j', Mod: tea.ModCtrl},
	}

	// when
	// ... each is translated
	var got []keyCode
	for _, p := range presses {
		_, code := translateKey(p)
		got = append(got, code)
	}

	// then
	// ... Enter sends and the chords type a newline
	want := []keyCode{keyEnter, keyNewline, keyNewline, keyNewline}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("press %d: got %d, want %d", i, got[i], want[i])
		}
	}
}
