package dingtalk

import (
	"context"
	"log/slog"

	"github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/client"
)

// StreamClient wraps the official DingTalk Stream SDK.
type StreamClient struct {
	appKey    string
	appSecret string
	handler   MessageHandler
	cli       *client.StreamClient
	cancel    context.CancelFunc
}

// NewStreamClient creates a new Stream mode client using the official SDK.
func NewStreamClient(appKey, appSecret string, handler MessageHandler) *StreamClient {
	return &StreamClient{
		appKey:    appKey,
		appSecret: appSecret,
		handler:   handler,
	}
}

// Connect starts the Stream connection. Blocks until Stop() is called.
// The SDK handles reconnection internally.
func (s *StreamClient) Connect() error {
	s.cli = client.NewStreamClient(
		client.WithAppCredential(client.NewAppCredentialConfig(s.appKey, s.appSecret)),
	)
	s.cli.RegisterChatBotCallbackRouter(s.onBotMessage)

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	defer s.cli.Close()

	slog.Info("starting dingtalk stream client...")
	if err := s.cli.Start(ctx); err != nil {
		return err
	}

	slog.Info("stream connected, waiting...")
	// Block until stopped
	<-ctx.Done()
	slog.Info("stream client stopped")
	return nil
}

// Stop closes the stream connection.
func (s *StreamClient) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
}

// onBotMessage converts SDK bot callback to our Event and dispatches.
func (s *StreamClient) onBotMessage(ctx context.Context, data *chatbot.BotCallbackDataModel) ([]byte, error) {
	event := &Event{
		MsgType:          data.Msgtype,
		SenderID:         data.SenderId,
		SenderNick:       data.SenderNick,
		ChatID:           data.ConversationId,
		ConversationID:   data.ConversationId,
		ConversationType: data.ConversationType,
		SessionWebhook:   data.SessionWebhook,
		IsAdmin:          data.IsAdmin,
	}
	event.Text.Content = data.Text.Content

	slog.Info("received bot message",
		"msg_type", event.MsgType,
		"chat_id", event.ChatID,
		"sender_id", event.SenderID,
		"content", event.Text.Content,
	)

	// Process asynchronously
	go func() {
		if err := s.handler(event); err != nil {
			slog.Error("handle event", "error", err)
		}
	}()

	return []byte(""), nil
}
