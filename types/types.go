package types

import "time"

// Message represents a WhatsApp message
type Message struct {
	ID        string
	Type      MessageType
	Content   interface{}
	Timestamp time.Time
	ChatID    string
}

// MessageType defines the type of a message
type MessageType string

const (
	// TextMessage is a text message
	TextMessage MessageType = "text"
	// ImageMessage is an image message
	ImageMessage MessageType = "image"
	// DocumentMessage is a document message
	DocumentMessage MessageType = "document"
)
