package ai

import "context"

// IClient defines the interface for AI client operations
type IClient interface {
	Complete(prompt string) (string, error)
	GenerateImageDescription(ctx context.Context, imageData []byte) (string, error)
	Summarize(ctx context.Context, history []string) (string, error)
}
