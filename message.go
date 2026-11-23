package main

import (
	"context"
	"strings"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type Media struct {
	URL       string `json:"mxc_url"`
	MimeType  string `json:"mime_type,omitempty"`
	Filename  string `json:"filename,omitempty"`
	Size      int    `json:"size,omitempty"`
	Encrypted bool   `json:"encrypted,omitempty"`
}

type Message struct {
	EventID    string              `json:"message_id"`
	RoomID     string              `json:"-"`
	Time       string              `json:"time"`
	TS         int64               `json:"-"`
	Sender     string              `json:"sender"`
	SenderName string              `json:"sender_name,omitempty"`
	IsMe       bool                `json:"from_me,omitempty"`
	Kind       string              `json:"kind"`
	Body       string              `json:"body,omitempty"`
	ReplyTo    string              `json:"reply_to,omitempty"`
	ThreadRoot string              `json:"thread_root,omitempty"`
	Edited     bool                `json:"edited,omitempty"`
	Reactions  map[string][]string `json:"reactions,omitempty"`
	Media      *Media              `json:"media,omitempty"`

	// Relations, consumed by aggregation and not shown directly.
	Edits   string `json:"edits,omitempty"`
	Redacts string `json:"-"`
	ReactTo string `json:"reaction_to,omitempty"`
}

func fmtTS(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

func (app *App) displayName(ctx context.Context, roomID id.RoomID, userID id.UserID) string {
	if app.cli.StateStore != nil {
		if member, err := app.cli.StateStore.GetMember(ctx, roomID, userID); err == nil && member != nil && member.Displayname != "" {
			return member.Displayname
		}
	}
	localpart, _, _ := userID.Parse()
	return localpart
}

func stripReplyFallback(body string) string {
	if !strings.HasPrefix(body, "> ") {
		return body
	}
	lines := strings.Split(body, "\n")
	i := 0
	for i < len(lines) && strings.HasPrefix(lines[i], ">") {
		i++
	}
	if i < len(lines) && lines[i] == "" {
		i++
	}
	return strings.Join(lines[i:], "\n")
}

// toMessage converts a (decrypted) timeline event into a compact message. Returns nil for events not worth showing.
func (app *App) toMessage(ctx context.Context, evt *event.Event) *Message {
	m := &Message{
		EventID: evt.ID.String(),
		RoomID:  evt.RoomID.String(),
		TS:      evt.Timestamp,
		Time:    fmtTS(evt.Timestamp),
		Sender:  evt.Sender.String(),
		IsMe:    evt.Sender == app.cli.UserID,
	}
	m.SenderName = app.displayName(ctx, evt.RoomID, evt.Sender)
	if evt.Unsigned.RedactedBecause != nil {
		m.Kind = "deleted"
		return m
	}
	switch evt.Type {
	case event.EventMessage, event.EventSticker:
		content, ok := evt.Content.Parsed.(*event.MessageEventContent)
		if !ok {
			return nil
		}
		if evt.Type == event.EventSticker {
			m.Kind = "sticker"
		} else {
			m.Kind = strings.TrimPrefix(string(content.MsgType), "m.")
		}
		if rel := content.RelatesTo; rel != nil {
			if rel.Type == event.RelReplace && rel.EventID != "" {
				m.Kind = "edit"
				m.Edits = rel.EventID.String()
				if content.NewContent != nil {
					m.Body = content.NewContent.Body
				} else {
					m.Body = strings.TrimPrefix(content.Body, "* ")
				}
				return m
			}
			if reply := rel.GetNonFallbackReplyTo(); reply != "" { // thread events carry a fallback reply for clients without threads
				m.ReplyTo = reply.String()
			}
			if rel.Type == event.RelThread {
				m.ThreadRoot = rel.EventID.String()
			}
		}
		m.Body = stripReplyFallback(content.Body)
		switch content.MsgType {
		case event.MsgImage, event.MsgVideo, event.MsgAudio, event.MsgFile:
			media := &Media{Filename: content.FileName}
			if media.Filename == "" {
				media.Filename = content.Body
			}
			if content.File != nil {
				media.URL = string(content.File.URL)
				media.Encrypted = true
			} else {
				media.URL = string(content.URL)
			}
			if content.Info != nil {
				media.MimeType = content.Info.MimeType
				media.Size = content.Info.Size
			}
			m.Media = media
			if content.FileName != "" && content.FileName != content.Body {
				m.Body = content.Body // body is the caption
			} else {
				m.Body = ""
			}
		case event.MsgLocation:
			m.Body = strings.TrimSpace(content.Body + " " + content.GeoURI)
		}
	case event.EventReaction:
		content, ok := evt.Content.Parsed.(*event.ReactionEventContent)
		if !ok {
			return nil
		}
		m.Kind = "reaction"
		m.Body = content.RelatesTo.Key
		m.ReactTo = content.RelatesTo.EventID.String()
	case event.EventRedaction:
		m.Kind = "redaction"
		if content, ok := evt.Content.Parsed.(*event.RedactionEventContent); ok && content.Redacts != "" {
			m.Redacts = content.Redacts.String()
		} else {
			m.Redacts = evt.Redacts.String()
		}
	case event.EventEncrypted:
		m.Kind = "undecryptable"
		m.Body = "[encrypted message - keys not available to this device]"
	case event.StateMember:
		content, ok := evt.Content.Parsed.(*event.MemberEventContent)
		if !ok {
			return nil
		}
		m.Kind = "membership"
		target := ""
		if evt.StateKey != nil {
			target = *evt.StateKey
		}
		m.Body = target + " " + string(content.Membership)
	case event.StateRoomName:
		m.Kind = "room_name"
		if content, ok := evt.Content.Parsed.(*event.RoomNameEventContent); ok {
			m.Body = content.Name
		}
	case event.StateTopic:
		m.Kind = "room_topic"
		if content, ok := evt.Content.Parsed.(*event.TopicEventContent); ok {
			m.Body = content.Topic
		}
	default:
		if evt.Type.Class == event.StateEventType || evt.StateKey != nil {
			return nil
		}
		if evt.Type.Type == "m.call.invite" || strings.HasPrefix(evt.Type.Type, "org.matrix.msc3401.call") {
			m.Kind = "call"
			return m
		}
		if strings.Contains(evt.Type.Type, "poll.start") {
			m.Kind = "poll"
			return m
		}
		return nil
	}
	return m
}

// aggregate folds edits, reactions and redactions into the messages they target. Input is oldest-first.
func aggregate(msgs []*Message, includeState bool) []*Message {
	byID := map[string]*Message{}
	for _, m := range msgs {
		byID[m.EventID] = m
	}
	out := make([]*Message, 0, len(msgs))
	for _, m := range msgs {
		switch m.Kind {
		case "edit":
			if target, ok := byID[m.Edits]; ok {
				target.Body = m.Body
				target.Edited = true
				continue
			}
		case "reaction":
			if target, ok := byID[m.ReactTo]; ok {
				if target.Reactions == nil {
					target.Reactions = map[string][]string{}
				}
				name := m.SenderName
				if m.IsMe {
					name = "me"
				}
				target.Reactions[m.Body] = append(target.Reactions[m.Body], name)
				continue
			}
		case "redaction":
			if target, ok := byID[m.Redacts]; ok {
				target.Kind = "deleted"
				target.Body = ""
				target.Media = nil
			}
			continue
		case "membership", "room_name", "room_topic":
			if !includeState {
				continue
			}
		}
		out = append(out, m)
	}
	return out
}
