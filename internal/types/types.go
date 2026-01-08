package types

import "time"

type Event struct {
	Type    string         `json:"type"`
	Ts      time.Time      `json:"timestamp"`
	Payload map[string]any `json:"payload,omitempty"`
}

type Session struct {
	ID        string    `json:"session_id"`
	RoomName  string    `json:"room_name"`
	RoomURL   string    `json:"room_url"`
	BotToken  string    `json:"bot_token"`
	CreatedAt time.Time `json:"created_at"`
	Status    string    `json:"status"`

	BotPID          int        `json:"bot_pid,omitempty"`
	BotLastExitCode int        `json:"bot_last_exit_code,omitempty"`
	BotLastExitAt   *time.Time `json:"bot_last_exit_at,omitempty"`
}
