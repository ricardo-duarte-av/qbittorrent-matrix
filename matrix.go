package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// CommandFunc is the signature for bot command handlers.
// It returns a plain-text fallback and an HTML body (may be empty, falling
// back to plain text), plus any error.
type CommandFunc func(ctx context.Context, args string) (plain, html string, err error)

type MatrixBot struct {
	client   *mautrix.Client
	roomID   id.RoomID
	commands map[string]CommandFunc
}

// RegisterCommand registers a handler for "!<name>" messages.
// Must be called before Start().
func (b *MatrixBot) RegisterCommand(name string, fn CommandFunc) {
	b.commands[strings.ToLower(name)] = fn
}

func NewMatrixBot(cfg MatrixConfig) (*MatrixBot, error) {
	client, err := mautrix.NewClient(cfg.Homeserver, id.UserID(cfg.UserID), cfg.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("creating matrix client: %w", err)
	}

	if cfg.AccessToken == "" {
		resp, err := client.Login(context.Background(), &mautrix.ReqLogin{
			Type: mautrix.AuthTypePassword,
			Identifier: mautrix.UserIdentifier{
				Type: mautrix.IdentifierTypeUser,
				User: cfg.Username,
			},
			Password:                 cfg.Password,
			InitialDeviceDisplayName: "qbittorrent-matrix",
			StoreCredentials:         true,
		})
		if err != nil {
			return nil, fmt.Errorf("matrix login: %w", err)
		}
		log.Printf("Logged in as %s", resp.UserID)
	}

	return &MatrixBot{
		client:   client,
		roomID:   id.RoomID(cfg.RoomID),
		commands: make(map[string]CommandFunc),
	}, nil
}

// SendNotice sends a plain notice to the configured room.
func (b *MatrixBot) SendNotice(ctx context.Context, text string) error {
	_, err := b.client.SendNotice(ctx, b.roomID, text)
	return err
}

// sendHTML sends a notice with both a plain-text fallback and an HTML body.
func (b *MatrixBot) sendHTML(ctx context.Context, plain, htmlBody string) error {
	_, err := b.client.SendMessageEvent(ctx, b.roomID, event.EventMessage, &event.MessageEventContent{
		MsgType:       event.MsgNotice,
		Body:          plain,
		Format:        event.FormatHTML,
		FormattedBody: htmlBody,
	})
	return err
}

// Start joins the configured room, registers the message event handler, and
// runs the Matrix sync loop. Blocking; cancel ctx to stop.
func (b *MatrixBot) Start(ctx context.Context) {
	if _, err := b.client.JoinRoomByID(ctx, b.roomID); err != nil {
		log.Printf("join room %s: %v", b.roomID, err)
	}

	syncer := b.client.Syncer.(*mautrix.DefaultSyncer)
	syncer.OnEventType(event.EventMessage, b.handleMessage)

	if err := b.client.SyncWithContext(ctx); err != nil && ctx.Err() == nil {
		log.Printf("sync error: %v", err)
	}
}

func (b *MatrixBot) handleMessage(ctx context.Context, evt *event.Event) {
	// Only process messages from the configured room.
	if evt.RoomID != b.roomID {
		return
	}
	// Ignore own messages.
	if evt.Sender == b.client.UserID {
		return
	}
	// Ignore messages that arrived before the bot started (stale on first sync).
	if time.Since(time.UnixMilli(evt.Timestamp)) > 30*time.Second {
		return
	}

	content := evt.Content.AsMessage()
	if content.MsgType != event.MsgText {
		return
	}

	body := strings.TrimSpace(content.Body)
	if !strings.HasPrefix(body, "!") {
		return
	}

	parts := strings.SplitN(body[1:], " ", 2)
	cmd := strings.ToLower(parts[0])
	args := ""
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}

	fn, ok := b.commands[cmd]
	if !ok {
		return
	}

	log.Printf("Command !%s from %s", cmd, evt.Sender)

	plain, htmlBody, err := fn(ctx, args)
	if err != nil {
		log.Printf("command !%s error: %v", cmd, err)
		_ = b.SendNotice(ctx, fmt.Sprintf("Error running !%s: %v", cmd, err))
		return
	}
	if err := b.sendHTML(ctx, plain, htmlBody); err != nil {
		log.Printf("send command response for !%s: %v", cmd, err)
	}
}
