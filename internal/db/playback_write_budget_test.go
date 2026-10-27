package db

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"doriyamedia/internal/models"
)

func TestPlaybackRequestWritesHonorDeadlineWhileWriterLocked(t *testing.T) {
	const writeBudget = 200 * time.Millisecond
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

	assertDeadline := func(t *testing.T, started time.Time, err error) {
		t.Helper()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("locked write error = %v, want context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("locked write took %v, want <= 2s", elapsed)
		}
	}

	t.Run("create ticket", func(t *testing.T) {
		store := openPlaybackTestStore(t)
		fixture := addPlaybackTestFixture(t, store, "budget-create", "budget-create", now)
		seed := createReadyPlaybackTicket(t, store, fixture, now,
			[]string{models.PlaybackTransportServerProxy}, `{}`)
		before := playbackTableCount(t, store, "playback_tickets")
		writer := lockPlaybackTicketWriter(t, store, fixture.bindingWith(seed.TicketID))

		ctx, cancel := context.WithTimeout(context.Background(), writeBudget)
		defer cancel()
		started := time.Now()
		created, err := store.CreatePlaybackTicketContext(ctx, models.PlaybackTicketRequest{
			UserID: fixture.user.ID, DeviceID: fixture.device.ID, FileID: fixture.fileID,
			CandidateTransports: []string{models.PlaybackTransportServerProxy},
			MediaSummary:        json.RawMessage(`{}`),
		}, now.Add(time.Second))
		elapsedErr := err
		if err := writer.Rollback(); err != nil {
			t.Fatalf("release playback ticket writer: %v", err)
		}
		assertDeadline(t, started, elapsedErr)
		if created != nil {
			t.Fatalf("locked ticket create returned ticket: %+v", created)
		}
		if after := playbackTableCount(t, store, "playback_tickets"); after != before {
			t.Fatalf("locked ticket create changed row count from %d to %d", before, after)
		}
	})

	t.Run("claim transport", func(t *testing.T) {
		store := openPlaybackTestStore(t)
		fixture := addPlaybackTestFixture(t, store, "budget-claim", "budget-claim", now)
		ready := createReadyPlaybackTicket(t, store, fixture, now,
			[]string{models.PlaybackTransportServerProxy}, `{}`)
		binding := fixture.bindingWith(ready.TicketID)
		before := readPlaybackTicketContextSnapshot(t, store, binding)
		writer := lockPlaybackTicketWriter(t, store, binding)

		ctx, cancel := context.WithTimeout(context.Background(), writeBudget)
		defer cancel()
		started := time.Now()
		claimed, err := store.ClaimNextPlaybackTransport(
			ctx, binding, ready.Version, now.Add(time.Second))
		elapsedErr := err
		if err := writer.Rollback(); err != nil {
			t.Fatalf("release playback ticket writer: %v", err)
		}
		assertDeadline(t, started, elapsedErr)
		if claimed != nil {
			t.Fatalf("locked claim returned ticket: %+v", claimed)
		}
		if after := readPlaybackTicketContextSnapshot(t, store, binding); after != before {
			t.Fatalf("locked claim changed ticket\nbefore: %+v\nafter:  %+v", before, after)
		}
	})

	t.Run("validate issuance", func(t *testing.T) {
		store := openPlaybackTestStore(t)
		fixture := addPlaybackTestFixture(t, store, "budget-validate", "budget-validate", now)
		ready := createReadyPlaybackTicket(t, store, fixture, now,
			[]string{models.PlaybackTransportServerProxy}, `{}`)
		claimed := claimPlaybackTransport(
			t, store, fixture, ready, models.PlaybackTransportServerProxy, now.Add(time.Second))
		binding := fixture.bindingWith(claimed.TicketID)
		before := readPlaybackTicketContextSnapshot(t, store, binding)
		writer := lockPlaybackTicketWriter(t, store, binding)

		ctx, cancel := context.WithTimeout(context.Background(), writeBudget)
		defer cancel()
		started := time.Now()
		err := store.ValidatePlaybackTransportIssuance(
			ctx, binding, claimed.Version, claimed.SelectedTransport,
			claimed.ClaimToken, now.Add(2*time.Second))
		elapsedErr := err
		if err := writer.Rollback(); err != nil {
			t.Fatalf("release playback ticket writer: %v", err)
		}
		assertDeadline(t, started, elapsedErr)
		if after := readPlaybackTicketContextSnapshot(t, store, binding); after != before {
			t.Fatalf("locked issuance validation changed ticket\nbefore: %+v\nafter:  %+v",
				before, after)
		}
	})

	t.Run("revoke ticket", func(t *testing.T) {
		store := openPlaybackTestStore(t)
		fixture := addPlaybackTestFixture(t, store, "budget-revoke", "budget-revoke", now)
		ready := createReadyPlaybackTicket(t, store, fixture, now,
			[]string{models.PlaybackTransportServerProxy}, `{}`)
		binding := fixture.bindingWith(ready.TicketID)
		before := readPlaybackTicketContextSnapshot(t, store, binding)
		writer := lockPlaybackTicketWriter(t, store, binding)

		ctx, cancel := context.WithTimeout(context.Background(), writeBudget)
		defer cancel()
		started := time.Now()
		revoked, err := store.RevokePlaybackTicketContext(
			ctx, binding, ready.Version, now.Add(time.Second))
		elapsedErr := err
		if err := writer.Rollback(); err != nil {
			t.Fatalf("release playback ticket writer: %v", err)
		}
		assertDeadline(t, started, elapsedErr)
		if revoked != nil {
			t.Fatalf("locked ticket revoke returned ticket: %+v", revoked)
		}
		if after := readPlaybackTicketContextSnapshot(t, store, binding); after != before {
			t.Fatalf("locked ticket revoke changed ticket\nbefore: %+v\nafter:  %+v",
				before, after)
		}
	})

	t.Run("create media session", func(t *testing.T) {
		store := openPlaybackTestStore(t)
		fixture := addPlaybackTestFixture(t, store, "budget-media", "budget-media", now)
		ready := createReadyPlaybackTicket(t, store, fixture, now,
			[]string{models.PlaybackTransportServerProxy}, `{}`)
		claimed := claimPlaybackTransport(
			t, store, fixture, ready, models.PlaybackTransportServerProxy, now.Add(time.Second))
		binding := fixture.bindingWith(claimed.TicketID)
		before := readPlaybackTicketMutationSnapshot(t, store, binding)
		writer := lockPlaybackTicketWriter(t, store, binding)

		ctx, cancel := context.WithTimeout(context.Background(), writeBudget)
		defer cancel()
		started := time.Now()
		grant, err := store.CreatePlaybackMediaSession(ctx, models.PlaybackMediaSessionRequest{
			Ticket: binding, Transport: models.PlaybackTransportServerProxy,
			Mode: "remux", AudioOrdinal: 0,
		}, now.Add(2*time.Second))
		elapsedErr := err
		if err := writer.Rollback(); err != nil {
			t.Fatalf("release playback ticket writer: %v", err)
		}
		assertDeadline(t, started, elapsedErr)
		if grant != nil {
			t.Fatalf("locked media session create returned grant: %+v", grant)
		}
		if after := readPlaybackTicketMutationSnapshot(t, store, binding); after != before {
			t.Fatalf("locked media session create changed rows\nbefore: %+v\nafter:  %+v",
				before, after)
		}
	})

	t.Run("touch media session", func(t *testing.T) {
		store := openPlaybackTestStore(t)
		fixture := addPlaybackTestFixture(t, store, "budget-touch", "budget-touch", now)
		_, grant := createActivePlaybackCapability(t, store, fixture, now)
		binding := fixture.sessionBinding(
			grant.Session.TicketID, models.PlaybackTransportServerProxy)
		before := readPlaybackMediaSessionContextSnapshot(
			t, store, grant.Session.ID, fixture.device.ID)
		writer := lockPlaybackMediaSessionWriter(t, store, grant.Capability)

		ctx, cancel := context.WithTimeout(context.Background(), writeBudget)
		defer cancel()
		started := time.Now()
		touched, err := store.TouchPlaybackMediaSessionContext(
			ctx, binding, grant.Capability, now.Add(2*time.Minute))
		elapsedErr := err
		if err := writer.Rollback(); err != nil {
			t.Fatalf("release playback media writer: %v", err)
		}
		assertDeadline(t, started, elapsedErr)
		if touched != nil {
			t.Fatalf("locked media session touch returned session: %+v", touched)
		}
		if after := readPlaybackMediaSessionContextSnapshot(
			t, store, grant.Session.ID, fixture.device.ID); after != before {
			t.Fatalf("locked media session touch changed rows\nbefore: %+v\nafter:  %+v",
				before, after)
		}
	})

	t.Run("revoke media session", func(t *testing.T) {
		store := openPlaybackTestStore(t)
		fixture := addPlaybackTestFixture(t, store, "budget-unmedia", "budget-unmedia", now)
		_, grant := createActivePlaybackCapability(t, store, fixture, now)
		binding := fixture.sessionBinding(
			grant.Session.TicketID, models.PlaybackTransportServerProxy)
		before := readPlaybackMediaSessionContextSnapshot(
			t, store, grant.Session.ID, fixture.device.ID)
		writer := lockPlaybackMediaSessionWriter(t, store, grant.Capability)

		ctx, cancel := context.WithTimeout(context.Background(), writeBudget)
		defer cancel()
		started := time.Now()
		revoked, err := store.RevokePlaybackMediaSessionContext(
			ctx, binding, grant.Capability, grant.Session.Version, now.Add(time.Minute))
		elapsedErr := err
		if err := writer.Rollback(); err != nil {
			t.Fatalf("release playback media writer: %v", err)
		}
		assertDeadline(t, started, elapsedErr)
		if revoked != nil {
			t.Fatalf("locked media session revoke returned session: %+v", revoked)
		}
		if after := readPlaybackMediaSessionContextSnapshot(
			t, store, grant.Session.ID, fixture.device.ID); after != before {
			t.Fatalf("locked media session revoke changed rows\nbefore: %+v\nafter:  %+v",
				before, after)
		}
	})

	t.Run("release transport claim", func(t *testing.T) {
		store := openPlaybackTestStore(t)
		fixture := addPlaybackTestFixture(t, store, "budget-release", "budget-release", now)
		ready := createReadyPlaybackTicket(t, store, fixture, now,
			[]string{models.PlaybackTransportServerProxy}, `{}`)
		claimed := claimPlaybackTransport(
			t, store, fixture, ready, models.PlaybackTransportServerProxy, now.Add(time.Second))
		binding := fixture.bindingWith(claimed.TicketID)
		before := readPlaybackTicketContextSnapshot(t, store, binding)
		writer := lockPlaybackTicketWriter(t, store, binding)

		ctx, cancel := context.WithTimeout(context.Background(), writeBudget)
		defer cancel()
		started := time.Now()
		err := store.ReleasePlaybackTransportClaim(
			ctx, binding, claimed.Version, claimed.SelectedTransport,
			claimed.ClaimToken, now.Add(2*time.Second))
		elapsedErr := err
		if err := writer.Rollback(); err != nil {
			t.Fatalf("release playback ticket writer: %v", err)
		}
		assertDeadline(t, started, elapsedErr)
		if after := readPlaybackTicketContextSnapshot(t, store, binding); after != before {
			t.Fatalf("locked claim release changed ticket\nbefore: %+v\nafter:  %+v",
				before, after)
		}
	})
}

func TestPlaybackRequestWritesHonorCancellationWhileWriterLocked(t *testing.T) {
	now := time.Date(2026, 10, 27, 13, 0, 0, 0, time.UTC)

	assertCanceled := func(t *testing.T, started time.Time, err error) {
		t.Helper()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("locked write error = %v, want context.Canceled", err)
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("locked write took %v, want <= 2s", elapsed)
		}
	}

	t.Run("revoke ticket", func(t *testing.T) {
		store := openPlaybackTestStore(t)
		fixture := addPlaybackTestFixture(t, store, "cancel-revoke", "cancel-revoke", now)
		ready := createReadyPlaybackTicket(t, store, fixture, now,
			[]string{models.PlaybackTransportServerProxy}, `{}`)
		binding := fixture.bindingWith(ready.TicketID)
		before := readPlaybackTicketContextSnapshot(t, store, binding)
		writer := lockPlaybackTicketWriter(t, store, binding)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		time.AfterFunc(100*time.Millisecond, cancel)
		started := time.Now()
		revoked, err := store.RevokePlaybackTicketContext(
			ctx, binding, ready.Version, now.Add(time.Second))
		elapsedErr := err
		if err := writer.Rollback(); err != nil {
			t.Fatalf("release playback ticket writer: %v", err)
		}
		assertCanceled(t, started, elapsedErr)
		if revoked != nil {
			t.Fatalf("canceled ticket revoke returned ticket: %+v", revoked)
		}
		if after := readPlaybackTicketContextSnapshot(t, store, binding); after != before {
			t.Fatalf("canceled ticket revoke changed ticket\nbefore: %+v\nafter:  %+v",
				before, after)
		}
	})

	t.Run("touch media session", func(t *testing.T) {
		store := openPlaybackTestStore(t)
		fixture := addPlaybackTestFixture(t, store, "cancel-touch", "cancel-touch", now)
		_, grant := createActivePlaybackCapability(t, store, fixture, now)
		binding := fixture.sessionBinding(
			grant.Session.TicketID, models.PlaybackTransportServerProxy)
		before := readPlaybackMediaSessionContextSnapshot(
			t, store, grant.Session.ID, fixture.device.ID)
		writer := lockPlaybackMediaSessionWriter(t, store, grant.Capability)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		time.AfterFunc(100*time.Millisecond, cancel)
		started := time.Now()
		touched, err := store.TouchPlaybackMediaSessionContext(
			ctx, binding, grant.Capability, now.Add(2*time.Minute))
		elapsedErr := err
		if err := writer.Rollback(); err != nil {
			t.Fatalf("release playback media writer: %v", err)
		}
		assertCanceled(t, started, elapsedErr)
		if touched != nil {
			t.Fatalf("canceled media session touch returned session: %+v", touched)
		}
		if after := readPlaybackMediaSessionContextSnapshot(
			t, store, grant.Session.ID, fixture.device.ID); after != before {
			t.Fatalf("canceled media session touch changed rows\nbefore: %+v\nafter:  %+v",
				before, after)
		}
	})
}

func playbackTableCount(t *testing.T, store *Store, table string) int {
	t.Helper()
	var count int
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
		t.Fatalf("count %s rows: %v", table, err)
	}
	return count
}
