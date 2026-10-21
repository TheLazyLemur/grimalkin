package core

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"testing"
)

func TestASteerWhileNoTurnRunsIsRefusedAndNeverReachesTheModel(t *testing.T) {
	// given
	// ... an agent whose turn has ended
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A2")}},
	}}
	a := newAgent(model, &mockTranscript{})
	a.RunTurn("one")

	// when
	// ... the user steers, then prompts
	err := a.Steer("too late")
	a.RunTurn("two")

	// then
	// ... the steer is refused and the next turn's model call doesn't carry it
	if !errors.Is(err, ErrNoTurn) {
		t.Errorf("err = %v, want ErrNoTurn", err)
	}
	want := []Message{{Role: RoleUser, Text: "one"}, {Role: RoleAssistant, Content: raw("A1"), Model: parentModelName}, {Role: RoleUser, Text: "two"}}
	if !reflect.DeepEqual(model.requests[1], want) {
		t.Errorf("request = %+v\nwant %+v", model.requests[1], want)
	}
}

func TestASteerDuringAToolRoundReachesTheNextModelCallWithTheResults(t *testing.T) {
	// given
	// ... a model that calls a tool once, and a tool during which the user steers
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "list", `{}`)}, Content: raw("A1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A2")}},
	}}
	transcript := &mockTranscript{}
	a := newAgent(model, transcript)
	var steerErr error
	a.Tools = []Tool{&mockTool{name: "list", run: func(context.Context) (string, error) {
		steerErr = a.Steer("also check b")
		return "a.txt", nil
	}}}

	// when
	// ... a turn runs
	a.RunTurn("list files")

	// then
	// ... the steer was taken, the next model call got it after the tool's result, and it was shown delivered before that call
	if steerErr != nil {
		t.Fatalf("steer: %v", steerErr)
	}
	want := Message{Role: RoleUser, ToolResults: []ToolResult{{ToolUseID: "toolu_1", Content: "a.txt"}}, Text: "also check b"}
	if got := model.requests[1][2]; !reflect.DeepEqual(got, want) {
		t.Errorf("message = %+v\nwant %+v", got, want)
	}
	wantEvents := []Event{
		ReplyStarted{},
		ToolStarted{ID: "toolu_1", Name: "list", Summary: "list {}", Input: json.RawMessage(`{}`)},
		ToolFinished{ID: "toolu_1", Output: "a.txt"},
		SteerDelivered{Text: "also check b"},
		ReplyStarted{},
	}
	if !reflect.DeepEqual(transcript.events, wantEvents) {
		t.Errorf("events = %#v\nwant %#v", transcript.events, wantEvents)
	}
}

func TestASteerDuringTheFinalModelCallRunsOneMoreRoundThatSeesIt(t *testing.T) {
	// given
	// ... a model that ends its turn, during whose first call the user steers
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A2")}},
	}}
	a := newAgent(model, &mockTranscript{})
	history := &mockHistory{}
	a.History = history
	var once sync.Once
	var steerErr error
	model.hold = func(context.Context, ModelRequest) error {
		once.Do(func() { steerErr = a.Steer("and b") })
		return nil
	}

	// when
	// ... a turn runs
	a.RunTurn("do a")

	// then
	// ... the model was called again with the steer as the user's, and the turn kept both rounds
	if steerErr != nil {
		t.Fatalf("steer: %v", steerErr)
	}
	want := []Message{
		{Role: RoleUser, Text: "do a"},
		{Role: RoleAssistant, Content: raw("A1"), Model: parentModelName},
		{Role: RoleUser, Text: "and b"},
	}
	if len(model.requests) != 2 || !reflect.DeepEqual(model.requests[1], want) {
		t.Fatalf("requests = %+v\nwant the second to be %+v", model.requests, want)
	}
	if wantKept := append(want, Message{Role: RoleAssistant, Content: raw("A2"), Model: parentModelName}); !reflect.DeepEqual(history.kept, wantKept) {
		t.Errorf("kept = %+v\nwant %+v", history.kept, wantKept)
	}
}

// steerTools are a tool that steers "first" and one that steers "second"
// and then cancels the turn, so a turn calling one and then the other takes
// a steer between rounds and is cancelled holding another.
func steerTools(a *Agent) []Tool {
	return []Tool{
		&mockTool{name: "note", run: func(context.Context) (string, error) {
			return "ok", a.Steer("first")
		}},
		&mockTool{name: "stop", run: func(ctx context.Context) (string, error) {
			err := a.Steer("second")
			a.Cancel()
			return "", errors.Join(err, await(ctx.Done()))
		}},
	}
}

func TestSteersOfACancelledTurnAreInTheNextTurnsFirstModelCall(t *testing.T) {
	// given
	// ... a turn that takes one steer between rounds and is cancelled holding another
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "note", `{}`)}, Content: raw("A1")}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_2", "stop", `{}`)}, Content: raw("A2")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A3")}},
	}}
	transcript := &mockTranscript{}
	a := newAgent(model, transcript)
	a.Tools = steerTools(a)
	a.RunTurn("one")

	// when
	// ... the user prompts again
	a.RunTurn("two")

	// then
	// ... the first turn was discarded, and the next model call has both steers, in order, before the prompt
	if !slices.Contains(transcript.events, Event(TurnDiscarded{Reason: "cancelled"})) {
		t.Errorf("events = %#v, want the turn discarded", transcript.events)
	}
	want := []Message{{Role: RoleUser, Text: "first\n\nsecond\n\ntwo"}}
	if !reflect.DeepEqual(model.requests[2], want) {
		t.Errorf("request = %+v\nwant %+v", model.requests[2], want)
	}
}

func TestSteersOfACancelledTurnAreRecordedSoASeededAgentCarriesThem(t *testing.T) {
	// given
	// ... the history of a session whose only turn took a steer and was cancelled holding another
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "note", `{}`)}, Content: raw("A1")}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_2", "stop", `{}`)}, Content: raw("A2")}},
	}}
	a := newAgent(model, &mockTranscript{})
	a.Tools = steerTools(a)
	history := &mockHistory{}
	a.History = history
	a.RunTurn("one")
	resumed := &mockModel{replies: []mockReply{{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("B1")}}}}
	b := newAgent(resumed, &mockTranscript{})

	// when
	// ... a new agent is seeded from that history and prompted
	err := b.Seed(history.kept)
	b.RunTurn("two")

	// then
	// ... the history holds the steers as the user's message, and the seeded agent's model call has them before the prompt
	if want := []Message{{Role: RoleUser, Text: "first\n\nsecond"}}; !reflect.DeepEqual(history.kept, want) {
		t.Errorf("kept = %+v\nwant %+v", history.kept, want)
	}
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if want := []Message{{Role: RoleUser, Text: "first\n\nsecond\n\ntwo"}}; !reflect.DeepEqual(resumed.requests[0], want) {
		t.Errorf("request = %+v\nwant %+v", resumed.requests[0], want)
	}
}

func TestASteerRacingTheEndOfTheTurnIsEitherRefusedOrDelivered(t *testing.T) {
	// given
	// ... many turns that end at once, each with a steer sent a little later than the one before, up to a point
	for i := range 500 {
		model := &mockModel{replies: []mockReply{
			{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A1")}},
			{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A2")}},
		}}
		a := newAgent(model, &mockTranscript{})
		history := &mockHistory{}
		a.History = history
		run := a.StartTurn("go")
		done := make(chan struct{})

		// when
		// ... the user steers while the turn ends
		go func() { run(); close(done) }()
		for range i % 20 {
			runtime.Gosched()
		}
		err := a.Steer("late")
		<-done

		// then
		// ... a steer that was accepted is in the kept history, and one that was refused is not
		delivered := slices.ContainsFunc(history.kept, func(m Message) bool { return m.Text == "late" })
		if delivered != (err == nil) {
			t.Fatalf("steer err = %v, delivered = %v", err, delivered)
		}
	}
}
