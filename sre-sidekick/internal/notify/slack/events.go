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
	// ChannelType distinguishes a channel message ("channel") from a direct
	// message ("im"), which are routed differently: a DM is a question for
	// the sidekick by definition, while a channel message is only
	// interesting inside a session thread.
	ChannelType string
}

// InThread reports whether the message is a threaded reply rather than a new
// top-level message. Only threaded replies can be routed to a session.
func (m Message) InThread() bool {
	return strings.TrimSpace(m.ThreadTS) != "" && m.ThreadTS != m.MessageTS
}

// Mention is a direct address to the sidekick outside any incident it
// started: an @mention in a channel or thread, or a direct message.
//
// It is a separate type from Message because the routing rules are
// genuinely different, not merely a variation. A Message is only ever
// interesting inside a session thread; a Mention is interesting anywhere,
// is answered from live state rather than from a frozen diagnosis, and
// carries text that has already had the bot's own handle stripped off it.
type Mention struct {
	// TurnID identifies the human turn rather than the delivery. Slack
	// sends a mention inside a thread as BOTH an app_mention and a message
	// event, with different event ids, so deduplicating on the delivery id
	// would answer the same question twice. Channel, timestamp and user
	// together identify the turn itself, which is what must happen once.
	TurnID    string
	ChannelID string
	ThreadTS  string
	MessageTS string
	UserID    string
	// Text is the question with the leading <@BOTID> handle removed.
	Text   string
	TeamID string
	// DirectMessage marks a message in a DM conversation, where there is no
	// handle to strip and no channel context to scope by.
	DirectMessage bool
	// FromBot marks a mention posted by a bot, including this one.
	FromBot bool
}

// InThread reports whether the mention arrived as a threaded reply.
func (m Mention) InThread() bool {
	return strings.TrimSpace(m.ThreadTS) != "" && m.ThreadTS != m.MessageTS
}

// ReplyThreadTS is the thread the answer belongs in. A mention inside a
// thread is answered in that thread; a top-level mention is answered in a
// new thread hanging off it, which keeps a busy channel readable and gives
// any future follow-up a thread key to hold on to.
func (m Mention) ReplyThreadTS() string {
	if m.InThread() {
		return m.ThreadTS
	}
	return m.MessageTS
}

// turnID builds the delivery-independent identity of one human turn.
func turnID(channelID, messageTS, userID string) string {
	return "turn:" + channelID + "|" + messageTS + "|" + userID
}

// stripHandle removes a leading <@U123456> mention from the text.
//
// The bot's own user id is deliberately not consulted: any leading handle
// is stripped, so the adapter needs no auth.test call at startup and no id
// in config. A handle in the middle of a sentence is left alone, because
// "ask @alice" is part of the question, not addressing.
func stripHandle(text string) string {
	trimmed := strings.TrimSpace(text)
	for strings.HasPrefix(trimmed, "<@") {
		end := strings.Index(trimmed, ">")
		if end < 0 {
			break
		}
		trimmed = strings.TrimSpace(trimmed[end+1:])
	}
	return trimmed
}

func leadingHandleID(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "<@") {
		return ""
	}
	end := strings.Index(trimmed, ">")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(trimmed[2:end])
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
		EventID:     eventID,
		ChannelType: inner.ChannelType,
		ChannelID:   inner.Channel,
		ThreadTS:    inner.ThreadTimeStamp,
		MessageTS:   inner.TimeStamp,
		UserID:      inner.User,
		Text:        inner.Text,
		TeamID:      event.TeamID,
		// A message is from a bot when Slack tags it with a bot id or the
		// bot_message subtype. Either marker is enough.
		FromBot: strings.TrimSpace(inner.BotID) != "" || inner.SubType == "bot_message",
	}, true
}

// mentionFrom converts an app_mention callback into a Mention.
func mentionFrom(event slackevents.EventsAPIEvent, _ string) (Mention, bool) {
	inner, ok := event.InnerEvent.Data.(*slackevents.AppMentionEvent)
	if !ok {
		return Mention{}, false
	}
	return Mention{
		TurnID:    turnID(inner.Channel, inner.TimeStamp, inner.User),
		ChannelID: inner.Channel,
		ThreadTS:  inner.ThreadTimeStamp,
		MessageTS: inner.TimeStamp,
		UserID:    inner.User,
		Text:      stripHandle(inner.Text),
		TeamID:    event.TeamID,
		FromBot:   strings.TrimSpace(inner.BotID) != "",
	}, true
}

// directMessageFrom converts a message.im event into a Mention.
//
// A DM carries no handle - nobody types "@sidekick" in a one-to-one
// conversation - so it never arrives as an app_mention. Treating it as a
// mention is what makes "talk to the bot in a DM" work at all.
func directMessageFrom(msg Message, channelType string) (Mention, bool) {
	if channelType != "im" {
		return Mention{}, false
	}
	return Mention{
		TurnID:        turnID(msg.ChannelID, msg.MessageTS, msg.UserID),
		ChannelID:     msg.ChannelID,
		ThreadTS:      msg.ThreadTS,
		MessageTS:     msg.MessageTS,
		UserID:        msg.UserID,
		Text:          stripHandle(msg.Text),
		TeamID:        msg.TeamID,
		DirectMessage: true,
		FromBot:       msg.FromBot,
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
