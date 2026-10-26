package api

import (
	"net/http"

	"doriyamedia/internal/config"
)

const v1ContractVersion = 1

type v1MetaResponse struct {
	ContractVersion       int                     `json:"contract_version"`
	ServerVersion         string                  `json:"server_version"`
	MinimumClientVersions v1MinimumClientVersions `json:"minimum_client_versions"`
	Features              v1FeatureFlags          `json:"features"`
}

type v1MinimumClientVersions struct {
	Windows string `json:"windows"`
	Android string `json:"android"`
}

// v1FeatureFlags is intentionally closed so discovery cannot drift from the
// published contract when a flag is renamed.
type v1FeatureFlags struct {
	ClientV1Enabled       bool `json:"client_v1_enabled"`
	DeviceAuthEnabled     bool `json:"device_auth_enabled"`
	PlaybackTicketEnabled bool `json:"playback_ticket_enabled"`
	DirectHTTPEnabled     bool `json:"direct_http_enabled"`
	ServerProxyV1Enabled  bool `json:"server_proxy_v1_enabled"`
	ServerHLSV1Enabled    bool `json:"server_hls_v1_enabled"`
	DirectDASHEnabled     bool `json:"direct_dash_enabled"`
	LocalRelayEnabled     bool `json:"local_relay_enabled"`
	NativePlayerEnabled   bool `json:"native_player_enabled"`
	LibMPVFallbackEnabled bool `json:"libmpv_fallback_enabled"`
}

func (s *Server) handleV1Meta(w http.ResponseWriter, r *http.Request) {
	version := s.Version
	if version == "" {
		version = "dev"
	}
	features := v1FeatureFlags{}
	if s.Cfg != nil {
		features = v1FeatureFlagsFromConfig(s.Cfg.Client)
	}
	writeJSON(w, http.StatusOK, v1MetaResponse{
		ContractVersion: v1ContractVersion,
		ServerVersion:   version,
		MinimumClientVersions: v1MinimumClientVersions{
			Windows: "0.1.0",
			Android: "0.1.0",
		},
		Features: features,
	})
}

func v1FeatureFlagsFromConfig(c config.ClientFeatures) v1FeatureFlags {
	return v1FeatureFlags{
		ClientV1Enabled:       c.V1Enabled,
		DeviceAuthEnabled:     c.DeviceAuthEnabled,
		PlaybackTicketEnabled: c.PlaybackTicketEnabled,
		DirectHTTPEnabled:     c.DirectHTTPEnabled,
		ServerProxyV1Enabled:  c.ServerProxyV1Enabled,
		ServerHLSV1Enabled:    c.ServerHLSV1Enabled,
		DirectDASHEnabled:     c.DirectDASHEnabled,
		LocalRelayEnabled:     c.LocalRelayEnabled,
		NativePlayerEnabled:   c.NativePlayerEnabled,
		LibMPVFallbackEnabled: c.LibMPVFallbackEnabled,
	}
}
