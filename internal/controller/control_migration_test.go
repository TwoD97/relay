package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/TwoD97/relay/internal/personalwhatsapp"
	"github.com/TwoD97/relay/internal/whatsappbridge"
)

func TestControlChannelsStopAtMigrationFence(t *testing.T) {
	s, client := personalFixture(t)
	s.migration = openMigrationStore(s.config.StateDir)
	s.migration.record.SourceFrozen = true
	s.whatsappBusiness = openWhatsAppBusinessStore(s.config.StateDir)
	s.whatsappBusiness.record = businessRecordFixture()
	s.discord.record = discordRecord{Format: 1, Epoch: "epoch", Token: "token", Owner: "300000", Channel: "200000", Paired: true}
	// These would reach the network without the migration admission fence.
	s.pollDiscord()
	s.pollWhatsAppBusiness()
	if err := s.handleDiscord(s.discord.snapshot(), discordFixtureMessage("100001", "continue work")); err == nil {
		t.Fatal("moved Discord handler admitted work")
	}
	if err := s.handleTelegram(s.telegram.snapshot(), telegramUpdate{}); err == nil {
		t.Fatal("moved Telegram handler admitted work")
	}
	if err := s.handleWhatsAppBusiness(s.whatsappBusiness.snapshot(), whatsappbridge.Event{ID: "fixture", Text: "continue work"}); err == nil {
		t.Fatal("moved Business handler admitted work")
	}
	if err := s.handlePersonalWhatsApp(s.personalWhatsApp.snapshot().Epoch, personalwhatsapp.Message{ID: "3EB01234567890", At: time.Now(), Text: "continue work"}); err == nil {
		t.Fatal("moved personal handler admitted work")
	}
	consumed := false
	ticket := controlApprovalTicket{ID: strings.Repeat("a", 32), CanAllow: true, ExpiresAt: time.Now().Add(time.Minute)}
	result := s.applyControlDecision(ticket, "allow", func() error { consumed = true; return nil })
	if consumed || !strings.Contains(result, "no decision was sent") {
		t.Fatal("moved approval was consumed or dispatched", result)
	}
	s.connectPersonalWhatsApp()
	if client.closes != 1 || client.logouts != 0 || client.connects != 0 || s.personalWhatsApp.current(s.personalWhatsApp.snapshot().Epoch) != nil {
		t.Fatal("moved personal client did not stop without logging out")
	}
	state, _, _ := s.control.snapshot()
	if len(state.Messages) != 0 {
		t.Fatal("moved controller accepted an instruction")
	}
}
