package db

import (
	"encoding/json"
	"testing"
	"time"

	"doriyamedia/internal/models"
)

func TestCleanupPlaybackKeepsActiveMediaAndDropsOldTerminalRows(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "Cleanup", "cleanup", now)

	activeTicket := createReadyPlaybackTicket(
		t, store, fixture, now, []string{models.PlaybackTransportServerProxy},
		`{"container":"mp4","duration":120,"audio":[]}`)
	activeTicket = claimPlaybackTransport(
		t, store, fixture, activeTicket, models.PlaybackTransportServerProxy, now)
	activeGrant := createPlaybackMediaSession(
		t, store, fixture, activeTicket, models.PlaybackTransportServerProxy, now)

	terminalTicket := createReadyPlaybackTicket(
		t, store, fixture, now, []string{models.PlaybackTransportServerProxy},
		`{"container":"mp4","duration":120,"audio":[]}`)
	terminalTicket = claimPlaybackTransport(
		t, store, fixture, terminalTicket, models.PlaybackTransportServerProxy, now)
	terminalGrant := createPlaybackMediaSession(
		t, store, fixture, terminalTicket, models.PlaybackTransportServerProxy, now)

	old := now.Add(-25 * time.Hour)
	if _, err := store.DB.Exec(`UPDATE playback_tickets
		SET updated_at = ? WHERE ticket_id IN (?, ?)`,
		old, activeTicket.TicketID, terminalTicket.TicketID); err != nil {
		t.Fatalf("age tickets: %v", err)
	}
	if _, err := store.DB.Exec(`UPDATE playback_tickets
		SET state = ? WHERE ticket_id = ?`,
		models.PlaybackTicketExpired, terminalTicket.TicketID); err != nil {
		t.Fatalf("expire terminal ticket: %v", err)
	}
	if _, err := store.DB.Exec(`UPDATE playback_media_sessions
		SET state = ?, revoked_at = ?, updated_at = ?
		WHERE ticket_id = ?`,
		models.PlaybackMediaSessionRevoked, old, old, terminalTicket.TicketID); err != nil {
		t.Fatalf("age terminal session: %v", err)
	}
	future := now.Add(6 * time.Hour)
	if _, err := store.DB.Exec(`UPDATE playback_media_sessions
		SET idle_expires_at = ?, absolute_expires_at = ?, updated_at = ?
		WHERE ticket_id = ?`,
		future, future, old, activeTicket.TicketID); err != nil {
		t.Fatalf("keep active session alive: %v", err)
	}

	stats, err := store.CleanupPlayback(now)
	if err != nil {
		t.Fatalf("CleanupPlayback: %v", err)
	}
	if stats.DeletedMediaSessions != 1 {
		t.Errorf("deleted media sessions = %d, want 1", stats.DeletedMediaSessions)
	}
	if stats.DeletedTickets != 1 {
		t.Errorf("deleted tickets = %d, want 1", stats.DeletedTickets)
	}

	var count int
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM playback_tickets
		WHERE ticket_id = ?`, activeTicket.TicketID).Scan(&count); err != nil {
		t.Fatalf("count active ticket: %v", err)
	}
	if count != 1 {
		t.Fatalf("ticket with an active media session was deleted")
	}
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM playback_tickets
		WHERE ticket_id = ?`, terminalTicket.TicketID).Scan(&count); err != nil {
		t.Fatalf("count terminal ticket: %v", err)
	}
	if count != 0 {
		t.Fatalf("old terminal ticket was retained")
	}

	var capabilityHash string
	if err := store.DB.QueryRow(`SELECT capability_hash FROM playback_media_sessions
		WHERE ticket_id = ?`, activeTicket.TicketID).Scan(&capabilityHash); err != nil {
		t.Fatalf("active session disappeared: %v", err)
	}
	if capabilityHash != hashPlaybackCapability(activeGrant.Capability) {
		t.Fatalf("active session capability hash changed")
	}
	if terminalGrant == nil {
		t.Fatal("terminal grant was not created")
	}
}

func TestCleanupPlaybackExpiresOverdueRows(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "Expiry", "expiry", now)
	ticket := createReadyPlaybackTicket(
		t, store, fixture, now, []string{models.PlaybackTransportServerHLS},
		`{"container":"mp4","duration":120,"audio":[]}`)
	ticket = claimPlaybackTransport(
		t, store, fixture, ticket, models.PlaybackTransportServerHLS, now)
	grant := createPlaybackMediaSession(
		t, store, fixture, ticket, models.PlaybackTransportServerHLS, now)

	stats, err := store.CleanupPlayback(now.Add(models.PlaybackMediaSessionAbsoluteTTL + time.Minute))
	if err != nil {
		t.Fatalf("CleanupPlayback: %v", err)
	}
	if stats.ExpiredTickets != 1 {
		t.Errorf("expired tickets = %d, want 1", stats.ExpiredTickets)
	}
	if stats.ExpiredMediaSessions != 1 {
		t.Errorf("expired media sessions = %d, want 1", stats.ExpiredMediaSessions)
	}

	var ticketState, sessionState string
	if err := store.DB.QueryRow(`SELECT state FROM playback_tickets WHERE ticket_id = ?`,
		ticket.TicketID).Scan(&ticketState); err != nil {
		t.Fatalf("read ticket state: %v", err)
	}
	if err := store.DB.QueryRow(`SELECT state FROM playback_media_sessions WHERE ticket_id = ?`,
		ticket.TicketID).Scan(&sessionState); err != nil {
		t.Fatalf("read session state: %v", err)
	}
	if ticketState != models.PlaybackTicketExpired ||
		sessionState != models.PlaybackMediaSessionExpired {
		t.Fatalf("states = %s/%s, want expired/expired", ticketState, sessionState)
	}
	if grant == nil {
		t.Fatal("media session was not created")
	}
}

func TestCleanupPlaybackHandlesSubtitleSessionsAndProtectsActiveTicket(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	fixture := addPlaybackTestFixture(t, store, "Subtitle cleanup", "subtitle-cleanup", now)
	subtitleID := addPlaybackSubtitle(
		t, store, fixture.fileID, "/subtitle-cleanup/caption.srt", "subrip", false, 0)

	createSubtitleTicket := func() (*models.PlaybackTicket, *models.PlaybackSubtitleSessionGrant) {
		t.Helper()
		ticket := createConsumedPlaybackTicket(
			t, store, fixture, now, []int64{subtitleID})
		grant, err := store.CreatePlaybackSubtitleSession(
			models.PlaybackSubtitleSessionRequest{
				Ticket:       playbackSubtitleTicketBinding(ticket),
				Transport:    models.PlaybackTransportDirectHTTP,
				SubtitleID:   subtitleID,
				StartSeconds: 0,
			},
			now.Add(3*time.Second),
		)
		if err != nil {
			t.Fatalf("create cleanup subtitle session: %v", err)
		}
		return ticket, grant
	}

	activeTicket, activeGrant := createSubtitleTicket()
	terminalTicket, terminalGrant := createSubtitleTicket()
	overdueTicket, overdueGrant := createSubtitleTicket()
	old := now.Add(-25 * time.Hour)
	if _, err := store.DB.Exec(`UPDATE playback_tickets
		SET state = ?, updated_at = ?
		WHERE ticket_id IN (?, ?, ?)`,
		models.PlaybackTicketExpired, old,
		activeTicket.TicketID, terminalTicket.TicketID, overdueTicket.TicketID,
	); err != nil {
		t.Fatalf("age cleanup tickets: %v", err)
	}
	if _, err := store.DB.Exec(`UPDATE playback_subtitle_sessions
		SET state = ?, revoked_at = ?, updated_at = ?
		WHERE id = ?`,
		models.PlaybackSubtitleSessionRevoked, old, old, terminalGrant.Session.ID,
	); err != nil {
		t.Fatalf("revoke terminal subtitle session: %v", err)
	}
	if _, err := store.DB.Exec(`UPDATE playback_subtitle_sessions
		SET idle_expires_at = ?, absolute_expires_at = absolute_expires_at,
			updated_at = ?
		WHERE id = ?`,
		now.Add(-time.Minute), old, overdueGrant.Session.ID,
	); err != nil {
		t.Fatalf("expire overdue subtitle session: %v", err)
	}

	stats, err := store.CleanupPlayback(now.Add(2 * time.Minute))
	if err != nil {
		t.Fatalf("CleanupPlayback: %v", err)
	}
	if stats.ExpiredSubtitleSessions != 1 {
		t.Errorf("expired subtitle sessions = %d, want 1", stats.ExpiredSubtitleSessions)
	}
	if stats.DeletedSubtitleSessions != 1 {
		t.Errorf("deleted subtitle sessions = %d, want 1", stats.DeletedSubtitleSessions)
	}
	if stats.DeletedTickets != 2 {
		t.Errorf("deleted tickets = %d, want 2", stats.DeletedTickets)
	}

	var count int
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM playback_tickets
		WHERE ticket_id = ?`, activeTicket.TicketID).Scan(&count); err != nil {
		t.Fatalf("count active subtitle ticket: %v", err)
	}
	if count != 1 {
		t.Fatal("ticket with an active subtitle session was deleted")
	}
	var state string
	if err := store.DB.QueryRow(`SELECT state FROM playback_subtitle_sessions
		WHERE id = ?`, activeGrant.Session.ID).Scan(&state); err != nil {
		t.Fatalf("read active subtitle session: %v", err)
	}
	if state != models.PlaybackSubtitleSessionActive {
		t.Fatalf("active subtitle session state = %q", state)
	}
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM playback_subtitle_sessions
		WHERE id = ?`, terminalGrant.Session.ID).Scan(&count); err != nil {
		t.Fatalf("count deleted subtitle session: %v", err)
	}
	if count != 0 {
		t.Fatal("old terminal subtitle session was retained")
	}
	for _, ticket := range []*models.PlaybackTicket{terminalTicket, overdueTicket} {
		if err := store.DB.QueryRow(`SELECT COUNT(*) FROM playback_tickets
			WHERE ticket_id = ?`, ticket.TicketID).Scan(&count); err != nil {
			t.Fatalf("count deleted subtitle ticket: %v", err)
		}
		if count != 0 {
			t.Fatalf("old terminal subtitle ticket %s was retained", ticket.TicketID)
		}
	}
}

func TestCleanupPlaybackStatsAreJSONSafe(t *testing.T) {
	stats := PlaybackCleanupStats{ExpiredTickets: 1, DeletedTickets: 2}
	if _, err := json.Marshal(stats); err != nil {
		t.Fatalf("marshal cleanup stats: %v", err)
	}
}
