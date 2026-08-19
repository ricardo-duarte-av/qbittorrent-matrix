package main

import (
	"context"
	"fmt"
	"html"
	"log"
	"sort"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// CommandFunc is the signature for bot command handlers.
// It returns slices of plain-text fallbacks and HTML bodies (one entry per
// message to send), plus any error. Returning a single-element slice is the
// common case; multiple elements cause multiple messages to be sent in order.
type CommandFunc func(ctx context.Context, args string) (plains, htmls []string, err error)

type command struct {
	fn CommandFunc
	// description is shown by !help. An empty description hides the command
	// from the listing, which is how aliases are kept out of it.
	description string
}

type MatrixBot struct {
	client   *mautrix.Client
	roomID   id.RoomID
	commands map[string]command
}

// RegisterCommand registers a handler for "!<name>" messages.
// A command registered with an empty description is hidden from !help; use
// that for aliases of an already-listed command.
// Must be called before Start().
func (b *MatrixBot) RegisterCommand(name, description string, fn CommandFunc) {
	b.commands[strings.ToLower(name)] = command{fn: fn, description: description}
}

// cmdHelp lists every registered command that has a description.
func (b *MatrixBot) cmdHelp(_ context.Context, _ string) (plains, htmls []string, err error) {
	names := make([]string, 0, len(b.commands))
	for name, cmd := range b.commands {
		if cmd.description != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var plain strings.Builder
	var htmlBody strings.Builder
	plain.WriteString("Available commands:\n")
	htmlBody.WriteString("<p><b>Available commands</b></p><ul>")
	for _, name := range names {
		desc := b.commands[name].description
		fmt.Fprintf(&plain, "!%-14s %s\n", name, desc)
		fmt.Fprintf(&htmlBody, "<li><code>!%s</code> — %s</li>",
			html.EscapeString(name), html.EscapeString(desc))
	}
	htmlBody.WriteString("</ul>")

	return []string{plain.String()}, []string{htmlBody.String()}, nil
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

	bot := &MatrixBot{
		client:   client,
		roomID:   id.RoomID(cfg.RoomID),
		commands: make(map[string]command),
	}
	bot.RegisterCommand("help", "Show this list of commands", bot.cmdHelp)
	return bot, nil
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

	c, ok := b.commands[cmd]
	if !ok {
		return
	}

	log.Printf("Command !%s from %s", cmd, evt.Sender)

	plains, htmls, err := c.fn(ctx, args)
	if err != nil {
		log.Printf("command !%s error: %v", cmd, err)
		_ = b.SendNotice(ctx, fmt.Sprintf("Error running !%s: %v", cmd, err))
		return
	}
	for i := range plains {
		h := ""
		if i < len(htmls) {
			h = htmls[i]
		}
		if err := b.sendHTML(ctx, plains[i], h); err != nil {
			log.Printf("send command response for !%s (part %d/%d): %v", cmd, i+1, len(plains), err)
		}
	}
}
