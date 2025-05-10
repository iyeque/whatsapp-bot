
package whatsapp

import (
	"context"
	"fmt"
	"time"

	"github.com/cenkalti/backoff/v4"
)

type Client struct {
	handler   IEventHandler
	state     ConnectionState
	qrChan    chan string
	connected chan struct{}
	session   *SessionData
	storage   SessionStorage
	isTimeout bool
}

func (c *Client) Connect(ctx context.Context) error {
	// Reset timeout flag
	c.isTimeout = false

	// Try to restore session without timeout
	if c.storage != nil {
		session, err := c.storage.Load()
		if err == nil {
			c.session = session
			if err := c.handler.RestoreSession(session.Data.([]byte)); err == nil {
				c.state = StateConnected
				return nil
			}
			// Update session state and error
			session.State = StateDisconnected
			session.LastError = err
			session.LastActive = time.Now()
			_ = c.storage.Save(session)
		}
	}

	c.state = StateConnecting

	// Handle QR code if needed
	go func() {
		for {
			select {
			case qr := <-c.qrChan:
				if qr != "" {
					fmt.Printf("QR Code received, please scan: %s\n", qr)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Setup exponential backoff with no max elapsed time for infinite retries
	b := backoff.NewExponentialBackOff()
	b.MaxInterval = ReconnectDelay
	b.MaxElapsedTime = 0 // Run indefinitely

	// Update retry logic without timeout
	err := backoff.Retry(func() error {
		if err := c.handler.Handle(nil); err != nil {
			c.state = StateReconnecting
			return err
		}

		select {
		case <-c.connected:
			c.state = StateConnected
			return nil
		case <-ctx.Done():
			c.state = StateDisconnected
			return fmt.Errorf("context cancelled")
		}
	}, b)

	if err != nil {
		return fmt.Errorf("failed to connect after retries: %v", err)
	}

	return nil
}

func (c *Client) HandleQRCode(code string) {
	select {
	case c.qrChan <- code:
	default:
		// Channel full or closed, ignore
	}
}