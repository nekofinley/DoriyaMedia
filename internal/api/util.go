package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"doriyamedia/internal/media"
)

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

type videoInfo struct {
	Codec     string  `json:"codec"`
	Tag       string  `json:"tag,omitempty"`
	Profile   string  `json:"profile,omitempty"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	FrameRate float64 `json:"frame_rate,omitempty"`
	DVProfile int     `json:"dv_profile,omitempty"`
	BitRate   int64   `json:"bit_rate,omitempty"`
}

type audioInfo struct {
	Ordinal  int    `json:"ordinal"`
	Index    int    `json:"index"`
	Codec    string `json:"codec"`
	Lang     string `json:"lang,omitempty"`
	Title    string `json:"title,omitempty"`
	Channels int    `json:"channels,omitempty"`
	Selected bool   `json:"selected"`
}

type mediaSummary struct {
	Container string      `json:"container,omitempty"`
	Duration  float64     `json:"duration,omitempty"`
	BitRate   int64       `json:"bit_rate,omitempty"`
	Video     *videoInfo  `json:"video,omitempty"`
	Audio     []audioInfo `json:"audio"`
}

// probeSummary renders a compact summary from stored probe JSON.
func probeSummary(raw string) *mediaSummary {
	var p media.Probe
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil
	}
	out := &mediaSummary{
		Container: p.Format.FormatName,
		Duration:  p.Duration(),
		BitRate:   p.BitRate(),
		Audio:     []audioInfo{},
	}
	if v := p.Video(); v != nil {
		out.Video = &videoInfo{
			Codec: v.CodecName, Tag: v.CodecTagString, Profile: v.Profile,
			Width: v.Width, Height: v.Height, FrameRate: v.FrameRate(),
			DVProfile: v.DVProfile(), BitRate: v.BitRateVal(),
		}
	}
	for i, a := range p.AudioStreams() {
		out.Audio = append(out.Audio, audioInfo{
			Ordinal: i, Index: a.Index, Codec: a.CodecName, Lang: a.Language(),
			Title: a.Title(), Channels: a.Channels,
		})
	}
	return out
}

// audioOrdinal returns the ordinal of a stream index among audio streams (-1 if absent).
func audioOrdinal(p *media.Probe, streamIndex int) int {
	for i, a := range p.AudioStreams() {
		if a.Index == streamIndex {
			return i
		}
	}
	return -1
}

func stripVTT(s string) string {
	return strings.TrimSuffix(strings.TrimSuffix(s, ".vtt"), ".VTT")
}

func parseSegID(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}
