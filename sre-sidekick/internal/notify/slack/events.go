package slack

import (
	"strings"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// The types in this file are the adapter's own narrow view of an inbound Slack
// event. The handler layer works with these rather than with slack-go's
// structs, so the conversation logic is not coupled to the library's JSON
// shapes and can be tested by constructing a three-field struct.

// Message is a human message posted in a channel or thread.
type Message struct {
	// EventID is Slack's id for the delivery, used to drop duplicates.
	EventID string
	// ChannelID and ThreadTS together identify the session: a reply in a
	// thread carries the root message's timestamp, which is the session key.
	ChannelID string
	ThreadTS  string
	// MessageTS is this message's own timestamp.
	MessageTS string
	UserID    string
	Text      string
	TeamID    string
	// FromBot marks a message posted by a bot, including this one. Acting on
	// these would make the sidekick answer itself in a loop.
	FromBot bool
}

// InThread reports whether the message is a threaded reply rather than a new
// top-level message. Only threaded replies can be routed to a session.
func (m Message) InThread() bool {
	return strings.TrimSpace(m.ThreadTS) != "" && m.ThreadTS != m.MessageTS
}

// Interaction is a Block Kit button click.
type Interaction struct {
	// EnvelopeID is Slack's id for the delivery, used to drop duplicates.
	EnvelopeID string
	// ActionID is one of the ActionApprove/ActionDecline/ActionClose
	// constants.
	ActionID string
	// Value is the button's payload, which carries the correlation id.
	Value string
	// ChannelID and ThreadTS identify the session. A button lives on the
	// diagnosis message, which is the thread root, so ThreadTS is that
	// message's timestamp.
	ChannelID string
	ThreadTS  string
	MessageTS string
	UserID    string
	UserName  string
	TeamID    string
	// ResponseURL can be used to reply or update the message without a bot
	// token; kept for the handler layer.
	ResponseURL string
}

// DiagnoseCommand is the only slash command this adapter answers (PRD section
// 14). It is the name registered in the Slack app manifest, e.g.
// `/diagnose support-agent`.
const DiagnoseCommand = "/diagnose"

// Command is a slash command invocation, e.g. `/diagnose support-agent`.
//
// Slash commands are deliberately used only to *start* a session. They cannot
// carry a thread timestamp, so they can never identify an existing thread -
// which is why closing a session is a button, not an `/end` command (session
// design section 2.1, edge case E1).
type Command struct {
	EnvelopeID  string
	Command     string
	Text        string
	ChannelID   string
	ChannelName string
	UserID      string
	UserName    string
	TeamID      string
	ResponseURL string
	TriggerID   string
}

// messageFrom converts an Events API callback into a Message, reporting
// whether it was a message event at all.
func messageFrom(event slackevents.EventsAPIEvent, eventID string) (Message, bool) {
	inner, ok := event.InnerEvent.Data.(*slackevents.MessageEvent)
	if !ok {
		return Message{}, false
	}

	return Message{
		EventID:   eventID,
		ChannelID: inner.Channel,
		ThreadTS:  inner.ThreadTimeStamp,
		MessageTS: inner.TimeStamp,
		UserID:    inner.User,
		Text:      inner.Text,
		TeamID:    event.TeamID,
		// A message is from a bot when Slack tags it with a bot id or the
		// bot_message subtype. Either marker is enough.
		FromBot: strings.TrimSpace(inner.BotID) != "" || inner.SubType == "bot_message",
	}, true
}

// interactionFrom converts a block-actions callback into an Interaction. Only
// button clicks are of interest; other interaction types are ignored.
func interactionFrom(callback slack.InteractionCallback, envelopeID string) (Interaction, bool) {
	if callback.Type != slack.InteractionTypeBlockActions {
		return Interaction{}, false
	}
	if len(callback.ActionCallback.BlockActions) == 0 {
		return Interaction{}, false
	}

	action := callback.ActionCallback.BlockActions[0]
	threadTS := strings.TrimSpace(callback.Message.ThreadTimestamp)
	if threadTS == "" {
		// The diagnosis message is the thread root, so its own timestamp is
		// the thread key.
		threadTS = callback.Message.Timestamp
	}
	if threadTS == "" {
		threadTS = callback.Container.ThreadTs
	}
	if threadTS == "" {
		threadTS = callback.Container.MessageTs
	}

	channelID := callback.Channel.ID
	if channelID == "" {
		channelID = callback.Container.ChannelID
	}

	return Interaction{
		EnvelopeID:  envelopeID,
		ActionID:    action.ActionID,
		Value:       action.Value,
		ChannelID:   channelID,
		ThreadTS:    threadTS,
		MessageTS:   callback.Container.MessageTs,
		UserID:      callback.User.ID,
		UserName:    callback.User.Name,
		TeamID:      callback.Team.ID,
		ResponseURL: callback.ResponseURL,
	}, true
}

func commandFrom(cmd slack.SlashCommand, envelopeID string) Command {
	return Command{
		EnvelopeID:  envelopeID,
		Command:     cmd.Command,
		Text:        strings.TrimSpace(cmd.Text),
		ChannelID:   cmd.ChannelID,
		ChannelName: cmd.ChannelName,
		UserID:      cmd.UserID,
		UserName:    cmd.UserName,
		TeamID:      cmd.TeamID,
		ResponseURL: cmd.ResponseURL,
		TriggerID:   cmd.TriggerID,
	}
}
