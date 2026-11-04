package adapter

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestPersonalWhatsAppSelfChatIsOnlyCommandSource(t *testing.T) {
	own := types.NewJID("19995550123", types.DefaultUserServer)
	lid := types.NewJID("1234567", types.HiddenUserServer)
	base := func() *events.Message {
		return &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: own, Sender: own, IsFromMe: true}, ID: "3EB0FIXTURE", Timestamp: time.Now().UTC()}, Message: &waE2E.Message{Conversation: proto.String("!relay inspect fixture only")}}
	}
	for _, test := range []struct {
		name   string
		change func(*events.Message)
		want   bool
	}{
		{"self", func(*events.Message) {}, true},
		{"self LID", func(e *events.Message) { e.Info.Chat = lid }, true},
		{"other user", func(e *events.Message) { e.Info.Chat = types.NewJID("19995550999", types.DefaultUserServer) }, false},
		{"incoming contact", func(e *events.Message) { e.Info.IsFromMe = false }, false},
		{"group", func(e *events.Message) { e.Info.IsGroup = true }, false},
		{"history", func(e *events.Message) { e.SourceWebMsg = &waWeb.WebMessageInfo{} }, false},
		{"edited", func(e *events.Message) { e.IsEdit = true }, false},
		{"bot", func(e *events.Message) { e.IsBotInvoke = true }, false},
		{"view once", func(e *events.Message) { e.IsViewOnce = true }, false},
		{"reply loop", func(e *events.Message) { e.Message.Conversation = proto.String("Relay\n!relay inspect fixture") }, false},
		{"no prefix", func(e *events.Message) { e.Message.Conversation = proto.String("inspect fixture") }, false},
		{"caption", func(e *events.Message) {
			e.Message = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("!relay inspect")}}
		}, false},
		{"extended text", func(e *events.Message) {
			e.Message = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("!relay status")}}
		}, true},
		{"bad message id", func(e *events.Message) { e.Info.ID = "../secret" }, false},
		{"oversized", func(e *events.Message) { e.Message.Conversation = proto.String("!relay " + strings.Repeat("x", 8010)) }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := base()
			test.change(e)
			_, ok := selfCommand(e, own, lid)
			if ok != test.want {
				t.Fatalf("command admission=%v", ok)
			}
		})
	}
	if _, ok := selfCommand(base(), types.EmptyJID, types.EmptyJID); ok {
		t.Fatal("unlinked account accepted a command")
	}
}
func TestPersonalWhatsAppPrivateSQLiteReopensWithoutNetwork(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private Ł device")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Open(ctx, dir, func(Message) { t.Error("no account should emit messages") })
	if err != nil {
		t.Fatal(err)
	}
	if state := c.State(); state.Linked || state.Connected {
		t.Fatal("new store reported a linked account", state)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "device.sqlite"))
	if err != nil || info.Size() == 0 {
		t.Fatal("SQLite migrations missing", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("credential database permissions", info.Mode())
	}
	c, err = Open(ctx, dir, nil)
	if err != nil {
		t.Fatal("private SQLite reopen", err)
	}
	if c.State().Linked {
		t.Fatal("reopen invented an account")
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if err = Forget(dir); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("owned credential files not removed", err)
	}
}
func TestPersonalWhatsAppDatabaseRejectsLinkedFilesAndPreservesUnknown(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := PrepareDirectory(dir); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(dir, "user-owned.txt")
	if err := os.WriteFile(unknown, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Forget(dir); err == nil {
		t.Fatal("unknown content silently removed")
	}
	if data, _ := os.ReadFile(unknown); string(data) != "preserve" {
		t.Fatal("unknown content changed")
	}
	t.Run("linked database", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte("preserve"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, "device.sqlite")); err != nil {
			t.Skipf("test user lacks symlink permission: %v", err)
		}
		if _, err := Open(context.Background(), dir, nil); err == nil {
			t.Fatal("linked credentials opened")
		}
		if data, _ := os.ReadFile(outside); string(data) != "preserve" {
			t.Fatal("linked target changed")
		}
	})
}
