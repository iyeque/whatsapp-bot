package utils

import (
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
)

// CreateTextMessage creates a WhatsApp text message
func CreateTextMessage(text string) *waProto.Message {
	return &waProto.Message{
		Conversation: &text,
	}
}

// CreateImageMessage creates a WhatsApp image message
func CreateImageMessage(caption string, uploaded whatsmeow.UploadResponse, data []byte, mimeType string) *waProto.Message {
	return &waProto.Message{
		ImageMessage: &waProto.ImageMessage{
			Caption:       &caption,
			URL:           &uploaded.URL,
			MediaKey:      uploaded.MediaKey,
			Mimetype:      &mimeType,
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    &[]uint64{uint64(len(data))}[0],
			Height:        &[]uint32{100}[0],
			Width:         &[]uint32{100}[0],
			DirectPath:    new(string),
		},
	}
}
