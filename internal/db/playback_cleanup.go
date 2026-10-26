package db

import (
	"time"

	"doriyamedia/internal/models"
)

const playbackCleanupRetention = 24 * time.Hour

// PlaybackCleanupStats reports one bounded cleanup pass. Duration counters are
// useful for tests and operational logs; they contain no ticket or source data.
type PlaybackCleanupStats struct {
	ExpiredTickets          int64
	ExpiredMediaSessions    int64
	ExpiredSubtitleSessions int64
	DeletedTickets          int64
	DeletedMediaSessions    int64
	DeletedSubtitleSessions int64
}

// CleanupPlayback expires overdue rows, then deletes terminal rows older than
// the retention window. Ticket rows with an active media session are retained:
// the decision ticket is short-lived, while the authorized media session may
// legitimately run for up to six hours.
func (s *Store) CleanupPlayback(now time.Time) (PlaybackCleanupStats, error) {
	var stats PlaybackCleanupStats
	now = playbackTime(now)
	cutoff := now.Add(-playbackCleanupRetention)

	tx, err := s.DB.Begin()
	if err != nil {
		return stats, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`UPDATE playback_tickets
		SET state = ?, version = version + 1, updated_at = ?
		WHERE state IN (?,?,?) AND expires_at <= ?`,
		models.PlaybackTicketExpired, now,
		models.PlaybackTicketProbing, models.PlaybackTicketReady,
		models.PlaybackTicketConsuming, now)
	if err != nil {
		return stats, err
	}
	stats.ExpiredTickets, err = res.RowsAffected()
	if err != nil {
		return stats, err
	}

	res, err = tx.Exec(`UPDATE playback_media_sessions
		SET state = ?, version = version + 1, updated_at = ?
		WHERE state = ? AND (idle_expires_at <= ? OR absolute_expires_at <= ?)`,
		models.PlaybackMediaSessionExpired, now,
		models.PlaybackMediaSessionActive, now, now)
	if err != nil {
		return stats, err
	}
	stats.ExpiredMediaSessions, err = res.RowsAffected()
	if err != nil {
		return stats, err
	}

	res, err = tx.Exec(`UPDATE playback_subtitle_sessions
		SET state = ?, version = version + 1, updated_at = ?
		WHERE state = ? AND (idle_expires_at <= ? OR absolute_expires_at <= ?)`,
		models.PlaybackSubtitleSessionExpired, now,
		models.PlaybackSubtitleSessionActive, now, now)
	if err != nil {
		return stats, err
	}
	stats.ExpiredSubtitleSessions, err = res.RowsAffected()
	if err != nil {
		return stats, err
	}

	res, err = tx.Exec(`DELETE FROM playback_media_sessions
		WHERE state IN (?,?) AND updated_at <= ?`,
		models.PlaybackMediaSessionExpired,
		models.PlaybackMediaSessionRevoked, cutoff)
	if err != nil {
		return stats, err
	}
	stats.DeletedMediaSessions, err = res.RowsAffected()
	if err != nil {
		return stats, err
	}

	res, err = tx.Exec(`DELETE FROM playback_subtitle_sessions
		WHERE state IN (?,?) AND updated_at <= ?`,
		models.PlaybackSubtitleSessionExpired,
		models.PlaybackSubtitleSessionRevoked, cutoff)
	if err != nil {
		return stats, err
	}
	stats.DeletedSubtitleSessions, err = res.RowsAffected()
	if err != nil {
		return stats, err
	}

	res, err = tx.Exec(`DELETE FROM playback_tickets
		WHERE state IN (?,?,?,?,?) AND updated_at <= ?
			AND NOT EXISTS (
				SELECT 1 FROM playback_media_sessions
				WHERE playback_media_sessions.ticket_id = playback_tickets.ticket_id
					AND playback_media_sessions.state = ?
			)
			AND NOT EXISTS (
				SELECT 1 FROM playback_subtitle_sessions
				WHERE playback_subtitle_sessions.ticket_id = playback_tickets.ticket_id
					AND playback_subtitle_sessions.state = ?
			)`,
		models.PlaybackTicketUnsupported,
		models.PlaybackTicketFailed,
		models.PlaybackTicketExpired,
		models.PlaybackTicketRevoked,
		models.PlaybackTicketExhausted,
		cutoff,
		models.PlaybackMediaSessionActive,
		models.PlaybackSubtitleSessionActive)
	if err != nil {
		return stats, err
	}
	stats.DeletedTickets, err = res.RowsAffected()
	if err != nil {
		return stats, err
	}
	if err := tx.Commit(); err != nil {
		return PlaybackCleanupStats{}, err
	}
	return stats, nil
}
