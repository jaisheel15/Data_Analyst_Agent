package session

import (
	"time"

	"dataanalyst/api/internal/agent"
)

// View is the GET /api/sessions/{id} response.
type View struct {
	ID       string        `json:"id"`
	Datasets []agent.Table `json:"datasets"`
	Messages []MessageView `json:"messages"`
}

type MessageView struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}
