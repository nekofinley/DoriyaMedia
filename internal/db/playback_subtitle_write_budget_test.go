package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"doriyamedia/internal/models"
)

const playbackSubtitleWriteBudgetTestTimeout = 200 * time.Millisecond

func TestPlaybackSubtitleRequestWritesHonorContextWhileWriterLocked(t *testing.T) {
	now := time.Date(2026, 10, 27, 14, 0, 0, 0, time.UTC)
	modes := []struct {
		name   string
		want   error
		cancel bool
	}{
		{name: "deadline", want: context.DeadlineExceeded},
		{name: "cancel", want: context.Canceled, cancel: true},
	}

	for _, mode := range modes {
		t.Run("create/"+mode.name, func(t *testing.T) {
			store, _, ticket, subtitleID := addPlaybackSubtitleWriteFixture(
				t, "subtitle-budget-create-"+mode.name, now)
			binding := playbackSubtitleTicketBinding(ticket)
			ticketBefore := readPlaybackTicketContextSnapshot(t, store, binding)
			countBefore := countPlaybackSubtitleSessions(t, store)
			writer := lockPlaybackTicketWriter(t, store, binding)

			ctx, cancel := playbackSubtitleWriteContext(mode.cancel)
			defer cancel()
			started := time.Now()
			grant, err := store.CreatePlaybackSubtitleSessionContext(
				ctx,
				models.PlaybackSubtitleSessionRequest{
					Ticket:       binding,
					Transport:    models.PlaybackTransportDirectHTTP,
					SubtitleID:   subtitleID,
					StartSeconds: 12.5,
				},
				now.Add(3*time.Second),
			)
			elapsed := time.Since(started)
			if err := writer.Rollback(); err != nil {
				t.Fatalf("release playback ticket writer: %v", err)
			}

			assertPlaybackSubtitleContextError(t, mode.want, started, elapsed, err)
			if grant != nil {
				t.Fatalf("locked subtitle create returned grant: %+v", grant)
			}
			if countAfter := countPlaybackSubtitleSessions(t, store); countAfter != countBefore {
				t.Fatalf("locked subtitle create changed row count from %d to %d",
					countBefore, countAfter)
			}
			if ticketAfter := readPlaybackTicketContextSnapshot(
				t, store, binding); ticketAfter != ticketBefore {
				t.Fatalf("locked subtitle create changed ticket\nbefore: %+v\nafter:  %+v",
					ticketBefore, ticketAfter)
			}
		})

		t.Run("authorize/"+mode.name, func(t *testing.T) {
			store, fixture, ticket, subtitleID := addPlaybackSubtitleWriteFixture(
				t, "subtitle-budget-authorize-"+mode.name, now)
			grant := createPlaybackSubtitleWriteGrant(
				t, store, ticket, subtitleID, now.Add(3*time.Second))
			before := readPlaybackSubtitleContextSnapshot(
				t, store, grant.Session.ID, fixture.device.ID)
			writer := lockPlaybackSubtitleSessionWriter(t, store, grant.Capability)

			ctx, cancel := playbackSubtitleWriteContext(mode.cancel)
			defer cancel()
			started := time.Now()
			access, err := store.AuthorizePlaybackSubtitleSessionContext(
				ctx, grant.Capability, false, now.Add(time.Minute))
			elapsed := time.Since(started)
			if err := writer.Rollback(); err != nil {
				t.Fatalf("release playback subtitle writer: %v", err)
			}

			assertPlaybackSubtitleContextError(t, mode.want, started, elapsed, err)
			if access != nil {
				t.Fatalf("locked subtitle authorization returned access: %+v", access)
			}
			after := readPlaybackSubtitleContextSnapshot(
				t, store, grant.Session.ID, fixture.device.ID)
			if after != before {
				t.Fatalf("locked subtitle authorization changed rows\nbefore: %+v\nafter:  %+v",
					before, after)
			}
		})

		t.Run("revoke/"+mode.name, func(t *testing.T) {
			store, fixture, ticket, subtitleID := addPlaybackSubtitleWriteFixture(
				t, "subtitle-budget-revoke-"+mode.name, now)
			grant := createPlaybackSubtitleWriteGrant(
				t, store, ticket, subtitleID, now.Add(3*time.Second))
			before := readPlaybackSubtitleContextSnapshot(
				t, store, grant.Session.ID, fixture.device.ID)
			writer := lockPlaybackSubtitleSessionWriter(t, store, grant.Capability)

			ctx, cancel := playbackSubtitleWriteContext(mode.cancel)
			defer cancel()
			started := time.Now()
			revoked, err := store.RevokePlaybackSubtitleSessionContext(
				ctx, grant.Capability, grant.Session.Version, now.Add(time.Minute))
			elapsed := time.Since(started)
			if err := writer.Rollback(); err != nil {
				t.Fatalf("release playback subtitle writer: %v", err)
			}

			assertPlaybackSubtitleContextError(t, mode.want, started, elapsed, err)
			if revoked != nil {
				t.Fatalf("locked subtitle revoke returned session: %+v", revoked)
			}
			after := readPlaybackSubtitleContextSnapshot(
				t, store, grant.Session.ID, fixture.device.ID)
			if after != before {
				t.Fatalf("locked subtitle revoke changed rows\nbefore: %+v\nafter:  %+v",
					before, after)
			}
		})
	}
}

func addPlaybackSubtitleWriteFixture(
	t *testing.T,
	name string,
	now time.Time,
) (*Store, playbackTestFixture, *models.PlaybackTicket, int64) {
	t.Helper()
	store := openPlaybackTestStore(t)
	fixture := addPlaybackTestFixture(t, store, name, name, now)
	subtitleID := addPlaybackSubtitle(
		t, store, fixture.fileID, "/"+name+"/subtitle.srt", "subrip", false, 0)
	ticket := createConsumedPlaybackTicket(
		t, store, fixture, now, []int64{subtitleID})
	return store, fixture, ticket, subtitleID
}

func createPlaybackSubtitleWriteGrant(
	t *testing.T,
	store *Store,
	ticket *models.PlaybackTicket,
	subtitleID int64,
	now time.Time,
) *models.PlaybackSubtitleSessionGrant {
	t.Helper()
	grant, err := store.CreatePlaybackSubtitleSession(
		models.PlaybackSubtitleSessionRequest{
			Ticket:       playbackSubtitleTicketBinding(ticket),
			Transport:    models.PlaybackTransportDirectHTTP,
			SubtitleID:   subtitleID,
			StartSeconds: 12.5,
		},
		now,
	)
	if err != nil {
		t.Fatalf("create playback subtitle grant: %v", err)
	}
	return grant
}

func playbackSubtitleWriteContext(cancel bool) (context.Context, context.CancelFunc) {
	if !cancel {
		return context.WithTimeout(
			context.Background(), playbackSubtitleWriteBudgetTestTimeout)
	}
	ctx, cancelFn := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancelFn)
	return ctx, cancelFn
}

func assertPlaybackSubtitleContextError(
	t *testing.T,
	want error,
	started time.Time,
	elapsed time.Duration,
	err error,
) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("locked subtitle write error = %v, want %v", err, want)
	}
	if elapsed > 2*time.Second || time.Since(started) > 2*time.Second {
		t.Fatalf("locked subtitle write was not promptly canceled: elapsed=%v", elapsed)
	}
}

func countPlaybackSubtitleSessions(t *testing.T, store *Store) int {
	t.Helper()
	var count int
	if err := store.DB.QueryRow(
		`SELECT COUNT(*) FROM playback_subtitle_sessions`).Scan(&count); err != nil {
		t.Fatalf("count playback subtitle sessions: %v", err)
	}
	return count
}

type playbackSubtitleContextSnapshot struct {
	id                int64
	capabilityHash    string
	ticketID          string
	userID            int64
	deviceID          int64
	fileID            int64
	purpose           string
	transport         string
	subtitleID        int64
	state             string
	startSeconds      float64
	version           int64
	createdAt         time.Time
	lastUsedAt        sql.NullTime
	idleExpiresAt     time.Time
	absoluteExpiresAt time.Time
	updatedAt         time.Time
	revokedAt         sql.NullTime
	deviceLastSeen    time.Time
}

func readPlaybackSubtitleContextSnapshot(
	t *testing.T,
	store *Store,
	sessionID, deviceID int64,
) playbackSubtitleContextSnapshot {
	t.Helper()
	var snapshot playbackSubtitleContextSnapshot
	if err := store.DB.QueryRow(`SELECT id, capability_hash, ticket_id, user_id,
		device_id, file_id, purpose, transport, subtitle_id, state, start_seconds,
		version, created_at, last_used_at, idle_expires_at, absolute_expires_at,
		updated_at, revoked_at
		FROM playback_subtitle_sessions WHERE id = ?`, sessionID).Scan(
		&snapshot.id,
		&snapshot.capabilityHash,
		&snapshot.ticketID,
		&snapshot.userID,
		&snapshot.deviceID,
		&snapshot.fileID,
		&snapshot.purpose,
		&snapshot.transport,
		&snapshot.subtitleID,
		&snapshot.state,
		&snapshot.startSeconds,
		&snapshot.version,
		&snapshot.createdAt,
		&snapshot.lastUsedAt,
		&snapshot.idleExpiresAt,
		&snapshot.absoluteExpiresAt,
		&snapshot.updatedAt,
		&snapshot.revokedAt,
	); err != nil {
		t.Fatalf("read playback subtitle context snapshot: %v", err)
	}
	if err := store.DB.QueryRow(`SELECT last_seen_at FROM devices
		WHERE id = ?`, deviceID).Scan(&snapshot.deviceLastSeen); err != nil {
		t.Fatalf("read device last seen for playback subtitle snapshot: %v", err)
	}
	return snapshot
}

func lockPlaybackSubtitleSessionWriter(
	t *testing.T,
	store *Store,
	capability string,
) *sql.Tx {
	t.Helper()
	tx, err := store.DB.Begin()
	if err != nil {
		t.Fatalf("begin playback subtitle writer lock: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.Exec(`UPDATE playback_subtitle_sessions SET version = version
		WHERE capability_hash = ?`, hashPlaybackCapability(capability)); err != nil {
		t.Fatalf("hold playback subtitle writer lock: %v", err)
	}
	return tx
}
