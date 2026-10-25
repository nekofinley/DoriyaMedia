package bangumi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"doriyamedia/internal/models"
)

const maxCollectionsLimit = 50

var (
	// ErrInvalidCollectionsQuery lets the HTTP layer distinguish bad paging
	// parameters from an unavailable or rejected Bangumi account.
	ErrInvalidCollectionsQuery = errors.New("Bangumi 收藏分页参数无效")
	// ErrCollectionsUpstream deliberately omits the upstream body. Bangumi
	// error bodies are not part of this API and must never become a channel for
	// echoing a caller's credential.
	ErrCollectionsUpstream = errors.New("Bangumi 收藏服务暂时不可用")
)

// Collections returns the caller's own anime collection page plus local media
// matches. An unbound local account is a successful empty page, not an error.
func (s *Service) Collections(ctx context.Context, userID int64, collectionType, offset, limit int) (*models.BangumiCollectionsPage, error) {
	if collectionType < 0 || collectionType > 5 {
		return nil, fmt.Errorf("%w: type 必须在 0 到 5 之间", ErrInvalidCollectionsQuery)
	}
	if offset < 0 {
		return nil, fmt.Errorf("%w: offset 不能为负数", ErrInvalidCollectionsQuery)
	}
	if limit < 1 || limit > maxCollectionsLimit {
		return nil, fmt.Errorf("%w: limit 必须在 1 到 50 之间", ErrInvalidCollectionsQuery)
	}

	page := &models.BangumiCollectionsPage{
		Offset: offset,
		Limit:  limit,
		Items:  []models.BangumiCollectionItem{},
	}
	account, err := s.Store.BangumiAccountByUser(userID)
	if errors.Is(err, sql.ErrNoRows) {
		return page, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 Bangumi 账户失败: %w", err)
	}
	page.Connected = true
	page.Username = account.Username
	page.Nickname = account.Nickname
	page.AvatarURL = account.AvatarURL

	token, err := decryptToken(s.Cfg.Secret, account.UserID, account.TokenEnc)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	remote, err := s.clientFor(token).Collections(ctx, account.Username, collectionType, offset, limit)
	if err != nil {
		return nil, sanitizeCollectionsError(err)
	}

	subjectIDs := make([]int, 0, len(remote.Data))
	for _, collection := range remote.Data {
		subjectID := collection.SubjectID
		if subjectID <= 0 {
			subjectID = collection.Subject.ID
		}
		if subjectID > 0 {
			subjectIDs = append(subjectIDs, subjectID)
		}
	}
	localItems, err := s.Store.ItemIDsByBGMIDs(subjectIDs)
	if err != nil {
		return nil, fmt.Errorf("匹配本地媒体库失败: %w", err)
	}

	for _, collection := range remote.Data {
		subject := collection.Subject
		subjectID := collection.SubjectID
		if subjectID <= 0 {
			subjectID = subject.ID
		}
		if subjectID <= 0 {
			continue
		}
		page.Items = append(page.Items, models.BangumiCollectionItem{
			SubjectID:      subjectID,
			Name:           subject.Name,
			NameCN:         subject.NameCN,
			Summary:        subject.Summary,
			CoverURL:       proxyCoverURL(firstNonEmpty(subject.Images.Large, subject.Images.Common, subject.Images.Medium, subject.Images.Small, subject.Images.Grid)),
			Date:           subject.Date,
			Eps:            subject.Eps,
			Score:          subject.Score,
			Rank:           subject.Rank,
			CollectionType: collection.Type,
			EpStatus:       collection.EpStatus,
			UpdatedAt:      collection.UpdatedAt,
			LocalItemID:    localItems[subjectID],
		})
	}
	page.Total = max(remote.Total, 0)
	page.HasMore = offset+len(remote.Data) < page.Total
	return page, nil
}

func sanitizeCollectionsError(err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("%w: Bangumi 令牌无效、已过期或无权读取收藏", ErrCollectionsUpstream)
		case http.StatusTooManyRequests:
			return fmt.Errorf("%w: Bangumi 请求过于频繁,请稍后再试", ErrCollectionsUpstream)
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: 请求超时", ErrCollectionsUpstream)
	}
	return ErrCollectionsUpstream
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// proxyCoverURL only turns URLs that pass the strict host validation into
// same-origin endpoints. Invalid upstream values are omitted instead of being
// handed to the browser as a remote or arbitrary proxy target.
func proxyCoverURL(rawURL string) string {
	if _, err := parseCoverURL(rawURL); err != nil {
		return ""
	}
	return "/api/bangumi/cover?url=" + url.QueryEscape(rawURL)
}
