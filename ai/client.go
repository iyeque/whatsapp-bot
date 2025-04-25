package ai

import (
	"context"
	"errors"
	"time"

	"whatsapp-gpt-bot/utils"

	"github.com/rs/zerolog"
	"github.com/sashabaranov/go-openai"
)

// Client implements IClient
type Client struct {
	client  *openai.Client
	logger  zerolog.Logger
	retries int
	timeout time.Duration
}

func NewClient(opts ...ClientOption) (*Client, error) {
	c := &Client{
		client:  openai.NewClient(""),
		logger:  zerolog.Nop(),
		retries: 3,
		timeout: time.Minute,
	}
	return c, nil
}

func (c *Client) Chat(ctx context.Context, prompt string) (string, error) {
	var resp openai.ChatCompletionResponse
	config := &utils.RetryConfig{
		InitialInterval: 100 * time.Millisecond,
		MaxInterval:     2 * time.Second,
		MaxElapsedTime:  c.timeout,
	}

	err := utils.WithRetry(func() error {
		req := openai.ChatCompletionRequest{
			Model: openai.GPT3Dot5Turbo,
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleUser,
					Content: prompt,
				},
			},
		}

		result, err := c.client.CreateChatCompletion(ctx, req)
		if err != nil {
			return err
		}
		resp = result
		return nil
	}, config)

	if err != nil {
		return "", err
	}

	if len(resp.Choices) == 0 {
		return "", errors.New("no response from AI")
	}

	return resp.Choices[0].Message.Content, nil
}
