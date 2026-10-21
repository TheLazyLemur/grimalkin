package core

import "encoding/json"

// Event is something to show in a transcript. The number of the agent it
// belongs to is passed beside it, to Transcript.Show.
type Event interface{ isEvent() }

// Turn events: what an agent's own turn does.

type ReplyStarted struct{}

type ReplyText struct{ Text string }

type ToolStarted struct {
	ID, Name, Summary string
	Input             json.RawMessage // the call's, as the model wrote it
}

type ToolFinished struct {
	ID      string
	Output  string
	IsError bool
}

type TurnDiscarded struct{ Reason string }

// SkillLoaded reports a skill the user loaded with /name. Content is what the
// model receives.
type SkillLoaded struct{ Name, Content string }

// Lifecycle events: what happens to a child, shown in the transcript of the
// agent that started it. Child is the child's number.

// ChildWaiting reports a child that has no slot to run in yet.
type ChildWaiting struct {
	Child int
	Model string
}

// ChildStarted reports a child that has a slot and is running.
type ChildStarted struct {
	Child int
	Model string
}

// ChildProgress reports how many tool calls a child has made so far.
type ChildProgress struct {
	Child int
	Tools int
}

type ChildFinished struct{ Child int }

type ChildFailed struct {
	Child  int
	Reason string
}

type ChildCancelled struct{ Child int }

func (ReplyStarted) isEvent()   {}
func (ReplyText) isEvent()      {}
func (ToolStarted) isEvent()    {}
func (ToolFinished) isEvent()   {}
func (TurnDiscarded) isEvent()  {}
func (SkillLoaded) isEvent()    {}
func (ChildWaiting) isEvent()   {}
func (ChildStarted) isEvent()   {}
func (ChildProgress) isEvent()  {}
func (ChildFinished) isEvent()  {}
func (ChildFailed) isEvent()    {}
func (ChildCancelled) isEvent() {}
