package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"doriyamedia/internal/db"
	"doriyamedia/internal/media"
	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

type playbackTestProbeRunner struct {
	probeFor       func(context.Context, *models.MediaFile, source.Driver) (*media.Probe, error)
	probeTransient func(context.Context, *models.MediaFile, source.Driver) (*media.Probe, error)
}

func (r *playbackTestProbeRunner) ProbeFor(
	ctx context.Context,
	file *models.MediaFile,
	drv source.Driver,
) (*media.Probe, error) {
	if r == nil || r.probeFor == nil {
		return nil, errors.New("unexpected playback ProbeFor call")
	}
	return r.probeFor(ctx, file, drv)
}

func (r *playbackTestProbeRunner) ProbeTransient(
	ctx context.Context,
	file *models.MediaFile,
	drv source.Driver,
) (*media.Probe, error) {
	if r == nil || r.probeTransient == nil {
		return nil, errors.New("unexpected playback ProbeTransient call")
	}
	return r.probeTransient(ctx, file, drv)
}

func usePlaybackTestProbeRunner(
	t *testing.T,
	runner v1PlaybackTicketProbeRunner,
) {
	t.Helper()
	previous := v1PlaybackTicketProbeRunnerFor
	v1PlaybackTicketProbeRunnerFor = func(*Server) v1PlaybackTicketProbeRunner {
		return runner
	}
	t.Cleanup(func() {
		v1PlaybackTicketProbeRunnerFor = previous
	})
}

func playbackTestAccessContext(
	t *testing.T,
	f playbackAPIFixture,
) *v1DeviceAccessContext {
	t.Helper()
	access, err := f.server.Store.DeviceByAccessToken(f.access, time.Now().UTC())
	if err != nil {
		t.Fatalf("resolve playback test access: %v", err)
	}
	return &v1DeviceAccessContext{
		User:      access.User,
		Device:    access.Device,
		Overrides: access.Overrides,
	}
}

func playbackTicketCountForFile(
	t *testing.T,
	store *db.Store,
	fileID int64,
) int {
	t.Helper()
	var count int
	if err := store.DB.QueryRow(
		`SELECT COUNT(*) FROM playback_tickets WHERE file_id = ?`,
		fileID,
	).Scan(&count); err != nil {
		t.Fatalf("count playback tickets for file %d: %v", fileID, err)
	}
	return count
}

func TestPlaybackCreateInitialProbeContextFailureCreatesNoTicket(t *testing.T) {
	tests := []struct {
		name       string
		newContext func() (context.Context, context.CancelFunc)
		status     int
	}{
		{
			name: "canceled",
			newContext: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
			status: http.StatusRequestTimeout,
		},
		{
			name: "deadline",
			newContext: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(
					context.Background(), time.Now().Add(-time.Second))
			},
			status: http.StatusGatewayTimeout,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newPlaybackAPIFixture(t)
			f.server.Cfg.Client = playbackLifecycleFeatures(
				true, true, true, true, true, true,
			)
			fileID := seedPlaybackAPIFile(
				t, f.server.Store,
				"https://cdn.example.test/library/initial-probe.mp4",
				"",
			)
			access := playbackTestAccessContext(t, f)
			ctx, cancel := tc.newContext()
			defer cancel()

			probeCalled := false
			ticketCountDuringProbe := -1
			usePlaybackTestProbeRunner(t, &playbackTestProbeRunner{
				probeFor: func(
					probeCtx context.Context,
					_ *models.MediaFile,
					_ source.Driver,
				) (*media.Probe, error) {
					probeCalled = true
					ticketCountDuringProbe = playbackTicketCountForFile(
						t, f.server.Store, fileID)
					return nil, probeCtx.Err()
				},
			})

			body, err := json.Marshal(playbackCreateBody(
				fileID, f.deviceID, models.PlaybackTransportServerProxy))
			if err != nil {
				t.Fatalf("marshal playback create request: %v", err)
			}
			req := httptest.NewRequest(
				http.MethodPost,
				"/api/v1/playback/tickets",
				bytes.NewReader(body),
			).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			f.server.handleV1PlaybackTicketCreate(rec, req, access)

			if !probeCalled {
				t.Fatal("initial ProbeFor was not called")
			}
			if ticketCountDuringProbe != 0 {
				t.Fatalf("tickets during initial ProbeFor = %d, want 0",
					ticketCountDuringProbe)
			}
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body %s",
					rec.Code, tc.status, rec.Body.String())
			}
			if got := playbackTicketCountForFile(
				t, f.server.Store, fileID,
			); got != 0 {
				t.Fatalf("context failure left %d playback tickets", got)
			}
		})
	}
}

func TestPlaybackCreateCanceledCachedProbeCreatesNoTicket(t *testing.T) {
	tests := []struct {
		name       string
		newContext func() (context.Context, context.CancelFunc)
		status     int
	}{
		{
			name: "canceled",
			newContext: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
			status: http.StatusRequestTimeout,
		},
		{
			name: "deadline",
			newContext: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(
					context.Background(), time.Now().Add(-time.Second))
			},
			status: http.StatusGatewayTimeout,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newPlaybackAPIFixture(t)
			f.server.Cfg.Client = playbackLifecycleFeatures(
				true, true, true, true, true, true,
			)
			fileID := seedPlaybackAPIFile(
				t, f.server.Store,
				"https://cdn.example.test/library/cached-probe.mp4",
				playbackAPITestProbe,
			)
			access := playbackTestAccessContext(t, f)
			ctx, cancel := tc.newContext()
			defer cancel()

			ticketCountDuringProbe := -1
			usePlaybackTestProbeRunner(t, &playbackTestProbeRunner{
				probeFor: func(
					_ context.Context,
					file *models.MediaFile,
					_ source.Driver,
				) (*media.Probe, error) {
					ticketCountDuringProbe = playbackTicketCountForFile(
						t, f.server.Store, fileID)
					return media.ProbeFromCache(file), nil
				},
			})

			body, err := json.Marshal(playbackCreateBody(
				fileID, f.deviceID, models.PlaybackTransportServerProxy))
			if err != nil {
				t.Fatalf("marshal playback create request: %v", err)
			}
			req := httptest.NewRequest(
				http.MethodPost,
				"/api/v1/playback/tickets",
				bytes.NewReader(body),
			).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			f.server.handleV1PlaybackTicketCreate(rec, req, access)

			if ticketCountDuringProbe != 0 {
				t.Fatalf("tickets during cached ProbeFor = %d, want 0",
					ticketCountDuringProbe)
			}
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body %s",
					rec.Code, tc.status, rec.Body.String())
			}
			if got := playbackTicketCountForFile(
				t, f.server.Store, fileID,
			); got != 0 {
				t.Fatalf("canceled cached probe left %d playback tickets", got)
			}
		})
	}
}

func TestPlaybackCreateCanceledWhileWaitingForWriterCreatesNoTicket(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = playbackLifecycleFeatures(
		true, true, true, true, true, true,
	)
	fileID := seedPlaybackAPIFile(
		t, f.server.Store,
		"https://cdn.example.test/library/writer-cancel.mp4",
		playbackAPITestProbe,
	)
	access := playbackTestAccessContext(t, f)
	usePlaybackTestProbeRunner(t, &playbackTestProbeRunner{
		probeFor: func(
			_ context.Context,
			file *models.MediaFile,
			_ source.Driver,
		) (*media.Probe, error) {
			return media.ProbeFromCache(file), nil
		},
	})

	writer, err := f.server.Store.DB.Begin()
	if err != nil {
		t.Fatalf("begin writer lock: %v", err)
	}
	defer writer.Rollback()
	if _, err := writer.Exec(
		`UPDATE files SET probe = probe WHERE id = ?`, fileID); err != nil {
		t.Fatalf("acquire playback writer lock: %v", err)
	}

	body, err := json.Marshal(playbackCreateBody(
		fileID, f.deviceID, models.PlaybackTransportServerProxy))
	if err != nil {
		t.Fatalf("marshal playback create request: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/playback/tickets",
		bytes.NewReader(body),
	).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		f.server.handleV1PlaybackTicketCreate(rec, req, access)
		close(done)
	}()
	time.AfterFunc(100*time.Millisecond, cancel)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled playback create did not leave the writer wait")
	}
	if rec.Code != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want %d; body %s",
			rec.Code, http.StatusRequestTimeout, rec.Body.String())
	}
	if got := playbackTicketCountForFile(
		t, f.server.Store, fileID,
	); got != 0 {
		t.Fatalf("writer-canceled create left %d playback tickets", got)
	}
}

func TestPlaybackCreateInitialProbeSourceFailureCreatesFailedTicket(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = playbackLifecycleFeatures(
		true, true, true, true, true, true,
	)
	fileID := seedPlaybackAPIFile(
		t, f.server.Store,
		"https://cdn.example.test/library/initial-probe-failure.mp4",
		"",
	)
	access := playbackTestAccessContext(t, f)
	ticketCountDuringProbe := -1
	usePlaybackTestProbeRunner(t, &playbackTestProbeRunner{
		probeFor: func(
			context.Context,
			*models.MediaFile,
			source.Driver,
		) (*media.Probe, error) {
			ticketCountDuringProbe = playbackTicketCountForFile(
				t, f.server.Store, fileID)
			return nil, errors.New("source probe failed")
		},
	})

	body, err := json.Marshal(playbackCreateBody(
		fileID, f.deviceID, models.PlaybackTransportServerProxy))
	if err != nil {
		t.Fatalf("marshal playback create request: %v", err)
	}
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/playback/tickets",
		bytes.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	f.server.handleV1PlaybackTicketCreate(rec, req, access)

	if ticketCountDuringProbe != 0 {
		t.Fatalf("tickets during source-failing ProbeFor = %d, want 0",
			ticketCountDuringProbe)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s",
			rec.Code, http.StatusCreated, rec.Body.String())
	}
	var state, failureCode string
	if err := f.server.Store.DB.QueryRow(`
		SELECT state, failure_code FROM playback_tickets
		WHERE file_id = ?`, fileID).Scan(&state, &failureCode); err != nil {
		t.Fatalf("read failed playback ticket: %v", err)
	}
	if state != models.PlaybackTicketFailed ||
		failureCode != playbackFailureSourceUnavailable {
		t.Fatalf("source-failed ticket = state %q code %q", state, failureCode)
	}
}

func TestPlaybackTransientDriverContextErrorWithLiveProbeIsSourceFailure(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = playbackLifecycleFeatures(
		true, true, true, true, true, true,
	)
	fileID := seedPlaybackAPIFile(
		t, f.server.Store,
		"https://cdn.example.test/library/transient-context.mp4",
		playbackAPITestProbe,
	)
	ticket := playbackLifecycleCreateTicket(t, f, fileID)
	client, preferred, _, err := decodeStoredPlaybackCapabilities(
		ticket.ClientCapabilities)
	if err != nil {
		t.Fatalf("decode playback capabilities: %v", err)
	}
	called := false
	runner := &playbackTestProbeRunner{
		probeTransient: func(
			context.Context,
			*models.MediaFile,
			source.Driver,
		) (*media.Probe, error) {
			called = true
			return nil, context.Canceled
		},
	}

	f.server.completeV1PlaybackProbe(
		playbackBinding(ticket),
		ticket.Version,
		fileID,
		client,
		preferred,
		f.server.effectiveClientFeatures(models.DeviceFeatureOverrides{}),
		runner,
	)

	if !called {
		t.Fatal("background probe did not reach the driver")
	}
	var state, failureCode string
	if err := f.server.Store.DB.QueryRow(`
		SELECT state, failure_code FROM playback_tickets
		WHERE ticket_id = ?`, ticket.TicketID).Scan(&state, &failureCode); err != nil {
		t.Fatalf("read transient probe failure: %v", err)
	}
	if state != models.PlaybackTicketFailed ||
		failureCode != playbackFailureSourceUnavailable {
		t.Fatalf("transient probe failure = state %q code %q, want %q",
			state, failureCode, playbackFailureSourceUnavailable)
	}
}

func TestPlaybackFinalizeCASFailurePersistsProbeFinalizeFailed(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	f.server.Cfg.Client = playbackLifecycleFeatures(
		true, true, true, true, true, true,
	)
	fileID := seedPlaybackAPIFile(
		t, f.server.Store,
		"https://cdn.example.test/library/finalize-failure.mp4",
		playbackAPITestProbe,
	)
	usePlaybackTestProbeRunner(t, &playbackTestProbeRunner{
		probeFor: func(
			_ context.Context,
			file *models.MediaFile,
			_ source.Driver,
		) (*media.Probe, error) {
			probe := media.ProbeFromCache(file)
			if probe == nil {
				return nil, errors.New("playback fixture has no cached probe")
			}
			return probe, nil
		},
	})
	if _, err := f.server.Store.DB.Exec(`
		CREATE TRIGGER reject_playback_ready
		BEFORE UPDATE OF state ON playback_tickets
		WHEN NEW.state = 'ready'
		BEGIN
			SELECT RAISE(ABORT, 'forced ticket CAS failure');
		END`); err != nil {
		t.Fatalf("create forced finalize failure trigger: %v", err)
	}

	create := f.call(t, http.MethodPost, "/api/v1/playback/tickets",
		playbackCreateBody(fileID, f.deviceID, models.PlaybackTransportServerProxy))
	if create.Status != http.StatusInternalServerError {
		t.Fatalf("finalize failure create = %d; body %s",
			create.Status, create.Body)
	}
	location := create.Header.Get("Location")
	const ticketPrefix = "/api/v1/playback/tickets/"
	if !strings.HasPrefix(location, ticketPrefix) {
		t.Fatalf("finalize failure Location = %q", location)
	}
	ticketID := strings.TrimPrefix(location, ticketPrefix)
	var state, failureCode string
	if err := f.server.Store.DB.QueryRow(`
		SELECT state, failure_code FROM playback_tickets
		WHERE ticket_id = ?`, ticketID).Scan(&state, &failureCode); err != nil {
		t.Fatalf("read finalize-failed ticket: %v", err)
	}
	if state != models.PlaybackTicketFailed ||
		failureCode != playbackFailureFinalizeFailed {
		t.Fatalf("finalize-failed ticket = state %q code %q", state, failureCode)
	}

	status := f.call(t, http.MethodGet, location, nil)
	if status.Status != http.StatusOK {
		t.Fatalf("finalize-failed status = %d; body %s",
			status.Status, status.Body)
	}
	if got := status.JSON["failure_code"]; got != playbackFailureFinalizeFailed {
		t.Fatalf("public failure code = %v, want %q",
			got, playbackFailureFinalizeFailed)
	}
}

func TestPlaybackFinalizeContextEndPersistsProbeFinalizeFailed(t *testing.T) {
	f := newPlaybackAPIFixture(t)
	fileID := seedPlaybackAPIFile(
		t, f.server.Store,
		"https://cdn.example.test/library/finalize-context.mp4",
		playbackAPITestProbe,
	)
	ticket := playbackLifecycleCreateTicket(t, f, fileID)
	file, drv, err := f.server.v1PlaybackFileAndDriver(fileID)
	if err != nil {
		t.Fatalf("load finalize fixture: %v", err)
	}
	probe := media.ProbeFromCache(file)
	if probe == nil {
		t.Fatal("finalize fixture has no cached probe")
	}
	client, _, _, err := decodeStoredPlaybackCapabilities(ticket.ClientCapabilities)
	if err != nil {
		t.Fatalf("decode playback capabilities: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.server.finalizeV1PlaybackTicketCreate(
		ctx,
		playbackBinding(ticket),
		ticket,
		file,
		drv,
		probe,
		client,
		f.server.effectiveClientFeatures(models.DeviceFeatureOverrides{}),
	); err == nil {
		t.Fatal("canceled finalize context unexpectedly succeeded")
	}

	var state, failureCode string
	if err := f.server.Store.DB.QueryRow(`
		SELECT state, failure_code FROM playback_tickets
		WHERE ticket_id = ?`, ticket.TicketID).Scan(&state, &failureCode); err != nil {
		t.Fatalf("read canceled finalize failure: %v", err)
	}
	if state != models.PlaybackTicketFailed ||
		failureCode != playbackFailureFinalizeFailed {
		t.Fatalf("canceled finalize = state %q code %q, want %q",
			state, failureCode, playbackFailureFinalizeFailed)
	}
}
