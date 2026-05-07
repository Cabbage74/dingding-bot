package dingtalk

// Event represents a parsed DingTalk bot message event.
type Event struct {
	MsgType          string      `json:"MsgType"`
	Text             TextContent `json:"Text,omitempty"`
	ChatID           string      `json:"ChatId,omitempty"`
	SenderID         string      `json:"SenderId,omitempty"`
	SenderNick       string      `json:"SenderNick,omitempty"`
	ConversationID   string      `json:"ConversationId,omitempty"`
	ConversationType string      `json:"ConversationType,omitempty"`
	SessionWebhook   string      `json:"SessionWebhook,omitempty"`
	RobotCode        string      `json:"RobotCode,omitempty"`
	IsAdmin          bool        `json:"IsAdmin,omitempty"`
}

type TextContent struct {
	Content string `json:"content"`
}

// MessageHandler processes parsed DingTalk events.
type MessageHandler func(event *Event) error
