package core

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNoTurn is Steer's answer when no turn is running.
var ErrNoTurn = errors.New("no turn is running")

// Steer holds text, a message the user sends while a turn runs, for the turn
// to take into the conversation as the user's: with the results of the tool
// round it arrives during, or, when it arrives during the call that would end
// the turn, as one more round. Once Steer returns nil the message is in the
// conversation however the turn ends: a discarded turn leaves its steers as
// the user's last message, which History is given and the next prompt joins.
// With no turn running it holds nothing and returns ErrNoTurn. A turn takes
// steers from StartTurn until it ends; a child's never does.
func (a *Agent) Steer(text string) error {
	if a.parent != nil {
		panic(fmt.Sprintf("agent %d steered", a.number))
	}
	if text == "" {
		panic("empty steer")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.steering {
		return ErrNoTurn
	}
	a.held = append(a.held, text)
	return nil
}

// takeSteers takes the held steers. With last set and none held, the turn
// stops taking them in the same step, so a steer is either taken or refused.
func (a *Agent) takeSteers(last bool) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	held := a.held
	a.held = nil
	if last && len(held) == 0 {
		a.steering = false
	}
	return held
}

// deliver shows each of steers delivered and returns them as one text.
func (a *Agent) deliver(steers []string) string {
	for _, s := range steers {
		a.show(SteerDelivered{s})
	}
	return joinTexts(steers...)
}

func joinTexts(texts ...string) string { return strings.Join(texts, "\n\n") }

// conversation is messages as the model is sent them. A user message that
// follows another's, as the steers of a discarded turn are followed by the
// next prompt, is joined to it, so the roles alternate.
func conversation(messages []Message) []Message {
	var joined []Message
	for _, m := range messages {
		n := len(joined)
		if n == 0 || m.Role != RoleUser || joined[n-1].Role != RoleUser {
			joined = append(joined, m)
			continue
		}
		if len(m.ToolResults) > 0 {
			panic("tool results in a user message that follows the user's")
		}
		joined[n-1].Text = joinTexts(joined[n-1].Text, m.Text)
	}
	return joined
}
