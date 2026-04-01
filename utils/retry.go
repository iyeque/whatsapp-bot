package utils

import (
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"
)

type RetryConfig struct {
	MaxAttempts     int
	InitialInterval time.Duration
	MaxInterval     time.Duration
	MaxElapsedTime  time.Duration
}

func CreateTextMessage(text string) *waProto.Message {
	return &waProto.Message{
		Conversation: proto.String(text),
	}
}
