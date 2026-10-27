package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"doriyamedia/internal/models"
)

var (
	ErrInvalidDeviceFeatureOverrides = errors.New("invalid device feature overrides")
	ErrDeviceV1Disabled              = errors.New("device v1 API disabled")
	ErrDeviceAuthFeatureDisabled     = errors.New("device authentication disabled")
	ErrDevicePlaybackDisabled        = errors.New("device playback disabled")
)

// Device overrides live in their own table so existing device scans stay
// unchanged. The row is optional; no row is the same as every deny flag being
// false. Only service code writes this table, and clients never supply it.
var deviceFeatureMigrationStatements = []string{
	`CREATE TABLE IF NOT EXISTS device_feature_overrides (
		device_id INTEGER PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
		overrides_json TEXT NOT NULL DEFAULT '{}',
		updated_at TIMESTAMP NOT NULL,
		updated_by INTEGER REFERENCES users(id) ON DELETE SET NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_device_feature_overrides_updated
		ON device_feature_overrides(updated_at)`,
}

const clientDeviceColumns = `d.public_id, d.user_id, u.name, d.name,
	d.platform, d.client_version, d.created_at, d.last_seen_at, d.revoked_at,
	COALESCE(o.overrides_json, '{}')`

// ClientDevices returns the admin-only safe projection. It deliberately does
// not select authorization ids, token hashes, IP hashes, or source data.
func (s *Store) ClientDevices(limit int) ([]models.ClientDevice, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.DB.Query(`SELECT `+clientDeviceColumns+`
		FROM devices d
		JOIN users u ON u.id = d.user_id
		LEFT JOIN device_feature_overrides o ON o.device_id = d.id
		ORDER BY d.created_at DESC, d.id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	devices := make([]models.ClientDevice, 0)
	for rows.Next() {
		device, err := scanClientDevice(rows)
		if err != nil {
			return nil, err
		}
		devices = append(devices, device)
	}
	return devices, rows.Err()
}

// Time overrides for a device are replaced as one set. Any playback deny flag
// revokes existing playback tickets and capabilities atomically; auth deny
// flags also revoke all current access and refresh tokens. Clearing an override
// never resurrects a token or capability revoked by an earlier emergency stop.
func (s *Store) UpdateDeviceFeatureOverrides(
	publicID string,
	overrides models.DeviceFeatureOverrides,
	updatedBy int64,
	now time.Time,
) (*models.ClientDevice, error) {
	now = deviceAuthTime(now)
	publicID = strings.TrimSpace(publicID)
	if publicID == "" || updatedBy <= 0 {
		return nil, ErrDeviceNotFound
	}
	overridesJSON, err := json.Marshal(overrides)
	if err != nil {
		return nil, ErrInvalidDeviceFeatureOverrides
	}

	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Take SQLite's writer reservation before reading the device. A no-op
	// update serializes this emergency stop with ticket creation and revocation.
	if _, err := tx.Exec(`UPDATE devices SET last_seen_at = last_seen_at
		WHERE public_id = ?`, publicID); err != nil {
		return nil, err
	}
	var deviceID int64
	if err := tx.QueryRow(`SELECT id FROM devices WHERE public_id = ?`,
		publicID).Scan(&deviceID); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDeviceNotFound
	} else if err != nil {
		return nil, err
	}

	if overrides.Empty() {
		if _, err := tx.Exec(`DELETE FROM device_feature_overrides
			WHERE device_id = ?`, deviceID); err != nil {
			return nil, err
		}
	} else {
		if _, err := tx.Exec(`INSERT INTO device_feature_overrides
			(device_id, overrides_json, updated_at, updated_by)
			VALUES (?,?,?,?)
			ON CONFLICT(device_id) DO UPDATE SET
				overrides_json = excluded.overrides_json,
				updated_at = excluded.updated_at,
				updated_by = excluded.updated_by`,
			deviceID, string(overridesJSON), now, updatedBy); err != nil {
			return nil, err
		}
	}

	if overrides.DisablesDeviceAuth() {
		if _, err := tx.Exec(`UPDATE device_access_tokens
			SET revoked_at = COALESCE(revoked_at, ?), revoked_reason = ?
			WHERE device_id = ? AND revoked_at IS NULL`,
			now, "device_feature_disabled", deviceID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE device_refresh_tokens
			SET revoked_at = COALESCE(revoked_at, ?), revoked_reason = ?
			WHERE device_id = ? AND revoked_at IS NULL`,
			now, "device_feature_disabled", deviceID); err != nil {
			return nil, err
		}
	}
	if overrides.DisablesPlayback() {
		if err := revokePlaybackForDeviceTx(tx, deviceID, now); err != nil {
			return nil, err
		}
	}

	device, err := scanClientDevice(tx.QueryRow(`SELECT `+clientDeviceColumns+`
		FROM devices d
		JOIN users u ON u.id = d.user_id
		LEFT JOIN device_feature_overrides o ON o.device_id = d.id
		WHERE d.id = ?`, deviceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDeviceNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &device, nil
}

// DeviceFeatureOverridesByDeviceID is used by focused authorization checks.
func (s *Store) DeviceFeatureOverridesByDeviceID(deviceID int64) (models.DeviceFeatureOverrides, error) {
	return loadDeviceFeatureOverrides(s.DB, deviceID)
}

type deviceFeatureQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadDeviceFeatureOverrides(queryer deviceFeatureQueryer, deviceID int64) (models.DeviceFeatureOverrides, error) {
	return loadDeviceFeatureOverridesContext(context.Background(), queryer, deviceID)
}

func loadDeviceFeatureOverridesContext(
	ctx context.Context,
	queryer deviceFeatureQueryer,
	deviceID int64,
) (models.DeviceFeatureOverrides, error) {
	if deviceID <= 0 {
		return models.DeviceFeatureOverrides{}, ErrDeviceNotFound
	}
	var raw string
	err := queryer.QueryRowContext(ctx, `SELECT overrides_json FROM device_feature_overrides
		WHERE device_id = ?`, deviceID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return models.DeviceFeatureOverrides{}, nil
	}
	if err != nil {
		return models.DeviceFeatureOverrides{}, err
	}
	return decodeDeviceFeatureOverrides(raw)
}

func decodeDeviceFeatureOverrides(raw string) (models.DeviceFeatureOverrides, error) {
	var overrides models.DeviceFeatureOverrides
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	dec := json.NewDecoder(bytes.NewBufferString(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&overrides); err != nil {
		return models.DeviceFeatureOverrides{}, ErrInvalidDeviceFeatureOverrides
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return models.DeviceFeatureOverrides{}, ErrInvalidDeviceFeatureOverrides
	}
	return overrides, nil
}

func requirePlaybackTicketsAllowedTx(tx *sql.Tx, deviceID int64) error {
	return requirePlaybackTicketsAllowedTxContext(context.Background(), tx, deviceID)
}

func requirePlaybackTicketsAllowedTxContext(
	ctx context.Context,
	tx *sql.Tx,
	deviceID int64,
) error {
	overrides, err := loadDeviceFeatureOverridesContext(ctx, tx, deviceID)
	if err != nil {
		return err
	}
	if overrides.DisablesPlaybackTickets() {
		return ErrDevicePlaybackDisabled
	}
	return nil
}

func requirePlaybackTransportAllowedTx(tx *sql.Tx, deviceID int64, transport string) error {
	return requirePlaybackTransportAllowedTxContext(
		context.Background(), tx, deviceID, transport)
}

func requirePlaybackTransportAllowedTxContext(
	ctx context.Context,
	tx *sql.Tx,
	deviceID int64,
	transport string,
) error {
	overrides, err := loadDeviceFeatureOverridesContext(ctx, tx, deviceID)
	if err != nil {
		return err
	}
	if overrides.DisablesPlaybackTickets() ||
		overrides.DisablesPlaybackTransport(transport) {
		return ErrDevicePlaybackDisabled
	}
	return nil
}

func scanClientDevice(row interface{ Scan(...any) error }) (models.ClientDevice, error) {
	var (
		device       models.ClientDevice
		revokedAt    sql.NullTime
		overridesRaw string
	)
	if err := row.Scan(&device.ID, &device.UserID, &device.UserName,
		&device.Name, &device.Platform, &device.ClientVersion, &device.CreatedAt,
		&device.LastSeenAt, &revokedAt, &overridesRaw); err != nil {
		return models.ClientDevice{}, err
	}
	overrides, err := decodeDeviceFeatureOverrides(overridesRaw)
	if err != nil {
		return models.ClientDevice{}, err
	}
	device.Overrides = overrides
	if revokedAt.Valid {
		t := revokedAt.Time
		device.RevokedAt = &t
	}
	return device, nil
}
