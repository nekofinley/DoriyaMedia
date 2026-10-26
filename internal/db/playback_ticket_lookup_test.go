package db

import (
	"errors"
	"testing"
	"time"

	"doriyamedia/internal/models"
)

func TestPlaybackTicketByPublicIDEnforcesLiveBindingAndExpiry(t *testing.T) {
	store := openPlaybackTestStore(t)
	now := time.Date(2026, 10, 26, 16, 0, 0, 0, time.UTC)
	owner := addPlaybackTestFixture(t, store, "lookup-owner", "lookup-owner", now)
	other := addPlaybackTestFixture(t, store, "lookup-other", "lookup-other", now)

	ticket, err := store.CreatePlaybackTicket(models.PlaybackTicketRequest{
		UserID:              owner.user.ID,
		DeviceID:            owner.device.ID,
		FileID:              owner.fileID,
		CandidateTransports: []string{models.PlaybackTransportServerHLS},
	}, now)
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}

	got, err := store.PlaybackTicketByPublicID(
		owner.user.ID, owner.device.ID, ticket.TicketID, now.Add(time.Second))
	if err != nil {
		t.Fatalf("lookup ticket: %v", err)
	}
	if got.FileID != owner.fileID || got.TicketID != ticket.TicketID {
		t.Fatalf("lookup ticket = %+v", got)
	}
	for name, lookup := range map[string]struct {
		userID   int64
		deviceID int64
	}{
		"other user":   {other.user.ID, owner.device.ID},
		"other device": {owner.user.ID, other.device.ID},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := store.PlaybackTicketByPublicID(
				lookup.userID, lookup.deviceID, ticket.TicketID, now)
			if !errors.Is(err, ErrPlaybackTicketNotFound) {
				t.Fatalf("lookup error = %v, want not found", err)
			}
		})
	}

	expired, err := store.PlaybackTicketByPublicID(
		owner.user.ID, owner.device.ID, ticket.TicketID,
		now.Add(models.PlaybackTicketTTL))
	if err != nil {
		t.Fatalf("lookup expired ticket: %v", err)
	}
	if expired.State != models.PlaybackTicketExpired ||
		expired.Version != ticket.Version+1 {
		t.Fatalf("expired lookup = %+v", expired)
	}
}
