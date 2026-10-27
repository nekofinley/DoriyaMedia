package com.doriyamedia.spike.playback

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class PlaybackInputValidatorTest {
  @Test
  fun singleHttpsWithQueryIsAccepted() {
    val result =
      PlaybackInputValidator.validate(
        PlaybackDraft(
          mode = PlaybackMode.SINGLE,
          primaryUrl = "https://media.example/video.mp4?token=secret-token",
        ),
      )

    assertTrue(result is ValidationResult.Success)
    val request = (result as ValidationResult.Success).value as PlaybackRequest.Single
    assertEquals("https://media.example/video.mp4?token=secret-token", request.stream.url)
    assertEquals(null, request.stream.containerMimeType)
  }

  @Test
  fun malformedAndUnsafeUrlsAreRejectedWithoutEchoingValues() {
    val drafts =
      listOf(
        "" to "required",
        "http://media.example/video.mp4?token=secret-token" to "HTTPS",
        "https://user:password@media.example/video.mp4?token=secret-token" to "user information",
        "https://media.example/video.mp4#secret-fragment" to "fragment",
        "https:///video.mp4?token=secret-token" to "host",
      )

    drafts.forEach { (url, expectedReason) ->
      val result =
        PlaybackInputValidator.validate(
          PlaybackDraft(mode = PlaybackMode.SINGLE, primaryUrl = url),
        )

      assertTrue(result is ValidationResult.Failure)
      val reason = (result as ValidationResult.Failure).reason
      assertTrue(reason.contains(expectedReason, ignoreCase = true))
      assertFalse(reason.contains("secret-token", ignoreCase = true))
      assertFalse(reason.contains("password", ignoreCase = true))
    }
  }

  @Test
  fun mergeRequiresDistinctHttpsUrls() {
    val valid =
      PlaybackInputValidator.validate(
        PlaybackDraft(
          mode = PlaybackMode.MERGE,
          primaryUrl = "https://video.example/video.mp4",
          secondaryUrl = "https://audio.example/audio.mp4",
        ),
      )
    assertTrue(valid is ValidationResult.Success)
    assertTrue((valid as ValidationResult.Success).value is PlaybackRequest.Merge)

    val sameUrl =
      PlaybackInputValidator.validate(
        PlaybackDraft(
          mode = PlaybackMode.MERGE,
          primaryUrl = "https://media.example/same.mp4",
          secondaryUrl = "https://media.example/same.mp4",
        ),
      )
    assertTrue(sameUrl is ValidationResult.Failure)
    assertTrue((sameUrl as ValidationResult.Failure).reason.contains("different"))

    val insecureAudio =
      PlaybackInputValidator.validate(
        PlaybackDraft(
          mode = PlaybackMode.MERGE,
          primaryUrl = "https://video.example/video.mp4",
          secondaryUrl = "http://audio.example/audio.mp4",
        ),
      )
    assertTrue(insecureAudio is ValidationResult.Failure)
  }

  @Test
  fun mergeRejectsEquivalentUrlsAfterHostPortAndPathNormalization() {
    val equivalentPairs =
      listOf(
        "https://MEDIA.example/same.mp4" to "https://media.example/same.mp4",
        "https://media.example/same.mp4" to "https://media.example:443/same.mp4",
        "https://media.example" to "https://media.example/",
        "https://media.example/a/../same.mp4" to "https://media.example/same.mp4",
      )

    equivalentPairs.forEach { (videoUrl, audioUrl) ->
      val result =
        PlaybackInputValidator.validate(
          PlaybackDraft(
            mode = PlaybackMode.MERGE,
            primaryUrl = videoUrl,
            secondaryUrl = audioUrl,
          ),
        )

      assertTrue("$videoUrl and $audioUrl should be rejected", result is ValidationResult.Failure)
      assertTrue((result as ValidationResult.Failure).reason.contains("different"))
    }
  }

  @Test
  fun mergeStillAllowsDistinctQueriesOnTheSameEndpoint() {
    val result =
      PlaybackInputValidator.validate(
        PlaybackDraft(
          mode = PlaybackMode.MERGE,
          primaryUrl = "https://media.example/stream.m4s?track=video",
          secondaryUrl = "https://media.example/stream.m4s?track=audio",
        ),
      )

    assertTrue(result is ValidationResult.Success)
  }

  @Test
  fun mergeKeepsPerStreamHeadersAndContainerHintsSeparate() {
    val result =
      PlaybackInputValidator.validate(
        PlaybackDraft(
          mode = PlaybackMode.MERGE,
          primaryUrl = "https://media.example/stream.m4s?track=video",
          secondaryUrl = "https://media.example/stream.m4s?track=audio",
          primaryHeadersText =
            """
            Referer: https://www.example.com/video
            User-Agent: Video-Agent
            """.trimIndent(),
          secondaryHeadersText =
            """
            Origin: https://www.example.com
            Accept: audio/mp4
            """.trimIndent(),
          primaryContainerMimeType = "video/mp4",
          secondaryContainerMimeType = "audio/mp4",
        ),
      )

    assertTrue(result is ValidationResult.Success)
    val merge = (result as ValidationResult.Success).value as PlaybackRequest.Merge
    assertEquals(
      mapOf(
        "Referer" to "https://www.example.com/video",
        "User-Agent" to "Video-Agent",
      ),
      merge.video.headers,
    )
    assertEquals(
      mapOf(
        "Origin" to "https://www.example.com",
        "Accept" to "audio/mp4",
      ),
      merge.audio.headers,
    )
    assertEquals("video/mp4", merge.video.containerMimeType)
    assertEquals("audio/mp4", merge.audio.containerMimeType)
  }

  @Test
  fun mergeDefaultsContainerHintsAndRejectsWrongStreamKinds() {
    val draft =
      PlaybackDraft(
        mode = PlaybackMode.MERGE,
        primaryUrl = "https://video.example/video.mp4",
        secondaryUrl = "https://audio.example/audio.mp4",
      )
    val defaulted = PlaybackInputValidator.validate(draft)
    assertTrue(defaulted is ValidationResult.Success)
    val merge = (defaulted as ValidationResult.Success).value as PlaybackRequest.Merge
    assertEquals("video/mp4", merge.video.containerMimeType)
    assertEquals("audio/mp4", merge.audio.containerMimeType)

    val wrongKinds =
      listOf(
        draft.copy(primaryContainerMimeType = "audio/mp4") to "Video container MIME",
        draft.copy(secondaryContainerMimeType = "video/mp4") to "Audio container MIME",
        draft.copy(primaryContainerMimeType = "text/plain") to "Video container MIME",
      )
    wrongKinds.forEach { (invalidDraft, expectedReason) ->
      val result = PlaybackInputValidator.validate(invalidDraft)
      assertTrue(result is ValidationResult.Failure)
      assertTrue((result as ValidationResult.Failure).reason.contains(expectedReason))
    }
  }

  @Test
  fun singleStreamAcceptsAnExplicitContainerHint() {
    val result =
      PlaybackInputValidator.validate(
        PlaybackDraft(
          mode = PlaybackMode.SINGLE,
          primaryUrl = "https://media.example/live/index.m3u8?token=secret-token",
          primaryContainerMimeType = "application/vnd.apple.mpegurl",
        ),
      )

    assertTrue(result is ValidationResult.Success)
    val request = (result as ValidationResult.Success).value as PlaybackRequest.Single
    assertEquals("application/vnd.apple.mpegurl", request.stream.containerMimeType)
  }

  @Test
  fun headerNamesAreCanonicalizedAndAllReviewedHeadersAreAccepted() {
    val headers =
      PlaybackInputValidator.parseHeaders(
        """
        referer: https://www.example.com/watch
        ORIGIN: https://www.example.com
        user-agent: DoriyaMedia/0.1
        accept: video/mp4
        accept-language: zh-CN, en
        """.trimIndent(),
      )

    assertEquals(
      listOf("Accept", "Accept-Language", "Origin", "Referer", "User-Agent"),
      headers.keys.sorted(),
    )
    assertEquals("DoriyaMedia/0.1", headers["User-Agent"])
  }

  @Test
  fun forbiddenAndUnknownHeadersAreRejectedWithoutEchoingValues() {
    listOf("Authorization: Bearer secret-value", "X-Source-Token: secret-value").forEach {
      val result = validationWithHeaders(it)
      assertTrue(result is ValidationResult.Failure)
      val reason = (result as ValidationResult.Failure).reason
      assertTrue(reason.contains("allowlist"))
      assertFalse(reason.contains("secret-value"))
    }
  }

  @Test
  fun malformedInjectedAndDuplicateHeadersAreRejectedWithoutEchoingValues() {
    val inputs =
      listOf(
        "not-a-header",
        "User-Agent: first\nUser-Agent: second",
        "User-Agent: first\r\nAuthorization: secret-value",
        "User-Agent: value\u0000secret-value",
      )

    inputs.forEach { headers ->
      val result = validationWithHeaders(headers)
      assertTrue(result is ValidationResult.Failure)
      assertFalse((result as ValidationResult.Failure).reason.contains("secret-value"))
    }
  }

  @Test
  fun refererAndOriginSyntaxIsValidated() {
    assertTrue(validationWithHeaders("Referer: not a uri") is ValidationResult.Failure)
    assertTrue(
      validationWithHeaders("Referer: https://user:secret-value@example.com/")
        is ValidationResult.Failure,
    )
    assertTrue(
      validationWithHeaders("Origin: https://www.example.com/path")
        is ValidationResult.Failure,
    )
    assertTrue(
      validationWithHeaders("Origin: https://www.example.com?secret-value")
        is ValidationResult.Failure,
    )
  }

  @Test
  fun emptyAndOversizedHeaderValuesAreRejected() {
    assertTrue(validationWithHeaders("Accept: ") is ValidationResult.Failure)
    assertTrue(
      validationWithHeaders("User-Agent: ${"x".repeat(513)}") is ValidationResult.Failure,
    )
  }

  private fun validationWithHeaders(headers: String): ValidationResult<PlaybackRequest> =
    PlaybackInputValidator.validate(
      PlaybackDraft(
        mode = PlaybackMode.SINGLE,
        primaryUrl = "https://media.example/video.mp4",
        primaryHeadersText = headers,
      ),
    )
}
