package models

import "testing"

func TestDeviceFeatureOverridesDenyOnlySemantics(t *testing.T) {
	tests := []struct {
		name     string
		override DeviceFeatureOverrides
	}{
		{"client v1", DeviceFeatureOverrides{ClientV1Disabled: true}},
		{"device auth", DeviceFeatureOverrides{DeviceAuthDisabled: true}},
		{"playback ticket", DeviceFeatureOverrides{PlaybackTicketDisabled: true}},
		{"direct http", DeviceFeatureOverrides{DirectHTTPDisabled: true}},
		{"server proxy", DeviceFeatureOverrides{ServerProxyV1Disabled: true}},
		{"server hls", DeviceFeatureOverrides{ServerHLSV1Disabled: true}},
		{"direct dash", DeviceFeatureOverrides{DirectDASHDisabled: true}},
		{"local relay", DeviceFeatureOverrides{LocalRelayDisabled: true}},
		{"native player", DeviceFeatureOverrides{NativePlayerDisabled: true}},
		{"libmpv fallback", DeviceFeatureOverrides{LibMPVFallbackDisabled: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.override.Empty() {
				t.Fatal("single deny override reported empty")
			}
			if !tc.override.DisablesPlayback() {
				t.Fatal("playback emergency stop did not revoke issued capabilities")
			}
		})
	}

	if !(DeviceFeatureOverrides{}).Empty() {
		t.Fatal("zero overrides reported non-empty")
	}
	if (DeviceFeatureOverrides{}).DisablesPlayback() {
		t.Fatal("zero overrides disabled playback")
	}
	if !(DeviceFeatureOverrides{ClientV1Disabled: true}).DisablesPlaybackTickets() {
		t.Fatal("client v1 deny did not fail playback tickets closed")
	}
	if !(DeviceFeatureOverrides{DeviceAuthDisabled: true}).DisablesPlaybackTickets() {
		t.Fatal("device auth deny did not fail playback tickets closed")
	}
	if !(DeviceFeatureOverrides{PlaybackTicketDisabled: true}).DisablesPlaybackTickets() {
		t.Fatal("playback ticket deny did not fail playback tickets closed")
	}
	if (DeviceFeatureOverrides{NativePlayerDisabled: true}).DisablesPlaybackTickets() {
		t.Fatal("client engine deny should not prevent alternate server transport")
	}
}
