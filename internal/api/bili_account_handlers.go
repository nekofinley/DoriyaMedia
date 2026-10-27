package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

// bili_account_handlers.go: the account half of a bilibili source.
//
// A pasted cookie is a copy of a browser session, and bilibili rotates those from
// the browser: the copy dies at the first rotation and nothing says so. What the
// viewer then sees is a picture capped at 480P and a 会员 episode answering "this
// needs a membership" — on an account that has one. These routes exist so that
// failure is visible on the settings page, and so it can be fixed without a
// browser export: a scanned QR hands this server a session of its own, with a
// refresh_token that lets it stay logged in.

// biliDriverForSource builds a driver for a bilibili source row, refusing any
// other kind of source with a message the settings page can show.
func biliDriverForSource(src *models.Source) (*source.BilibiliDriver, models.SourceConfig, error) {
	var cfg models.SourceConfig
	if src.Config != "" {
		if err := json.Unmarshal([]byte(src.Config), &cfg); err != nil {
			return nil, cfg, err
		}
	}
	if src.Type != "bilibili" {
		return nil, cfg, errors.New("这个存储源不是 B 站番剧")
	}
	drv, err := source.NewBilibili(cfg.BiliCookie, cfg.BiliCodec, cfg.BiliProxy, cfg.BiliPlayableOnly)
	if err != nil {
		return nil, cfg, err
	}
	return drv, cfg, nil
}

// biliSourceFor resolves {id} into a bilibili source row.
func (s *Server) biliSourceFor(r *http.Request) (*models.Source, bool) {
	id, ok := pathID(r)
	if !ok {
		return nil, false
	}
	src, err := s.Store.SourceByID(id)
	if err != nil {
		return nil, false
	}
	if src.Type != "bilibili" {
		return nil, false
	}
	return src, true
}

// biliAccountTimeout bounds the calls that go out to bilibili. A dead proxy must
// not hang the settings page.
const biliAccountTimeout = 25 * time.Second

// handleBiliAccount reports who the source's cookie belongs to, and what that
// means for playback.
//
// It never fails for a dead cookie: that is the answer it exists to give, and
// telling it apart from "the proxy is down" is most of the point.
func (s *Server) handleBiliAccount(w http.ResponseWriter, r *http.Request, user *models.User) {
	src, ok := s.biliSourceFor(r)
	if !ok {
		fail(w, 404, "不是 B 站存储源")
		return
	}
	drv, cfg, err := biliDriverForSource(src)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), biliAccountTimeout)
	defer cancel()
	writeJSON(w, 200, drv.Health(ctx, strings.TrimSpace(cfg.BiliRefreshToken) != ""))
}

// handleBiliQR starts a scan-to-login for a source: a QR code plus the key the
// poll endpoint takes back.
//
// The admin who can see this page is the admin who owns the source, so the code
// is shown here rather than mailed anywhere: whoever scans it logs *this* server
// into *their* account.
func (s *Server) handleBiliQR(w http.ResponseWriter, r *http.Request, user *models.User) {
	src, ok := s.biliSourceFor(r)
	if !ok {
		fail(w, 404, "不是 B 站存储源")
		return
	}
	drv, _, err := biliDriverForSource(src)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), biliAccountTimeout)
	defer cancel()
	qr, err := drv.QRLoginStart(ctx)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, qr)
}

// handleBiliQRPoll asks whether a login QR has been confirmed, and stores the
// credential when it has.
//
// Storing is the whole reason this is a server route rather than something the
// page does: the credential must land in the source's config, next to the codec
// and proxy settings, and the config is only readable by an administrator.
func (s *Server) handleBiliQRPoll(w http.ResponseWriter, r *http.Request, user *models.User) {
	src, ok := s.biliSourceFor(r)
	if !ok {
		fail(w, 404, "不是 B 站存储源")
		return
	}
	var req struct {
		Key string `json:"key"`
	}
	if err := readBody(r, &req); err != nil || strings.TrimSpace(req.Key) == "" {
		fail(w, 400, "缺少扫码标识")
		return
	}
	drv, cfg, err := biliDriverForSource(src)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), biliAccountTimeout)
	defer cancel()
	st, err := drv.QRLoginPoll(ctx, req.Key)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	if st.State != "confirmed" {
		writeJSON(w, 200, st)
		return
	}

	// The login worked: replace the credential in the source's own config. The
	// old cookie is a dead copy of somebody's browser session, and keeping it
	// next to the new one would only make the next read ambiguous.
	cfg.BiliCookie = st.Cookie
	cfg.BiliRefreshToken = st.RefreshToken
	raw, err := json.Marshal(cfg)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	src.Config = string(raw)
	if err := s.Store.UpdateSource(src); err != nil {
		fail(w, 500, "登录成功但保存失败: "+err.Error())
		return
	}
	if s.Engine != nil {
		s.Engine.InvalidateSourceCaches(src.ID)
	}
	// Every cached listing was read with the dead cookie. They are all suspect
	// now — a 480P ladder, an empty season, a follow list that failed — so the
	// cache goes with the login rather than answering for it.
	source.ClearBiliCache()
	writeJSON(w, 200, st)
}

// handleBiliRefresh renews a stored session with its refresh_token.
//
// This is what makes a scanned login outlive a pasted one: bilibili hands out a
// refresh_token with a QR login, and a client that keeps using it is never logged
// out. The new token replaces the old one, because the old one dies the moment
// this succeeds.
func (s *Server) handleBiliRefresh(w http.ResponseWriter, r *http.Request, user *models.User) {
	src, ok := s.biliSourceFor(r)
	if !ok {
		fail(w, 404, "不是 B 站存储源")
		return
	}
	drv, cfg, err := biliDriverForSource(src)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), biliAccountTimeout)
	defer cancel()
	cookie, refresh, err := drv.RefreshCookie(ctx, cfg.BiliRefreshToken)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	cfg.BiliCookie, cfg.BiliRefreshToken = cookie, refresh
	raw, err := json.Marshal(cfg)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	src.Config = string(raw)
	if err := s.Store.UpdateSource(src); err != nil {
		fail(w, 500, "续期成功但保存失败: "+err.Error())
		return
	}
	if s.Engine != nil {
		s.Engine.InvalidateSourceCaches(src.ID)
	}
	source.ClearBiliCache()
	writeJSON(w, 200, drv.Health(ctx, true))
}
