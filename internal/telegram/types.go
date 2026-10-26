package telegram

// User is the subset of a Telegram user that the bot reads.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
}

// Chat is the subset of a Telegram chat that the bot reads.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// Message is the subset of a Telegram message that the bot reads.
type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from"`
	Chat      Chat   `json:"chat"`
	Date      int64  `json:"date"`
	Text      string `json:"text"`
}

// Update is one Telegram update. The client requests only message updates, so
// the other update kinds are never populated.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}
