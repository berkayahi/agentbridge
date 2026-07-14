// Package telegram defines Agent Bridge's Telegram boundary without exposing SDK types.
package telegram

import "time"

type ChatType string

const (
	ChatPrivate    ChatType = "private"
	ChatGroup      ChatType = "group"
	ChatSupergroup ChatType = "supergroup"
	ChatChannel    ChatType = "channel"
)

type User struct {
	ID       int64
	Username string
}

type Chat struct {
	ID   int64
	Type ChatType
}

type IncomingMessage struct {
	ID               int64
	Chat             Chat
	From             User
	Text             string
	Caption          string
	ReplyToMessageID int64
	MediaGroupID     string
}

type CallbackQuery struct {
	ID         string
	From       User
	Message    IncomingMessage
	Data       string
	ReceivedAt time.Time
}

type Update struct {
	ID       int64
	Message  *IncomingMessage
	Callback *CallbackQuery
}

func (u Update) identity() (User, Chat, bool) {
	if u.Message != nil {
		return u.Message.From, u.Message.Chat, true
	}
	if u.Callback != nil {
		return u.Callback.From, u.Callback.Message.Chat, true
	}
	return User{}, Chat{}, false
}
