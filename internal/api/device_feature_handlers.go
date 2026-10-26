package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"doriyamedia/internal/config"
	"doriyamedia/internal/db"
	"doriyamedia/internal/models"
)

const adminClientDeviceBodyLimit = 16 << 10

type adminClientDeviceListResponse struct {
	Devices []models.ClientDevice `json:"devices"`
}

// v1EffectiveClientFeatures is deliberately an AND. A device override can
// only turn an already enabled global feature off, never turn a globally
// disabled feature on.
func v1EffectiveClientFeatures(
	global config.ClientFeatures,
	overrides models.DeviceFeatureOverrides,
) config.ClientFeatures {
	global.V1Enabled = global.V1Enabled && !overrides.ClientV1Disabled
	global.DeviceAuthEnabled = global.DeviceAuthEnabled && !overrides.DeviceAuthDisabled
	global.PlaybackTicketEnabled = global.PlaybackTicketEnabled &&
		!overrides.PlaybackTicketDisabled
	global.DirectHTTPEnabled = global.DirectHTTPEnabled && !overrides.DirectHTTPDisabled
	global.ServerProxyV1Enabled = global.ServerProxyV1Enabled &&
		!overrides.ServerProxyV1Disabled
	global.ServerHLSV1Enabled = global.ServerHLSV1Enabled &&
		!overrides.ServerHLSV1Disabled
	global.DirectDASHEnabled = global.DirectDASHEnabled && !overrides.DirectDASHDisabled
	global.LocalRelayEnabled = global.LocalRelayEnabled && !overrides.LocalRelayDisabled
	global.NativePlayerEnabled = global.NativePlayerEnabled && !overrides.NativePlayerDisabled
	global.LibMPVFallbackEnabled = global.LibMPVFallbackEnabled &&
		!overrides.LibMPVFallbackDisabled
	return global
}

func (s *Server) effectiveClientFeatures(
	overrides models.DeviceFeatureOverrides,
) config.ClientFeatures {
	if s == nil || s.Cfg == nil {
		return config.ClientFeatures{}
	}
	return v1EffectiveClientFeatures(s.Cfg.Client, overrides)
}

func writeV1DeviceFeatureDisabled(w http.ResponseWriter) {
	writeV1Error(w, http.StatusForbidden, V1ErrorFeatureDisabled,
		"This device feature is disabled.", map[string]any{
			"action": "contact_admin",
		})
}

func (s *Server) handleAdminListClientDevices(
	w http.ResponseWriter,
	r *http.Request,
	_ *models.User,
) {
	if s == nil || s.Store == nil {
		fail(w, http.StatusInternalServerError, "无法读取客户端设备")
		return
	}
	devices, err := s.Store.ClientDevices(500)
	if err != nil {
		fail(w, http.StatusInternalServerError, "无法读取客户端设备")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, adminClientDeviceListResponse{Devices: devices})
}

func (s *Server) handleAdminUpdateClientDeviceFeatures(
	w http.ResponseWriter,
	r *http.Request,
	admin *models.User,
) {
	if s == nil || s.Store == nil || admin == nil {
		fail(w, http.StatusInternalServerError, "无法更新客户端设备")
		return
	}
	deviceID := strings.TrimSpace(r.PathValue("id"))
	if !v1DeviceIDPattern.MatchString(deviceID) {
		fail(w, http.StatusNotFound, "客户端设备不存在")
		return
	}
	var overrides models.DeviceFeatureOverrides
	if err := decodeAdminClientDeviceOverrides(w, r, &overrides); err != nil {
		fail(w, http.StatusBadRequest, "设备功能覆盖设置无效")
		return
	}
	device, err := s.Store.UpdateDeviceFeatureOverrides(
		deviceID, overrides, admin.ID, time.Now().UTC())
	if err != nil {
		switch {
		case errors.Is(err, db.ErrDeviceNotFound):
			fail(w, http.StatusNotFound, "客户端设备不存在")
		case errors.Is(err, db.ErrInvalidDeviceFeatureOverrides):
			fail(w, http.StatusBadRequest, "设备功能覆盖设置无效")
		default:
			fail(w, http.StatusInternalServerError, "无法更新客户端设备")
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, device)
}

func decodeAdminClientDeviceOverrides(
	w http.ResponseWriter,
	r *http.Request,
	dst *models.DeviceFeatureOverrides,
) error {
	contentType := strings.TrimSpace(r.Header.Get("Content-Type"))
	if contentType == "" {
		return errors.New("Content-Type must be application/json")
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return errors.New("Content-Type must be application/json")
	}
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, adminClientDeviceBodyLimit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var decoded *models.DeviceFeatureOverrides
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	if decoded == nil {
		return errors.New("request body must be a JSON object")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("request body must contain one JSON object")
	}
	*dst = *decoded
	return nil
}
