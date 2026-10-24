package media

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ProbeStream mirrors the ffprobe -show_streams JSON (subset).
type ProbeStream struct {
	Index          int               `json:"index"`
	CodecType      string            `json:"codec_type"`
	CodecName      string            `json:"codec_name"`
	CodecTagString string            `json:"codec_tag_string"`
	CodecTag       string            `json:"codec_tag"`
	Profile        string            `json:"profile"`
	Width          int               `json:"width"`
	Height         int               `json:"height"`
	Channels       int               `json:"channels"`
	AvgFrameRate   string            `json:"avg_frame_rate"`
	BitRate        json.Number       `json:"bit_rate"`
	SideDataList   []SideData        `json:"side_data_list"`
	Disposition    json.RawMessage   `json:"disposition"`
	Tags           map[string]string `json:"tags"`
}

type SideData struct {
	SideDataType string `json:"side_data_type"`
	DVProfile    int    `json:"dv_profile"`
	DVLevel      int    `json:"dv_level"`
}

type ProbeFormat struct {
	FormatName string            `json:"format_name"`
	Duration   json.Number       `json:"duration"`
	Size       json.Number       `json:"size"`
	BitRate    json.Number       `json:"bit_rate"`
	FormatTags map[string]string `json:"tags"`
}

type Probe struct {
	Streams []ProbeStream `json:"streams"`
	Format  ProbeFormat   `json:"format"`
}

func (p *Probe) Video() *ProbeStream {
	for i := range p.Streams {
		if p.Streams[i].CodecType == "video" && !p.Streams[i].IsCoverArt() {
			return &p.Streams[i]
		}
	}
	return nil
}

func (p *Probe) AudioStreams() []*ProbeStream {
	var out []*ProbeStream
	for i := range p.Streams {
		if p.Streams[i].CodecType == "audio" {
			out = append(out, &p.Streams[i])
		}
	}
	return out
}

func (p *Probe) SubtitleStreams() []*ProbeStream {
	var out []*ProbeStream
	for i := range p.Streams {
		if p.Streams[i].CodecType == "subtitle" {
			out = append(out, &p.Streams[i])
		}
	}
	return out
}

func (p *Probe) Duration() float64 {
	if d, err := p.Format.Duration.Float64(); err == nil && d > 0 {
		return d
	}
	return 0
}

// IsCoverArt filters mjpeg/png cover attachments inside mkv/mp4.
// Covers are usually flagged attached_pic=1 by ffprobe, but some muxes skip
// the flag — treat bare mjpeg/png/bmp/gif video streams as covers regardless.
func (s *ProbeStream) IsCoverArt() bool {
	if s.Disposition != nil {
		var d map[string]int
		if err := json.Unmarshal(s.Disposition, &d); err == nil && d["attached_pic"] == 1 {
			return true
		}
	}
	switch s.CodecName {
	case "mjpeg", "png", "bmp", "gif":
		return true
	}
	return false
}

// DVProfile returns the Dolby Vision profile, 0 when not DV.
func (s *ProbeStream) DVProfile() int {
	for _, sd := range s.SideDataList {
		if strings.Contains(strings.ToLower(sd.SideDataType), "dovi") || sd.DVProfile > 0 {
			return sd.DVProfile
		}
	}
	tag := strings.ToLower(s.CodecTagString)
	if strings.HasPrefix(tag, "dvh") || strings.HasPrefix(tag, "dvhe") {
		// profile unknown but DV present; assume 5 (most common for web sources)
		return 5
	}
	return 0
}

func (s *ProbeStream) Language() string {
	if s.Tags != nil {
		if v, ok := s.Tags["language"]; ok && v != "" && v != "und" {
			return v
		}
	}
	return ""
}

func (s *ProbeStream) Title() string {
	if s.Tags != nil {
		return s.Tags["title"]
	}
	return ""
}

// ---------- probing ----------

type Prober struct {
	FFProbe string
	// ProbeSizeMB / AnalyzeSec bound how much of the source is read before
	// streams are known. Zero falls back to the historical deep window.
	ProbeSizeMB int
	AnalyzeSec  int
}

// deepProbeSizeMB / deepProbeAnalyzeSec are the historical fixed window. They
// read ~60MB per file, so they are only used as the automatic fallback for
// muxes whose streams sit past a small probe window.
const (
	deepProbeSizeMB     = 50
	deepProbeAnalyzeSec = 20
)

func NewProber(ffprobe string) *Prober {
	return &Prober{FFProbe: ffprobe, ProbeSizeMB: 8, AnalyzeSec: 4}
}

// ProbeFile runs ffprobe against a local path or a URL with optional headers.
func (p *Prober) ProbeFile(ctx context.Context, target string, headers map[string]string) (*Probe, error) {
	return p.ProbeFileVia(ctx, target, headers, "")
}

// ProbeFileVia is ProbeFile with a proxy: proxy is an http:// URL ffprobe
// tunnels through, or "" to dial directly. ffprobe's own http protocol only
// honours an http:// proxy, so a source behind SOCKS5 or Shadowsocks is handed
// a loopback bridge (see netproxy.Bridge) — the same one its ffmpeg inputs use.
func (p *Prober) ProbeFileVia(ctx context.Context, target string, headers map[string]string, proxy string) (*Probe, error) {
	cctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	sizeMB, analyzeSec := p.ProbeSizeMB, p.AnalyzeSec
	if sizeMB <= 0 {
		sizeMB = deepProbeSizeMB
	}
	if analyzeSec <= 0 {
		analyzeSec = deepProbeAnalyzeSec
	}

	args := []string{
		"-v", "quiet",
		"-print_format", "json",
		"-show_format", "-show_streams",
		"-analyzeduration", strconv.Itoa(analyzeSec * 1_000_000),
		"-probesize", strconv.Itoa(sizeMB << 20),
	}
	if len(headers) > 0 {
		var b strings.Builder
		for k, v := range headers {
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(v)
			b.WriteString("\r\n")
		}
		args = append(args, "-headers", b.String())
	}
	args = ProxyArgs(args, proxy)
	args = append(args, target)

	cmd := exec.CommandContext(cctx, p.FFProbe, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	var probe Probe
	if err := json.Unmarshal(out, &probe); err != nil {
		return nil, fmt.Errorf("ffprobe json: %w", err)
	}
	if probe.Format.FormatName == "" && len(probe.Streams) == 0 {
		return nil, fmt.Errorf("ffprobe: empty result")
	}
	return &probe, nil
}

func (p *Probe) BitRate() int64 {
	if b, err := p.Format.BitRate.Int64(); err == nil {
		return b
	}
	return 0
}

func (s *ProbeStream) BitRateVal() int64 {
	if b, err := s.BitRate.Int64(); err == nil {
		return b
	}
	return 0
}

// FrameRate returns ffprobe's average frame rate as frames per second.
func (s *ProbeStream) FrameRate() float64 {
	return frameRateFloat(s.AvgFrameRate)
}

func frameRateFloat(s string) float64 {
	parts := strings.Split(s, "/")
	if len(parts) == 2 {
		a, e1 := strconv.ParseFloat(parts[0], 64)
		b, e2 := strconv.ParseFloat(parts[1], 64)
		if e1 == nil && e2 == nil && b > 0 {
			return a / b
		}
	}
	f, _ := strconv.ParseFloat(s, 64)
	return f
}
