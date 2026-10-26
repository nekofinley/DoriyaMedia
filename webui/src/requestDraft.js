const REQUEST_DRAFT_KEY = 'doriya.media-request-draft'

/** Hand one Bangumi collection card to the request page without a large URL. */
export function saveMediaRequestDraft(draft) {
  try {
    sessionStorage.setItem(REQUEST_DRAFT_KEY, JSON.stringify(draft))
  } catch {
    // Session storage can be unavailable in hardened browsers. The request
    // page still falls back to the title/Bangumi ID in the route query.
  }
}

/** Read and consume a draft written by the Bangumi shelf. */
export function takeMediaRequestDraft(expectedBGMID = 0) {
  try {
    const raw = sessionStorage.getItem(REQUEST_DRAFT_KEY)
    if (!raw) return null
    sessionStorage.removeItem(REQUEST_DRAFT_KEY)
    const draft = JSON.parse(raw)
    if (draft?.provider !== 'bangumi' || !draft.title || !Number(draft.bgm_id)) return null
    if (expectedBGMID > 0 && Number(draft.bgm_id) !== expectedBGMID) return null
    return draft
  } catch {
    return null
  }
}

/**
 * Collection covers use same-origin proxy URLs. Requests store the validated
 * upstream cover, so unwrap the proxy's url parameter before submitting.
 */
export function bangumiCoverSource(raw) {
  const value = String(raw || '')
  if (!value.startsWith('/api/bangumi/cover?')) return value
  try {
    return new URL(value, window.location.origin).searchParams.get('url') || ''
  } catch {
    return ''
  }
}
