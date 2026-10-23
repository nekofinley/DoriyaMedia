package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"doriyamedia/internal/media"
	"doriyamedia/internal/models"
	"doriyamedia/internal/netproxy"
	"doriyamedia/internal/scan"
	"doriyamedia/internal/source"
)

// proxyClient streams media through the server (no global timeout).
var proxyClient = &http.Client{
	Timeout: 0,
	Transport: &http.Transport{
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	},
}

const sigTTL = 12 * time.Hour

// resumeMinSeconds is how far into a file a saved position must be before it is
// worth starting a packaging session there. It scales down for clips shorter
// than a couple of minutes (see playFile), where any real position is already
// past this.
const resumeMinSeconds = 30.0

// ---------- /api/play/{id} ----------

func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request, user *models.User) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	file, err := s.Store.FileByID(id)
	if err != nil {
		fail(w, 404, "文件不存在")
		return
	}
	s.playFile(w, r, file)
}

// playFile runs the decision pipeline for a media file and writes the response.
// Shared by the library player (/api/play) and the file browser (/api/browseplay).
func (s *Server) playFile(w http.ResponseWriter, r *http.Request, file *models.MediaFile) {
	drv, err := s.Engine.DriverFor(file)
	if err != nil {
		fail(w, 500, "存储源错误: "+err.Error())
		return
	}
	// 清晰度 is a playback-time choice, not a library setting: the player sends
	// the rung it wants with each decision (and with every HLS request that
	// follows), and a driver without a ladder simply ignores it. The choice has
	// to reach the source before the probe, because the probe for a multi-stream
	// source *is* its resolved ladder — resolving the wrong rung first would
	// decide the picture's codec and size from another quality's streams.
	qualityPref := qualityParam(r)
	source.SetQuality(drv, qualityPref)

	// The decision needs a probe. A source that publishes its media as several
	// streams (bilibili DASH) states its own codecs, so that probe is built from
	// what the source says; an ordinary file uses the stored probe and, when
	// there is none, answers immediately with state=probing while a background
	// job reads it (probing a cloud-drive file is slow — ffprobe must pull a
	// window of it).
	ctx, cancel := context.WithTimeout(r.Context(), 150*time.Second)
	defer cancel()

	probe, perr := s.Engine.ProbeFor(ctx, file, drv)
	if perr != nil {
		// The source itself refused to hand out a playable stream (a membership
		// episode, a region lock). There is nothing to probe and nothing to play,
		// so the reason is the answer rather than a spinner.
		fail(w, 502, perr.Error())
		return
	}
	if probe == nil {
		s.Engine.StartProbe(file, drv)
		writeJSON(w, 200, s.playPreparingPayload(r, file))
		return
	}
	_, multiPart := drv.(source.MultiPart)

	// The ladder and the rung this stream actually uses. The latter is not the
	// preference when the requested rung is missing from this episode (a stored
	// 4K choice on a 720P title) or when the codec preference moves the picture
	// down a rung, so it is reported separately and the player shows the truth.
	qualities := make([]source.Quality, 0, 8)
	quality := source.SelectedQualityOf(drv)
	if qd, ok := drv.(source.QualityDriver); ok {
		if qs, err := qd.Qualities(ctx, file.Path); err == nil {
			qualities = qs
		}
		quality = qd.SelectedQuality()
	}

	client := s.clientKind(r)
	force := r.URL.Query().Get("force")
	if force == "" {
		force = s.Cfg.Playback.ForceMode
	}

	hasDirect := false
	var directHeaders map[string]string
	if drv.HasDirect() {
		if _, hdrs, err := drv.DirectURL(ctx, file.Path); err == nil {
			hasDirect = true
			directHeaders = hdrs
		}
	}

	dec := media.Decide(probe, file.Path, client, media.DecideOptions{
		Force:         force,
		HasDirect:     hasDirect,
		DirectHeaders: directHeaders,
		// Copying HEVC is only safe for Apple when the result is fMP4, which the
		// session packager produces; without it, Apple gets MPEG-TS it cannot
		// decode (audio, no picture) — so the packager implies the copy is allowed.
		AppleHEVCCopy: s.Cfg.Playback.AppleHEVCCopy || s.Cfg.Playback.HLSSessionPackager,
		// A browser says whether its MediaSource handles HEVC. Honouring that lets
		// a machine with hardware support copy instead of transcoding — but only
		// with the packager on, since the copy has to be fMP4 to be playable.
		ClientHEVC: capsHas(r.URL.Query().Get("caps"), "hevc") && s.Cfg.Playback.HLSSessionPackager,
		// The same report, negative: this browser looked and cannot play HEVC, so
		// neither direct play nor copying is on offer for it.
		ClientNoHEVC: capsHas(r.URL.Query().Get("caps"), "nohevc"),
		// The client's own audio report, if it made one: a codec the machine says
		// it can decode is allowed to direct-play even where the server's tables
		// guessed otherwise. Absent tokens keep the tables' verdict.
		ClientAudio: reportedAudio(r.URL.Query().Get("caps")),
		// The source hands out the picture and the sound separately, so the only
		// mode that can work is a server-side mux.
		MultiPart: multiPart,
	})

	// Resume support: tell the HLS packager where the viewer left off, so a
	// session does not have to package everything ahead of that point. Only HLS
	// URLs carry it: the 302/proxy paths stream from the start and let the player
	// seek, and on-demand segments are addressed by absolute index anyway.
	resumeFrom := 0.0
	if u := s.currentUser(r); u != nil {
		if p, err := s.Store.ProgressForFile(u.ID, file.ID); err == nil && p != nil {
			dur := p.Duration
			if dur <= 0 {
				dur = dec.Duration
			}
			// Resuming from the opening moments is not worth a session boundary,
			// so a flat minimum applies — but it scales down for short clips,
			// where any meaningful position is already past it. The final stretch
			// is an ending, not a place to resume.
			minPos := resumeMinSeconds
			if dur > 0 && dur*0.05 < minPos {
				minPos = dur * 0.05
			}
			if p.Position > minPos && (dur <= 0 || p.Position < dur*0.95) {
				resumeFrom = p.Position
			}
		}
	}
	startQ := ""
	if resumeFrom > 0 {
		startQ = "&start=" + strconv.FormatFloat(resumeFrom, 'f', 3, 64)
	}

	// A seek is not a resume. The guards above belong to "carry on where you left
	// off" — they refuse to resume from the opening seconds or into the final
	// stretch, because those are not places a viewer stopped at. A player asking to
	// start at a position means exactly that position, wherever it is, and it needs
	// the session boundary the server actually snapped to: packaged segments are
	// addressed by index, so a stream that begins at 601.5s when the player believes
	// it begins at 600s reports every timestamp 1.5s wrong.
	if v := r.URL.Query().Get("at"); v != "" {
		if at, err := strconv.ParseFloat(v, 64); err == nil && at > 0 && (dec.Duration <= 0 || at < dec.Duration) {
			resumeFrom = at
			startQ = "&start=" + strconv.FormatFloat(at, 'f', 3, 64)
		}
	}

	// subtitles: only convertible (text) tracks can be served as WebVTT, and the
	// one worth turning on automatically is picked here so every client agrees.
	// Resolved before the URL is built, because whether there are subtitles
	// decides whether a Safari client gets a master playlist or a media one.
	subs := s.discoverSubtitles(ctx, drv, file)
	servable := make([]models.Subtitle, 0, len(subs))
	for _, sub := range subs {
		if sub.Convertible {
			servable = append(servable, sub)
		}
	}
	defSubID := media.PickDefaultSubtitle(servable)

	type subDTO struct {
		ID       int64  `json:"id"`
		Label    string `json:"label"`
		Lang     string `json:"lang"`
		Embedded bool   `json:"embedded"`
		Default  bool   `json:"default"`
		// Burn marks a track that cannot be served as text and has to be overlaid
		// onto the video instead — the client offers it, knowing it re-encodes.
		Burn bool `json:"burn,omitempty"`
		// BurnAvailable marks a text track whose formatting WebVTT can only
		// approximate (ASS typesetting: signs placed to the pixel, karaoke,
		// animation). Overlaying it is the faithful rendering, so the client may
		// offer that as a choice — it is not a requirement, unlike Burn.
		BurnAvailable bool   `json:"burn_available,omitempty"`
		URL           string `json:"url,omitempty"`
	}
	subDTOs := make([]subDTO, 0, len(subs))
	for _, sub := range subs {
		if !sub.Convertible {
			// Only image tracks are worth offering this way: overlaying them is the
			// only way to show them at all.
			if burnIsBitmap(&sub) {
				subDTOs = append(subDTOs, subDTO{
					ID: sub.ID, Label: sub.Title, Lang: sub.Lang,
					Embedded: sub.Embedded, Burn: true,
				})
			}
			continue
		}
		// What the file actually holds, not what its name claimed: this is what
		// decides whether the faithful-rendering option makes sense.
		format, _ := s.subFormatFor(ctx, drv, &sub)
		subDTOs = append(subDTOs, subDTO{
			ID:       sub.ID,
			Label:    sub.Title,
			Lang:     sub.Lang,
			Embedded: sub.Embedded,
			Default:  sub.ID == defSubID,
			// ASS/SSA is where typesetting lives, so those tracks can also be
			// burned in when a viewer wants the original layout.
			BurnAvailable: format == "ass" || format == "ssa",
			URL:           fmt.Sprintf("/api/subs/%d/%d.vtt%s", file.ID, sub.ID, signedQuery(s.Cfg.Secret, file.ID, sigTTL)),
		})
	}

	// A burn request wins over the decision: overlaying a subtitle means
	// re-encoding the video, whatever the client could otherwise have played.
	burnSub := int64(0)
	if v := r.URL.Query().Get("burn"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			burnSub = n
		}
	}
	if burnSub > 0 {
		dec.Mode = fmt.Sprintf("burn.%d", burnSub)
		dec.Kind = "hls"
		dec.Reason = "烧录字幕(图形字幕只能叠加,需要重新编码)"
		dec.URL = ""
	}

	if force == "byterange" {
		// Serving the file's own bytes as HLS. Not chosen automatically: see §17 —
		// remuxing and the proxy already give zero-transcode playback, and both are
		// cheaper than indexing every fragment.
		dec.Mode, dec.Kind = "byterange", "hls"
		dec.Reason = "字节范围直取(零转码,不写磁盘)"
	}

	// A session begins at a segment boundary, and the player presents that stream
	// as starting at zero. So the client is told where the stream begins in file
	// time, or it would save progress measured from the resume point and line
	// subtitles up against the wrong clock.
	// Whether a stream can be asked to begin at an arbitrary position.
	//
	// It cannot for a two-stream source: its parts seek independently, so every
	// session starts at the beginning and the player can only reach what has been
	// packaged so far. A player that does not know this will drag its bar into
	// fragments that do not exist yet and appear to do nothing at all.
	streamStart := s.streamStartFor(dec.Mode, resumeFrom, multiPart)
	// Whether a stream can be asked to begin at an arbitrary position.
	//
	// It cannot for a two-stream source: its parts seek independently, so every
	// session starts at the beginning and the player can only reach what has been
	// packaged so far. A player that does not know this drags its bar into fragments
	// that do not exist yet and appears to do nothing at all.
	canStartAt := dec.Kind == "hls" && !multiPart

	// Safari manages HLS subtitles itself and ignores <track> on a natively
	// played stream, so it is handed a master playlist carrying the subtitles as
	// renditions. Other players keep the <track> route, which the page controls.
	// (A burned-in stream is deliberately excluded: its subtitles are already in
	// the picture, and renditions as well would draw them twice.)
	nativeSubs := client == media.ClientSafari && len(subDTOs) > 0 && dec.Mode == "remux"
	playlist := "index.m3u8"
	if nativeSubs {
		playlist = "master.m3u8"
	}

	// 弹幕: a source that publishes bullet comments hands the player a URL to
	// fetch them from. Nothing is read here — an episode's comments are a few
	// thousand entries, and the player only needs them once it is playing.
	danmakuURL := ""
	if _, ok := drv.(source.Danmaku); ok {
		danmakuURL = fmt.Sprintf("/api/danmaku/%d/comments.json%s", file.ID, signedQuery(s.Cfg.Secret, file.ID, sigTTL))
	}

	// The HLS stream is pinned to the rung this decision actually chose, not to
	// the preference: "auto" has to become a concrete stream for the session key,
	// and a segment request that re-resolved "auto" could otherwise land on a
	// different rung than the playlist it belongs to. When the ladder could not be
	// read (the menu call failed), the preference is the next best identity: it
	// still separates two explicit choices, and a missing rung falls back inside
	// the source exactly as it did for this decision.
	pinQuality := quality
	if pinQuality <= 0 {
		pinQuality = qualityPref
	}
	qualityQ := ""
	if pinQuality > 0 {
		qualityQ = "&q=" + strconv.Itoa(pinQuality)
	}
	// The packaging session this decision will use, named the same way
	// serveSessionHLS names it. A player that switches quality can then tell the
	// abandoned rung's progress from the new one's: both sessions can be alive at
	// once, and the abandoned one may already be minutes ahead.
	streamVariant := ""
	if multiPart {
		if v := probe.Video(); v != nil {
			streamVariant = media.StreamVariant(v.CodecName, v.Width, v.Height, pinQuality)
		}
	}

	switch dec.Mode {
	case "direct":
		dec.URL = fmt.Sprintf("/api/stream/%d%s", file.ID, signedQuery(s.Cfg.Secret, file.ID, sigTTL))
		dec.MimeType = "video/mp4"
	case "proxy":
		dec.URL = fmt.Sprintf("/api/stream/%d?proxy=1", file.ID) +
			strings.Replace(signedQuery(s.Cfg.Secret, file.ID, sigTTL), "?", "&", 1)
		dec.MimeType = "video/mp4"
	case "unsupported":
		// Nothing to hand the player. The reason travels with the decision, and the
		// player shows it instead of attaching a source it cannot decode.
		dec.URL = ""
		dec.MimeType = ""
	default: // remux → HLS
		sq := signedQuery(s.Cfg.Secret, file.ID, sigTTL)
		if force == "byterange" {
			dec.URL = fmt.Sprintf("/api/hls/%d/byterange.m3u8%s", file.ID, sq)
			dec.MimeType = "application/vnd.apple.mpegurl"
			break
		}
		burnQ := ""
		if burnSub > 0 {
			burnQ = "&burn=" + strconv.FormatInt(burnSub, 10)
		}
		dec.URL = fmt.Sprintf("/api/hls/%d/%s%s&m=%s%s%s%s", file.ID, playlist, sq, dec.Mode, startQ, burnQ, qualityQ)
		dec.MimeType = "application/vnd.apple.mpegurl"
	}

	// Fallback strategies for the player to auto-try when the primary mode fails
	// on this client. The decision already worked out *which* strategies can work
	// for this client and in what order; here each one only becomes a signed URL.
	sq := signedQuery(s.Cfg.Secret, file.ID, sigTTL)
	proxyURL := fmt.Sprintf("/api/stream/%d?proxy=1", file.ID) + strings.Replace(sq, "?", "&", 1)
	directURL := fmt.Sprintf("/api/stream/%d%s", file.ID, sq)
	hlsURL := func(m string) string {
		return fmt.Sprintf("/api/hls/%d/%s%s&m=%s%s%s", file.ID, playlist, sq, m, startQ, qualityQ)
	}
	alts := make([]media.DecisionAlt, 0, len(dec.Alternates))
	for _, a := range dec.Alternates {
		if dec.Duration <= 0 && a.Mode == "remux" {
			// HLS needs a duration to build a playlist; without one there is no
			// stream to fall back to.
			continue
		}
		switch a.Mode {
		case "direct":
			a.URL, a.MimeType = directURL, "video/mp4"
		case "proxy":
			a.URL, a.MimeType = proxyURL, "video/mp4"
		case "remux":
			a.URL, a.MimeType = hlsURL(a.Mode), "application/vnd.apple.mpegurl"
		default:
			continue
		}
		// A URL the client already has is not a fallback.
		if a.URL == dec.URL {
			continue
		}
		alts = append(alts, a)
	}
	dec.Alternates = alts

	// audio tracks
	auds := probe.AudioStreams()
	selOrdinal := 0
	if a := r.URL.Query().Get("a"); a != "" {
		if n, err := strconv.Atoi(a); err == nil && n >= 0 && n < len(auds) {
			selOrdinal = n
		}
	}
	tracks := make([]audioInfo, 0, len(auds))
	for i, a := range auds {
		tracks = append(tracks, audioInfo{
			Ordinal: i, Index: a.Index, Codec: a.CodecName, Lang: a.Language(),
			Title: a.Title(), Channels: a.Channels, Selected: i == selOrdinal,
		})
	}

	// item context
	title := s.playTitle(file)
	srcName := ""
	if src, err := s.Store.SourceByID(file.SourceID); err == nil {
		srcName = src.Name
	}

	writeJSON(w, 200, map[string]any{
		"state":        "ready",
		"file_id":      file.ID,
		"item_id":      file.ItemID,
		"title":        title,
		"source":       srcName,
		"client":       client,
		"mode":         dec.Mode,
		"kind":         dec.Kind,
		"url":          dec.URL,
		"mime_type":    dec.MimeType,
		"reason":       dec.Reason,
		"duration":     dec.Duration,
		"video_codec":  dec.VideoCodec,
		"audio_codec":  dec.AudioCodec,
		"resolution":   dec.Resolution,
		"alternates":   dec.Alternates,
		"media":        probeSummary(file.Probe),
		"audio_tracks": tracks,
		"versions":     s.versionsFor(file.ItemID, file.ID),
		"subtitles":    subDTOs,
		// The subtitle a player should enable by itself (0 = none). Sent both as
		// a flag per track and once here, so a client can pick either way.
		"default_subtitle_id": defSubID,
		// stream_start is the file position an HLS session begins at (0 when the
		// stream starts at the beginning). Players present a session stream as
		// starting at zero — hls.js re-aligns it, discarding the absolute
		// timestamps the segments carry — so the client needs this offset to seek,
		// to save progress in file time, and to line up absolutely timed subtitles.
		"stream_start": streamStart,
		// 清晰度: the rungs this file can be played at, the rung this stream uses
		// (0 when its source has no ladder), and the preference the caller asked
		// for (0 = automatic). An empty qualities list means the player offers
		// no menu at all — an ordinary file has one picture, not a choice.
		"qualities":    qualities,
		"quality":      quality,
		"quality_pref": qualityPref,
		// stream_variant names the packaging job this URL belongs to (empty for a
		// source with one stream). The status endpoint names its job the same way,
		// so a player that just changed quality can ignore the old job's progress.
		"stream_variant": streamVariant,
		// Whether the player may ask for a stream beginning at some other position
		// (see the seek handling in the web player): without it, the honest answer
		// to a drag past the packaged window is to go as far as it can and say so.
		"can_start_at": canStartAt,
		// native_subtitles says the stream carries its subtitles as HLS
		// renditions, so the player manages them (its own menu, its own default)
		// and the page must not also attach <track> elements.
		"native_subtitles": nativeSubs,
		// danmaku_url is where this file's bullet comments can be fetched, when
		// its source has any. Empty means the player shows no danmaku control.
		"danmaku_url": danmakuURL,
	})
}

// streamStartFor reports where an HLS stream will begin, in file time: the saved
// position snapped down to a segment boundary, or 0 when the stream starts at the
// file's beginning. It is 0 whenever packaging will not start mid-file — with the
// session packager off, the on-demand playlist covers the whole file from zero and
// the player's clock is already file time. The client (progress and seeks) and the
// subtitle renditions both depend on this agreeing with what the packager does.
//
// multiPart forces 0 for the same reason as a disabled packager: a source whose
// media is several streams is served segment by segment from the file's start,
// however far in the viewer left off, so its clock is file time as well.
func (s *Server) streamStartFor(mode string, startSec float64, multiPart bool) float64 {
	if startSec <= 0 || !s.Cfg.Playback.HLSSessionPackager || multiPart {
		return 0
	}
	if mode != "remux" && !strings.HasPrefix(mode, "burn.") {
		return 0
	}
	segLen := s.Cfg.Playback.SegmentSeconds
	if segLen <= 0 {
		segLen = 4
	}
	return math.Floor(startSec/segLen) * segLen
}

// playTitle resolves the display title for a file ("剧名 · S01E02 · 标题").
func (s *Server) playTitle(file *models.MediaFile) string {
	item, _ := s.Store.ItemByID(file.ItemID)
	if item == nil {
		return ""
	}
	title := item.Name
	if item.Type == "episode" {
		if season, err := s.Store.ItemByID(deref(item.ParentID)); err == nil && season.ParentID != nil {
			if show, err := s.Store.ItemByID(*season.ParentID); err == nil {
				title = fmt.Sprintf("%s · S%02dE%02d · %s", show.Name, item.Season, item.Episode, item.Name)
			}
		}
	}
	return title
}

// playPreparingPayload is the answer while the first probe of a file runs.
// It carries only what the player needs to render its "preparing" state —
// no URL, because the mode cannot be decided yet.
func (s *Server) playPreparingPayload(r *http.Request, file *models.MediaFile) map[string]any {
	srcName := ""
	if src, err := s.Store.SourceByID(file.SourceID); err == nil {
		srcName = src.Name
	}
	return map[string]any{
		"state":        "probing",
		"file_id":      file.ID,
		"item_id":      file.ItemID,
		"title":        s.playTitle(file),
		"source":       srcName,
		"client":       s.clientKind(r),
		"mode":         "",
		"kind":         "",
		"url":          "",
		"mime_type":    "",
		"reason":       "正在探测媒体信息(容器 / 编码 / 音轨)…",
		"duration":     0,
		"alternates":   []any{},
		"audio_tracks": []any{},
		"subtitles":    []any{},
		"versions":     s.versionsFor(file.ItemID, file.ID),
		// No ladder yet: the probe has not read one, and a source without one
		// never has any. Stated rather than omitted so the player's fields keep
		// their shapes while it polls.
		"qualities":      []any{},
		"quality":        0,
		"quality_pref":   qualityParam(r),
		"stream_variant": "",
	}
}

// ---------- /api/play/{id}/status ----------

// handlePlayStatus reports the probe state for a file so the player can show
// real progress instead of an indistinguishable spinner.
// streamStatusFor reports what is being produced for a file, preferring the
// packaging session's own output.
//
// A packaged playback answers one long-lived ffmpeg, so the per-request counters
// say nothing about it — and a player told nothing applies its own timeout and
// gives up on a stream that is merely slow to start, which is the normal case for
// the first seconds of a cold 4K file on a cloud drive.
//
// variant limits the answer to one source stream. A player that has just switched
// rung must not be told about the old rung's progress (which can be minutes
// ahead), and a caller that asks for a stream the server has not started yet gets
// no stream at all rather than an unrelated counter.
func (s *Server) streamStatusFor(fileID int64, variant string) *media.StreamStatus {
	if p := s.Engine.PackagerProgressVariant(fileID, variant); p != nil {
		return p
	}
	if variant != "" {
		return nil
	}
	return s.Engine.StreamStatus(fileID)
}

func (s *Server) handlePlayStatus(w http.ResponseWriter, r *http.Request, user *models.User) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	file, err := s.Store.FileByID(id)
	if err != nil {
		fail(w, 404, "文件不存在")
		return
	}
	st := s.Engine.ProbeStatus(id)
	if media.ProbeFromCache(file) != nil {
		st = media.ProbeStatus{State: "ready"}
	}
	// stream carries segment-generation progress once playback starts, so the
	// player can show how much is ready and whether the server keeps up.
	writeJSON(w, 200, map[string]any{"file_id": id,
		"state":   st.State, // idle | probing | ready | failed
		"elapsed": st.Elapsed,
		"error":   st.Error,
		"stream":  s.streamStatusFor(id, r.URL.Query().Get("variant")),
	})
}

func (s *Server) clientKind(r *http.Request) media.ClientKind {
	switch strings.ToLower(r.URL.Query().Get("client")) {
	case "ios", "safari":
		return media.ClientSafari
	case "chrome", "desktop", "android", "edge":
		return media.ClientChrome
	case "firefox":
		return media.ClientFirefox
	default:
		return media.DetectClient(r.UserAgent())
	}
}

// qualityParam is the 清晰度 a playback request asks for, in the source's own
// numbering (bilibili's qn). 0 or anything unreadable means automatic: the best
// rung the source will give. A source without a ladder ignores it.
func qualityParam(r *http.Request) int {
	v := r.URL.Query().Get("q")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// ---------- subtitle discovery ----------

func (s *Server) discoverSubtitles(ctx context.Context, drv source.Driver, file *models.MediaFile) []models.Subtitle {
	if !strings.HasPrefix(file.Path, "http://") && !strings.HasPrefix(file.Path, "https://") {
		if dir, base := splitDirBase(file.Path); dir != "" && base != "" {
			if entries, err := drv.List(ctx, dir); err == nil {
				// Where a subtitle sits is not who it belongs to: one directory
				// holds a whole season, and a release may name its subtitles
				// nothing like its video (see scan.SubtitleAttachment). Every
				// candidate in the directory is offered, and the attachment rule
				// decides — the format check only says whether it is a subtitle
				// at all.
				names := make([]string, len(entries))
				for i, e := range entries {
					if e.IsDir {
						continue
					}
					if _, ok := subFormatOf(e.Name); ok {
						names[i] = e.Name
					}
				}
				attached := scan.SubtitleAttachmentIn(base, names)
				for i, e := range entries {
					if !attached[i] {
						continue
					}
					format, conv := subFormatOf(e.Name)
					_ = s.Store.UpsertSubtitle(&models.Subtitle{
						FileID: file.ID, Path: e.Path,
						Lang: media.GuessLang(e.Name), Title: media.SubLabel(e.Name),
						Format: format, Convertible: conv,
					})
				}
			}
		}
	}
	// ensure embedded subs registered
	if file.Probe != "" {
		var p media.Probe
		if err := jsonUnmarshal(file.Probe, &p); err == nil {
			s.Engine.RegisterEmbeddedSubs(file, &p)
		}
	}
	subs, _ := s.Store.SubtitlesByFile(file.ID)
	return subs
}

func splitDirBase(p string) (string, string) {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}

// subFormatOf reports whether a listed file is a subtitle at all, and what it
// most likely holds.
//
// The extension decides the format when there is one. Releases that name their
// Chinese tracks "…aac].Chs" / "….Cht" leave the file with no extension at all,
// the language tag standing where the extension would be; the tag does not say
// what the file is written in, so the format it registers is corrected from the
// file's own bytes when the subtitle is served (subFormatFor).
func subFormatOf(name string) (format string, convertible bool) {
	if format, conv := media.DetectSubFormat(name); format != "" {
		return format, conv
	}
	return media.DetectTaggedSubFormat(name)
}

func stripExt(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[:i]
	}
	return name
}

// subFormatCache remembers what a subtitle file turned out to be, keyed by
// subtitle id, so sniffing its bytes happens once per track rather than on every
// cue request. Subtitle files are static, so the only way an entry goes stale is
// a rescan replacing the file behind the same row, which is not worth a TTL.
var subFormatCache sync.Map // int64 -> string

// subFormatFor returns the format a subtitle should actually be converted as.
//
// The format recorded at discovery came from the file name, and a name is only a
// claim: an ASS file shipped as "….Chs" has no extension to be read, and any
// release can put the wrong one on. So for external text tracks the first bytes
// get the final say, which is also what keeps the ASS converter (positioning,
// colours, fonts) from being handed SubRip text or the other way round — the two
// disagree about what a line means, and guessing wrong garbles the subtitles
// rather than degrading them.
func (s *Server) subFormatFor(ctx context.Context, drv source.Driver, sub *models.Subtitle) (string, bool) {
	if sub.Embedded {
		return sub.Format, sub.Convertible
	}
	if v, ok := subFormatCache.Load(sub.ID); ok {
		return v.(string), true
	}
	rc, err := drv.Open(ctx, sub.Path)
	if err != nil {
		return sub.Format, sub.Convertible
	}
	defer rc.Close()
	head, err := io.ReadAll(io.LimitReader(rc, 8<<10))
	if err != nil && len(head) == 0 {
		return sub.Format, sub.Convertible
	}
	format, _ := media.SniffTextSubFormat(head)
	subFormatCache.Store(sub.ID, format)
	return format, true
}

// ---------- /api/stream/{id} ----------

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	file, err := s.Store.FileByID(id)
	if err != nil {
		fail(w, 404, "文件不存在")
		return
	}
	drv, err := s.Engine.DriverFor(file)
	if err != nil {
		fail(w, 500, "存储源错误: "+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	proxy := r.URL.Query().Get("proxy") == "1"
	if !proxy && drv.HasDirect() {
		raw, hdrs, err := drv.DirectURL(ctx, file.Path)
		if err == nil && raw != "" && len(hdrs) == 0 {
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, raw, http.StatusFound)
			return
		}
		if err != nil {
			log.Printf("direct url failed for file %d: %v, falling back to proxy", file.ID, err)
		}
	}

	// local files: let net/http serve with full Range support
	if ld, ok := drv.(*source.LocalDriver); ok {
		ld.Serve(w, r, file.Path)
		return
	}

	target, headers, err := drv.ProbeURL(ctx, file.Path)
	if err != nil {
		fail(w, 502, "获取源地址失败: "+err.Error())
		return
	}
	proxyRemoteVia(w, r, target, headers, sourceHTTPClient(drv))
}

// sourceHTTPClient is the client to fetch a source's bytes with: the shared
// streaming client, unless the source reaches the network through a proxy of
// its own (a bilibili source abroad), in which case its bytes must leave the
// same way its API calls do.
func sourceHTTPClient(drv source.Driver) *http.Client {
	pd, ok := drv.(source.ProxiedDriver)
	if !ok || pd.ProxySpec() == "" {
		return proxyClient
	}
	spec, err := netproxy.Parse(pd.ProxySpec())
	if err != nil {
		return proxyClient
	}
	c, err := netproxy.NewClient(spec, 0)
	if err != nil {
		return proxyClient
	}
	return c
}

// proxyRemote and the range handling live in proxy_range.go.

// ---------- /api/hls/{id}/... ----------

func (s *Server) handleHLS(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	file, err := s.Store.FileByID(id)
	if err != nil {
		fail(w, 404, "文件不存在")
		return
	}
	drv, err := s.Engine.DriverFor(file)
	if err != nil {
		fail(w, 500, "存储源错误: "+err.Error())
		return
	}
	// The playlist, its init segment and every media segment carry the rung they
	// belong to (see playFile): re-resolving the same file at "automatic" for a
	// segment request could choose a different picture than the playlist's.
	source.SetQuality(drv, qualityParam(r))
	ctx, cancel := context.WithTimeout(r.Context(), 150*time.Second)
	defer cancel()

	rest := r.PathValue("rest")
	q := r.URL.Query()
	segLen := s.Cfg.Playback.SegmentSeconds

	// Where to begin packaging. /api/play passes the viewer's saved position, so
	// "continue watching" does not make the packager work through everything
	// before it first.
	startSec := 0.0
	if v := q.Get("start"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			startSec = f
		}
	}

	// Byte-range serving of an already-fragmented MP4: the playlist is built from
	// the file's own box structure, so nothing is packaged or written to disk.
	if rest == "byterange.m3u8" {
		s.serveByteRangeHLS(w, r, ctx, file, drv)
		return
	}

	// The subtitle side of HLS: a master playlist offering each servable subtitle
	// as a rendition, and the one-segment playlist for each one. Handled before
	// the packaging split, because it is the same either way.
	if rest == "master.m3u8" || strings.HasPrefix(rest, "subs/") {
		s.serveHLSSubtitles(w, r, ctx, file, drv, q.Get("m"), rest, q, startSec)
		return
	}

	// burn asks for a subtitle to be overlaid onto the video. That necessarily
	// re-encodes, and it is the only way to show a bitmap track (PGS/VobSub) at
	// all — so it takes precedence over the packaging mode.
	burnSubID := int64(0)
	if v := q.Get("burn"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			burnSubID = n
		}
	}
	if burnSubID > 0 {
		s.serveBurnHLS(w, r, ctx, file, drv, rest, q, segLen, startSec, burnSubID)
		return
	}

	// With the session packager on, every HLS playback runs through one long-lived
	// ffmpeg: it reads the source once instead of re-seeking and re-reading a
	// probe window for each and every segment, and it is also what produces the
	// fMP4 that copied HEVC for Apple requires (Apple does not decode HEVC from
	// MPEG-TS). With the flag off, everything keeps using the on-demand MPEG-TS
	// path, which needs no temporary disk. See media/session.go.
	//
	// A source whose media is several streams (bilibili DASH) always takes the
	// session path, whatever the flag says. The on-demand path opens the source
	// once per segment, and the parts of such a source seek independently: asking
	// for a later segment lands the picture and the sound in different places,
	// which a player reports as "Parsed buffers not in DTS sequence" and refuses
	// to play. One session reads the episode once and writes contiguous segments.
	_, multiPart := drv.(source.MultiPart)
	if s.Cfg.Playback.HLSSessionPackager || multiPart {
		mode := q.Get("m")
		if multiPart {
			mode = "remux" // the only mode a two-stream source can take
		}
		// Only remux: a video re-encode is no longer offered, so "m=transcode"
		// from a stale page falls through to the on-demand remux path below.
		if mode == "remux" {
			if probe, err := s.Engine.EnsureProbe(ctx, file, drv); err == nil {
				s.serveSessionHLS(w, r, ctx, file, drv, probe,
					sessionRequest{keyMode: mode, videoMode: mode}, rest, q, segLen, startSec)
				return
			}
		}
	}

	if rest == "" || strings.HasPrefix(rest, "index") || strings.HasPrefix(rest, "playlist") {
		probe, err := s.Engine.EnsureProbe(ctx, file, drv)
		if err != nil {
			fail(w, 500, "媒体探测失败: "+err.Error())
			return
		}
		duration := probe.Duration()
		if duration <= 0 {
			fail(w, 500, "无法确定媒体时长,无法生成 HLS")
			return
		}
		mode := "remux"
		sig := signedQuery(s.Cfg.Secret, file.ID, sigTTL) + "&m=" + mode
		if a := q.Get("a"); a != "" {
			sig += "&a=" + a
		}
		playlist := s.Engine.Playlist(fmt.Sprintf("/api/hls/%d", file.ID), sig, duration, segLen)
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(playlist))
		return
	}

	segIndex, err := parseSegID(strings.TrimSuffix(rest, ".ts"))
	if err != nil {
		fail(w, 404, "未知 HLS 资源")
		return
	}
	probe, err := s.Engine.EnsureProbe(ctx, file, drv)
	if err != nil {
		fail(w, 500, "媒体探测失败: "+err.Error())
		return
	}

	mode := "remux"
	audioOrdinal := 0
	if a := q.Get("a"); a != "" {
		if n, e := strconv.Atoi(a); e == nil {
			audioOrdinal = n
		}
	}
	auds := probe.AudioStreams()
	if audioOrdinal >= len(auds) {
		audioOrdinal = -1
	}
	audioMode := "copy"
	if audioOrdinal >= 0 {
		ac := auds[audioOrdinal].CodecName
		if mode == "transcode" || (ac != "aac" && ac != "mp3") {
			audioMode = "aac"
		}
	}

	videoIndex := -1
	if v := probe.Video(); v != nil {
		videoIndex = v.Index
	}
	start := float64(segIndex) * segLen
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-store")
	written, err := s.Engine.Segment(ctx, file, drv, start, segLen, mode, audioMode, audioOrdinal, videoIndex, s.Cfg.Playback.TranscodeHeight, w)
	if err != nil {
		log.Printf("hls segment %d/%d @%.0fs failed: %v", file.ID, segIndex, start, err)
		// Nothing reached the client yet, so the status is still ours to choose.
		// A real error makes the player retry the segment; answering 200 with an
		// empty body looks like a corrupt segment and can stop playback for good.
		if written == 0 {
			w.Header().Set("Retry-After", "2")
			http.Error(w, "分片生成失败,请重试", http.StatusBadGateway)
		}
	}
}

// ---------- /api/hls/{id}/... served from a packaging session ----------

// serveSessionHLS answers HLS requests from a long-lived packager session: the
// media playlist (rewritten to point at our signed endpoints), the fMP4 init
// segment, and the media segments. A resource the packager has not produced yet
// is waited for rather than 404'd — the player would otherwise treat a segment
// that is merely early as fatal.
// sessionRequest is what a caller wants packaged: the mode that identifies the
// session, how the video is handled, and optionally a subtitle to burn in.
type sessionRequest struct {
	keyMode   string // the session's identity, e.g. "remux" or "burn.12"
	videoMode string // what the packager does: remux (copy) or transcode
	burn      *media.BurnRequest
	burnID    int64 // the subtitle being burned, for the signed URL
	prepare   func(dir string) error
}

// serveByteRangeHLS serves an already-fragmented MP4 as HLS by pointing the player
// at byte ranges of the original file: no transcoding, no remuxing, and nothing
// written to disk. The player issues Range requests against the ordinary stream
// endpoint, which is what the range handling in proxy_range.go exists for.
//
// Indexing needs random access, so this is offered only when the source hands back
// a seekable file. A cloud source would need a header read per fragment — thousands
// of round trips for a whole film — so it keeps using the proxy or remux paths
// instead. That trade-off is deliberate; see docs §17.
func (s *Server) serveByteRangeHLS(w http.ResponseWriter, r *http.Request, ctx context.Context,
	file *models.MediaFile, drv source.Driver) {

	rc, err := drv.Open(ctx, file.Path)
	if err != nil {
		fail(w, 502, "读取源失败: "+err.Error())
		return
	}
	defer rc.Close()
	ra, seekable := rc.(io.ReaderAt)
	seeker, canSeek := rc.(io.Seeker)
	if !seekable || !canSeek {
		fail(w, 409, "字节范围直取需要可随机读取的源(当前源只能顺序读取)")
		return
	}
	size, err := seeker.Seek(0, io.SeekEnd)
	if err != nil {
		fail(w, 500, "读取源大小失败: "+err.Error())
		return
	}
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		fail(w, 500, "读取源失败: "+err.Error())
		return
	}
	idx, err := media.ParseFragmentedMP4(ra, size)
	if err != nil {
		fail(w, 409, "该文件不能按字节范围直取: "+err.Error())
		return
	}

	// The segment URI is the file itself, so the player's own Range requests fetch
	// exactly the bytes the playlist declares.
	sq := signedQuery(s.Cfg.Secret, file.ID, sigTTL)
	uri := fmt.Sprintf("/api/stream/%d%s", file.ID, sq)
	if _, isLocal := drv.(*source.LocalDriver); !isLocal {
		uri = fmt.Sprintf("/api/stream/%d?proxy=1", file.ID) + strings.Replace(sq, "?", "&", 1)
	}
	pl, err := idx.ByteRangePlaylist(uri)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(pl)
}

// serveBurnHLS packages a stream with a subtitle overlaid onto the video.
//
// A bitmap track (PGS/VobSub) cannot be converted to text, so overlaying it is
// the only way to show it at all; text tracks are burned too when asked, which
// keeps ASS typesetting that a WebVTT conversion would flatten. Either way the
// video is re-encoded, so this is opt-in per playback. Everything else — the
// long-lived packager, fMP4 for Apple, the absolute timeline, the cache — is the
// ordinary session machinery.
func (s *Server) serveBurnHLS(w http.ResponseWriter, r *http.Request, ctx context.Context,
	file *models.MediaFile, drv source.Driver, rest string, q url.Values, segLen, startSec float64, subID int64) {

	sub, err := s.Store.SubtitleByID(subID)
	if err != nil || sub.FileID != file.ID {
		fail(w, 404, "字幕不存在")
		return
	}
	probe, err := s.Engine.EnsureProbe(ctx, file, drv)
	if err != nil {
		fail(w, 500, "媒体探测失败: "+err.Error())
		return
	}

	burn := &media.BurnRequest{StreamIndex: sub.StreamIdx}
	var prepare func(dir string) error
	if !burnIsBitmap(sub) {
		data, ext, err := s.burnSubtitleFile(ctx, file, drv, sub)
		if err != nil {
			fail(w, 502, "字幕读取失败: "+err.Error())
			return
		}
		// The child runs with the session directory as its working directory, so a
		// bare file name needs no escaping — on Windows the alternative means
		// escaping drive colons and backslashes inside a filter argument.
		burn = &media.BurnRequest{StreamIndex: -1, File: "burn." + ext}
		prepare = func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "burn."+ext), data, 0o644)
		}
	}

	s.serveSessionHLS(w, r, ctx, file, drv, probe, sessionRequest{
		keyMode:   fmt.Sprintf("burn.%d", subID),
		videoMode: "transcode",
		burn:      burn,
		burnID:    subID,
		prepare:   prepare,
	}, rest, q, segLen, startSec)
}

// burnIsBitmap reports whether a subtitle track is images, which can only be
// overlaid and never converted to text.
func burnIsBitmap(sub *models.Subtitle) bool {
	switch strings.ToLower(sub.Format) {
	case "hdmv_pgs_subtitle", "pgs", "dvd_subtitle", "dvdsub",
		"dvb_subtitle", "dvbsub", "xsub":
		return true
	}
	return false
}

// burnSubtitleFile fetches a text subtitle and returns it in a format the
// subtitles filter reads, plus the extension to name it.
func (s *Server) burnSubtitleFile(ctx context.Context, file *models.MediaFile, drv source.Driver, sub *models.Subtitle) ([]byte, string, error) {
	if sub.Embedded {
		data, err := s.Engine.ExtractEmbeddedSubtitle(ctx, file, drv, sub.StreamIdx)
		if err != nil {
			return nil, "", err
		}
		return data, "vtt", nil
	}
	rc, err := drv.Open(ctx, sub.Path)
	if err != nil {
		return nil, "", err
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, 8<<20))
	if err != nil {
		return nil, "", err
	}
	format, _ := s.subFormatFor(ctx, drv, sub)
	switch strings.ToLower(format) {
	case "subrip", "srt":
		return media.SRTToVTT(raw), "vtt", nil
	case "ass", "ssa":
		return raw, "ass", nil
	case "webvtt", "vtt":
		return raw, "vtt", nil
	}
	return media.SRTToVTT(raw), "vtt", nil
}

func (s *Server) serveSessionHLS(w http.ResponseWriter, r *http.Request, ctx context.Context,
	file *models.MediaFile, drv source.Driver, probe *media.Probe, req sessionRequest,
	rest string, q url.Values, segLen, startSec float64) {

	mode := req.keyMode
	audioOrdinal := 0
	if a := q.Get("a"); a != "" {
		if n, e := strconv.Atoi(a); e == nil {
			audioOrdinal = n
		}
	}
	auds := probe.AudioStreams()
	if audioOrdinal >= len(auds) {
		audioOrdinal = -1
	}
	videoIndex := -1
	videoCodec := ""
	if v := probe.Video(); v != nil {
		videoIndex = v.Index
		videoCodec = v.CodecName
	}

	// What ffmpeg will read: one URL for an ordinary file, one per stream for a
	// source that publishes the picture and the sound separately (bilibili DASH).
	// Re-resolved here so a link the source refused is replaced (see resolveTarget).
	inputs, err := s.Engine.SegmentInputs(ctx, file, drv, "")
	if err != nil {
		fail(w, 502, "获取源地址失败: "+err.Error())
		return
	}
	// A multi-stream source is packaged in one pass from the start. Its parts seek
	// independently — asking for 8s lands the picture and the sound in different
	// places — so there is no honest way to begin mid-file, and one pass over the
	// whole episode (at many times realtime) is cheaper than re-opening the source
	// for every segment anyway.
	multi := len(inputs) > 1

	// The rung these inputs were resolved at, and the one every request that
	// follows carries so the playlist and the segments behind it are the same
	// picture.
	//
	// This is the request's pinned rung (q), which playFile always writes, not
	// SelectedQuality: a warm parts cache answers without re-resolving and then
	// nothing records a selection on this driver. A request with no q is an
	// automatic stream and its packaging job is named q0, exactly as the play
	// decision named it.
	sessionQuality := source.QualityOf(drv)

	// Sessions are addressed by segment, so snap the requested position down to a
	// segment boundary: segment N then always means "N*segLen into the file", and
	// the playlist's MEDIA-SEQUENCE agrees with the timestamps we serve.
	if segLen <= 0 {
		segLen = 4
	}
	startSeg := 0
	sessionStart := 0.0
	if startSec > 0 && !multi {
		startSeg = int(startSec / segLen)
		sessionStart = float64(startSeg) * segLen
		if d := probe.Duration(); d > 0 && sessionStart >= d {
			// Past the end: nothing to resume, play from the start.
			startSeg, sessionStart = 0, 0
		}
	}

	audioMode := "copy"
	if audioOrdinal >= 0 {
		if ac := auds[audioOrdinal].CodecName; ac != "aac" && ac != "mp3" {
			audioMode = "aac"
		}
	}

	// The packager's command line is rebuilt from whichever inputs it is handed, so
	// recovering from a refused link is a matter of resolving again and asking for a
	// session with those.
	optsFor := func(in []media.Input) media.PackagerOptions {
		return media.PackagerOptions{
			FFmpeg:          s.Cfg.FFmpeg,
			Inputs:          in,
			VideoIndex:      videoIndex,
			AudioOrdinal:    audioOrdinal,
			VideoMode:       req.videoMode,
			AudioMode:       audioMode,
			VideoCodec:      videoCodec,
			TranscodeHeight: s.Cfg.Playback.TranscodeHeight,
			SegmentSeconds:  segLen,
			StartSeconds:    sessionStart,
			StartSegment:    startSeg,
			Burn:            req.burn,
			PrepareDir:      req.prepare,
		}
	}
	sig := signedQuery(s.Cfg.Secret, file.ID, sigTTL) + "&m=" + mode
	if a := q.Get("a"); a != "" {
		sig += "&a=" + a
	}
	if req.burn != nil {
		sig += "&burn=" + strconv.FormatInt(req.burnID, 10)
	}
	if sessionQuality > 0 {
		// Every playlist, init segment and media segment names the rung it was
		// packaged from. Without it, "automatic" would be re-resolved per request
		// and could choose another rung mid-stream.
		sig += "&q=" + strconv.Itoa(sessionQuality)
	}
	base := fmt.Sprintf("/api/hls/%d", file.ID)

	// Packaging output is identified by the stream it was made from as well as by
	// the file. A source that re-resolves its ladder on every play (bilibili) can
	// hand back a different rung the second time — 4K HDR one play, 1080p AVC the
	// next — and a session cached for the other rung is a different picture, not a
	// different encoding of the same one. Naming the rung in the key keeps the two
	// apart; an ordinary file has one stream and no variant.
	variant := ""
	if multi {
		if v := probe.Video(); v != nil {
			variant = media.StreamVariant(v.CodecName, v.Width, v.Height, sessionQuality)
		}
	}

	// Packaging can only start where a session is; which session answers a
	// request depends on where in the file the request asks for.
	height := s.Cfg.Playback.TranscodeHeight
	ensureAt := func(in []media.Input, startSeg int) (*media.Session, error) {
		o := optsFor(in)
		o.StartSegment = startSeg
		o.StartSeconds = float64(startSeg) * segLen
		if multi {
			// One session per file, always from the start — and keyed that way, or
			// the session would claim a start it does not have and every segment
			// would be shifted by the wrong amount.
			o.StartSegment, o.StartSeconds = 0, 0
		}
		return s.Engine.EnsureSession(ctx,
			media.SessionKeyVariant(file.ID, mode, audioOrdinal, videoIndex, height, o.StartSegment, variant), o)
	}
	// Pick the session that answers a playlist request. Two cases pull in opposite
	// directions: a refreshed playlist during playback must follow the session the
	// player is actually consuming (a seek moved it, and the URL still carries the
	// position this playback began at), while a playlist request that starts a
	// playback must honour its position — otherwise "continue watching" streams
	// from whatever session was left over.
	streamSession := func(in []media.Input) (*media.Session, error) {
		sess := s.Engine.SessionForStream(file.ID, mode, audioOrdinal, videoIndex, height, variant)
		switch {
		case sess == nil:
			return ensureAt(in, startSeg)
		case sess.StartSegment() == startSeg:
			sess.Touch()
			return sess, nil
		case sess.IdleFor() < media.SessionActiveFor:
			// Playback in progress: follow it.
			sess.Touch()
			return sess, nil
		default:
			// Nothing has been consumed here for a while, so treat this as a new
			// playback and begin where it asks.
			s.Engine.CloseStreamSessions(file.ID, mode, audioOrdinal, videoIndex, height, variant, nil)
			return ensureAt(in, startSeg)
		}
	}

	switch {
	case rest == "" || strings.HasPrefix(rest, "index") || strings.HasPrefix(rest, "playlist"):
		// Nothing has been written to the response while these waits fail, so a
		// packager that died on a refused link can be retried with a fresh one.
		err := media.RetryWithFreshLink(ctx, s.Engine, file, drv, inputs, func(in []media.Input) error {
			sess, err := streamSession(in)
			if err != nil {
				return fmt.Errorf("打包会话启动失败: %w", err)
			}
			// The playlist is rewritten in place as the packager runs, so "it
			// exists" is not "it is readable": a half-written manifest is fatal to
			// a player.
			if err := sess.WaitForPlaylist(ctx, 45*time.Second); err != nil {
				return fmt.Errorf("打包播放列表未就绪: %w", err)
			}
			pl, err := os.ReadFile(sess.PlaylistPath())
			if err != nil {
				return fmt.Errorf("读取播放列表失败: %w", err)
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write([]byte(media.RewriteSessionPlaylist(string(pl), base, sig)))
			return nil
		})
		if err != nil {
			log.Printf("hls playlist for file %d failed: %v", file.ID, err)
			fail(w, 502, err.Error())
		}

	case rest == "init.mp4":
		err := media.RetryWithFreshLink(ctx, s.Engine, file, drv, inputs, func(in []media.Input) error {
			sess, err := streamSession(in)
			if err != nil {
				return fmt.Errorf("打包会话启动失败: %w", err)
			}
			// ffmpeg writes the init segment in place, so it has to be waited for
			// until it is no longer growing — half of it makes the whole stream
			// undecodable.
			if err := sess.WaitForInit(ctx, 45*time.Second); err != nil {
				return fmt.Errorf("初始化分片未就绪: %w", err)
			}
			s.serveSessionFile(w, r, ctx, sess, sess.InitPath(), false)
			return nil
		})
		if err != nil {
			fail(w, 502, err.Error())
		}

	default:
		idx, ok := media.ParseSessionSegment(rest)
		if !ok {
			fail(w, 404, "未知 HLS 资源")
			return
		}
		sess := s.Engine.SessionForSegment(file.ID, mode, audioOrdinal, videoIndex, height, variant, idx, media.SessionSeekAhead)
		if sess == nil {
			// Outside what is packaged (a seek, forwards or backwards): package
			// from there instead of waiting for a session that will never reach
			// it. Yesterday's sessions go, since the player has moved on.
			//
			// A two-stream source keeps its single from-the-start session: there is
			// nowhere else to start from, so the request waits for the segment to be
			// packaged instead (packaging runs far faster than playback).
			if !multi {
				s.Engine.CloseStreamSessions(file.ID, mode, audioOrdinal, videoIndex, height, variant, nil)
			}
			next, err := ensureAt(inputs, idx)
			if err != nil {
				log.Printf("hls session for file %d failed to start at segment %d: %v", file.ID, idx, err)
				fail(w, 502, "打包会话启动失败: "+err.Error())
				return
			}
			sess = next
		} else {
			sess.Touch()
		}
		s.serveSessionFile(w, r, ctx, sess, sess.SegmentPath(idx), true)
	}
}

// serveHLSSubtitles answers the subtitle side of HLS: "master.m3u8" lists the
// media stream plus one EXT-X-MEDIA rendition per servable subtitle, and
// "subs/<id>.m3u8" is the single-segment playlist for one of them.
//
// This exists for players that manage subtitles themselves — Safari on iOS plays
// HLS natively and does not use <track> elements — so the same ranking that picks
// the default for the web player also picks the rendition marked DEFAULT.
func (s *Server) serveHLSSubtitles(w http.ResponseWriter, r *http.Request, ctx context.Context,
	file *models.MediaFile, drv source.Driver, mode, rest string, q url.Values, startSec float64) {

	probe, err := s.Engine.EnsureProbe(ctx, file, drv)
	if err != nil {
		fail(w, 500, "媒体探测失败: "+err.Error())
		return
	}
	servable := make([]models.Subtitle, 0, 4)
	for _, sub := range s.discoverSubtitles(ctx, drv, file) {
		if sub.Convertible {
			servable = append(servable, sub)
		}
	}
	defSubID := media.PickDefaultSubtitle(servable)
	// Rendition cues are timed against the stream the player is on, which begins
	// where packaging begins; the VTT endpoint shifts them by this much.
	streamStart := s.streamStartFor(mode, startSec, false)
	sig := signedQuery(s.Cfg.Secret, file.ID, sigTTL)
	startParam := ""
	if streamStart > 0 {
		startParam = "&start=" + strconv.FormatFloat(streamStart, 'f', 3, 64)
	}
	// The master playlist is a second entry point into the media stream, so the
	// variant it names has to carry the same rung the master was asked for.
	qParam := ""
	if qn := qualityParam(r); qn > 0 {
		qParam = "&q=" + strconv.Itoa(qn)
	}

	if rest == "master.m3u8" {
		if len(servable) == 0 {
			fail(w, 404, "该文件没有可用字幕")
			return
		}
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
		for _, sub := range servable {
			def := "NO"
			if sub.ID == defSubID {
				def = "YES"
			}
			fmt.Fprintf(&b, `#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="%s",DEFAULT=%s,AUTOSELECT=YES,FORCED=NO,URI="/api/hls/%d/subs/%d.m3u8%s%s"`+"\n",
				escapeM3U8(sub.Title), def, file.ID, sub.ID, sig, startParam)
		}
		// One variant, offering the subtitles. BANDWIDTH is required by the spec;
		// with a single variant the player has nothing to choose between, so an
		// estimate from the file's own bitrate is enough.
		bw := 2000000
		if size, dur := file.Size, probe.Duration(); dur > 0 && size > 0 {
			if est := int(float64(size) * 8 / dur); est > bw {
				bw = est
			}
		}
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d,SUBTITLES=\"subs\"\n", bw)
		fmt.Fprintf(&b, "/api/hls/%d/index.m3u8%s&m=%s%s%s\n", file.ID, sig, mode, startParam, qParam)
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(b.String()))
		return
	}

	// subs/<id>.m3u8: one WebVTT document covering the whole file.
	subID, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(rest, "subs/"), ".m3u8"), 10, 64)
	if err != nil {
		fail(w, 404, "未知字幕")
		return
	}
	var target *models.Subtitle
	for i := range servable {
		if servable[i].ID == subID {
			target = &servable[i]
			break
		}
	}
	if target == nil {
		fail(w, 404, "字幕不存在或无法转换")
		return
	}
	dur := probe.Duration()
	if dur <= 0 {
		dur = 1
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-PLAYLIST-TYPE:VOD\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n", int(math.Ceil(dur)))
	fmt.Fprintf(&b, "#EXTINF:%.3f,\n", dur)
	fmt.Fprintf(&b, "/api/subs/%d/%d.vtt%s%s\n", file.ID, target.ID, sig, startParam)
	b.WriteString("#EXT-X-ENDLIST\n")
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(b.String()))
}

// escapeM3U8 keeps a subtitle's own title from breaking out of the attribute it
// is written into.
func escapeM3U8(s string) string {
	s = strings.ReplaceAll(s, `"`, "'")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

// serveSessionFile waits for a packaged file and serves it with Range support.
// Media segments are shifted onto the absolute timeline first (see
// Session.ShiftSegment); the init segment carries no timestamps.
func (s *Server) serveSessionFile(w http.ResponseWriter, r *http.Request, ctx context.Context, sess *media.Session, path string, shift bool) {
	if err := sess.WaitForFile(ctx, path, 45*time.Second); err != nil {
		log.Printf("hls session %s: %v", filepath.Base(path), err)
		w.Header().Set("Retry-After", "2")
		http.Error(w, "分片尚未就绪,请重试", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	// Segments are transient: the session directory goes away when the session
	// closes, so nothing here may be cached by the client.
	w.Header().Set("Cache-Control", "no-store")

	if !shift {
		f, err := os.Open(path)
		if err != nil {
			fail(w, 502, "读取分片失败: "+err.Error())
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			fail(w, 502, "读取分片失败: "+err.Error())
			return
		}
		http.ServeContent(w, r, filepath.Base(path), fi.ModTime(), f)
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		fail(w, 502, "读取分片失败: "+err.Error())
		return
	}
	data = sess.ShiftSegment(data)
	http.ServeContent(w, r, filepath.Base(path), time.Now(), bytes.NewReader(data))
}

// ---------- /api/subs/{id}/{sub}.vtt ----------

func (s *Server) handleSubtitle(w http.ResponseWriter, r *http.Request) {
	fileID, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	subID, err := strconv.ParseInt(stripVTT(r.PathValue("sub")), 10, 64)
	if err != nil {
		fail(w, 400, "bad subtitle id")
		return
	}
	sub, err := s.Store.SubtitleByID(subID)
	if err != nil || sub.FileID != fileID {
		fail(w, 404, "字幕不存在")
		return
	}
	file, err := s.Store.FileByID(fileID)
	if err != nil {
		fail(w, 404, "文件不存在")
		return
	}
	drv, err := s.Engine.DriverFor(file)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	var data []byte
	if sub.Embedded {
		data, err = s.Engine.ExtractEmbeddedSubtitle(ctx, file, drv, sub.StreamIdx)
		if err != nil {
			fail(w, 500, "字幕提取失败: "+err.Error())
			return
		}
	} else {
		rc, err := drv.Open(ctx, sub.Path)
		if err != nil {
			fail(w, 502, "字幕读取失败: "+err.Error())
			return
		}
		defer rc.Close()
		raw, err := io.ReadAll(io.LimitReader(rc, 8<<20))
		if err != nil {
			fail(w, 502, "字幕读取失败: "+err.Error())
			return
		}
		format, _ := s.subFormatFor(ctx, drv, sub)
		data = media.ToVTT(format, raw)
	}
	// A subtitle rendition inside an HLS stream is timed against that stream,
	// which begins where the packaging session began; the caller says where that
	// is and the cues are moved accordingly. Without it (a plain <track>) the
	// client shifts them itself, since it knows the offset of its own clock.
	if v := r.URL.Query().Get("start"); v != "" {
		if off, err := strconv.ParseFloat(v, 64); err == nil && off > 0 {
			data = media.ShiftVTT(data, off)
			// Shifted cues are tied to one playback, so they must not be cached
			// the way the unshifted document is.
			w.Header().Set("Cache-Control", "no-store")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=86400")
		}
	} else {
		w.Header().Set("Cache-Control", "public, max-age=86400")
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	_, _ = w.Write(data)
}

// ---------- /api/danmaku/{id}/comments.json ----------

// handleDanmaku serves a file's bullet comments (弹幕) as JSON, for a source
// that publishes them (bilibili). The whole set goes out in one response: they
// are drawn on a canvas over the picture, so the player wants them before the
// first frame, not as a live feed.
func (s *Server) handleDanmaku(w http.ResponseWriter, r *http.Request) {
	fileID, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	file, err := s.Store.FileByID(fileID)
	if err != nil {
		fail(w, 404, "文件不存在")
		return
	}
	drv, err := s.Engine.DriverFor(file)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	dm, ok := drv.(source.Danmaku)
	if !ok {
		fail(w, 404, "该文件没有弹幕")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	comments, err := dm.Comments(ctx, file.Path)
	if err != nil {
		fail(w, 502, "弹幕获取失败: "+err.Error())
		return
	}
	// An episode's comments do not change while it is being watched, and the
	// source caches them too; half an hour keeps a re-watch off the API.
	w.Header().Set("Cache-Control", "public, max-age=1800")
	writeJSON(w, 200, map[string]any{"count": len(comments), "comments": comments})
}
