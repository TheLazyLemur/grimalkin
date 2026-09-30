package core

type Event interface{ isEvent() }

type ReplyStarted struct{}

type ReplyText struct{ Text string }

type ToolStarted struct {
	ID, Name, Summary string
}

type ToolFinished struct {
	ID      string
	Output  string
	IsError bool
}

type TurnDiscarded struct{ Reason string }

func (ReplyStarted) isEvent()  {}
func (ReplyText) isEvent()     {}
func (ToolStarted) isEvent()   {}
func (ToolFinished) isEvent()  {}
func (TurnDiscarded) isEvent() {}
