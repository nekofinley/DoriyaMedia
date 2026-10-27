package com.doriyamedia.spike.playback

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class SafePlaybackLogTest {
  @Test
  fun urlOriginDropsCredentialsPathQueryAndFragment() {
    val origin =
      SafePlaybackLog.urlOrigin(
        "https://user:password@media.example:8443/private/video.mp4?token=secret-token#part",
      )

    assertEquals("https://media.example:8443", origin)
    assertFalse(origin.contains("user"))
    assertFalse(origin.contains("password"))
    assertFalse(origin.contains("private"))
    assertFalse(origin.contains("secret-token"))
  }

  @Test
  fun invalidUrlUsesFixedPlaceholder() {
    assertEquals("[invalid-url]", SafePlaybackLog.urlOrigin("not a url secret-token"))
    assertEquals("[invalid-url]", SafePlaybackLog.urlOrigin(""))
  }

  @Test
  fun headerSummaryContainsNamesButNeverValues() {
    val summary =
      SafePlaybackLog.headerNames(
        linkedMapOf(
          "Referer" to "https://private.example/watch?token=secret-token",
          "User-Agent" to "Secret Credential Agent",
        ),
      )

    assertEquals("Referer,User-Agent", summary)
    assertFalse(summary.contains("secret-token"))
    assertFalse(summary.contains("Secret Credential Agent"))
  }

  @Test
  fun prepareSummaryOnlyContainsOriginsAndHeaderNames() {
    val summary =
      SafePlaybackLog.prepareSummary(
        mode = PlaybackMode.MERGE,
        videoOrigin = SafePlaybackLog.urlOrigin(
          "https://video.example/private/v.mp4?token=secret-token",
        ),
        audioOrigin = SafePlaybackLog.urlOrigin(
          "https://audio.example/private/a.mp4?token=secret-token",
        ),
        videoHeaders =
          mapOf(
            "Referer" to "https://private.example/watch?token=secret-token",
            "User-Agent" to "DoriyaMedia secret-token",
          ),
        audioHeaders =
          mapOf(
            "Origin" to "https://private.example",
            "Accept" to "audio/mp4 secret-token",
          ),
      )

    assertEquals(
      "event=prepare mode=merge video_origin=https://video.example " +
        "audio_origin=https://audio.example video_headers=Referer,User-Agent " +
        "audio_headers=Accept,Origin",
      summary,
    )
    assertFalse(summary.contains("secret-token"))
  }

  @Test
  fun errorSummaryDoesNotAcceptFreeFormMessages() {
    val summary =
      SafePlaybackLog.errorSummary(
        errorClassName = "SourceError?url=https://example.com/?token=secret-token",
        errorCodeName = "ERROR_CODE_IO_BAD_HTTP_STATUS&header=secret-token",
      )

    assertEquals("event=playback_error type=unknown code=unknown", summary)
    assertFalse(summary.contains("https://"))
    assertFalse(summary.contains("secret-token"))
  }
}
