package com.doriyamedia.spike.playback

import java.net.URI
import java.net.URISyntaxException
import java.util.Locale

enum class PlaybackMode {
  SINGLE,
  MERGE,
}

data class StreamRequest(
  val url: String,
  val headers: Map<String, String>,
  val containerMimeType: String?,
)

sealed interface PlaybackRequest {
  data class Single(val stream: StreamRequest) : PlaybackRequest

  data class Merge(
    val video: StreamRequest,
    val audio: StreamRequest,
  ) : PlaybackRequest
}

data class PlaybackDraft(
  val mode: PlaybackMode,
  val primaryUrl: String,
  val secondaryUrl: String = "",
  val primaryHeadersText: String = "",
  val secondaryHeadersText: String = "",
  val primaryContainerMimeType: String = "",
  val secondaryContainerMimeType: String = "",
)

sealed interface ValidationResult<out T> {
  data class Success<T>(val value: T) : ValidationResult<T>

  data class Failure(val reason: String) : ValidationResult<Nothing>
}

object PlaybackInputValidator {
  private const val MAX_URL_LENGTH = 4096
  private const val MAX_HEADER_COUNT = 5
  private val mergeVideoMimeTypes = setOf("video/mp4", "video/webm")
  private val mergeAudioMimeTypes = setOf("audio/mp4", "audio/webm")
  private val singleMimeTypes =
    mergeVideoMimeTypes +
      mergeAudioMimeTypes +
      setOf(
        "application/dash+xml",
        "application/vnd.apple.mpegurl",
        "application/x-mpegurl",
      )

  private data class HeaderRule(
    val canonicalName: String,
    val maxValueLength: Int,
  )

  private val headerRules =
    listOf(
        HeaderRule("Referer", 2048),
        HeaderRule("Origin", 512),
        HeaderRule("User-Agent", 512),
        HeaderRule("Accept", 512),
        HeaderRule("Accept-Language", 256),
      )
      .associateBy { it.canonicalName.lowercase(Locale.ROOT) }

  fun validate(draft: PlaybackDraft): ValidationResult<PlaybackRequest> =
    try {
      when (draft.mode) {
        PlaybackMode.SINGLE -> {
          val url = validateHttpsUrl("URL", draft.primaryUrl)
          val headers = parseHeaders(draft.primaryHeadersText, "Stream")
          val containerMimeType =
            validateContainerMimeType(
              label = "Container MIME",
              rawValue = draft.primaryContainerMimeType,
              allowed = singleMimeTypes,
              defaultWhenBlank = null,
            )
          ValidationResult.Success(
            PlaybackRequest.Single(
              StreamRequest(
                url = url,
                headers = headers,
                containerMimeType = containerMimeType,
              ),
            ),
          )
        }

        PlaybackMode.MERGE -> {
          val videoUrl = validateHttpsUrl("Video URL", draft.primaryUrl)
          val audioUrl = validateHttpsUrl("Audio URL", draft.secondaryUrl)
          if (canonicalUrlIdentity(videoUrl) == canonicalUrlIdentity(audioUrl)) {
            fail("Video URL and Audio URL must be different.")
          }
          val videoHeaders = parseHeaders(draft.primaryHeadersText, "Video")
          val audioHeaders = parseHeaders(draft.secondaryHeadersText, "Audio")
          val videoMimeType =
            validateContainerMimeType(
              label = "Video container MIME",
              rawValue = draft.primaryContainerMimeType,
              allowed = mergeVideoMimeTypes,
              defaultWhenBlank = "video/mp4",
            )
          val audioMimeType =
            validateContainerMimeType(
              label = "Audio container MIME",
              rawValue = draft.secondaryContainerMimeType,
              allowed = mergeAudioMimeTypes,
              defaultWhenBlank = "audio/mp4",
            )
          ValidationResult.Success(
            PlaybackRequest.Merge(
              video =
                StreamRequest(
                  url = videoUrl,
                  headers = videoHeaders,
                  containerMimeType = videoMimeType,
                ),
              audio =
                StreamRequest(
                  url = audioUrl,
                  headers = audioHeaders,
                  containerMimeType = audioMimeType,
                ),
            ),
          )
        }
      }
    } catch (problem: InputProblem) {
      ValidationResult.Failure(problem.message)
    }

  fun parseHeaders(text: String, streamLabel: String = "Stream"): Map<String, String> {
    if (text.isBlank()) {
      return emptyMap()
    }

    val headers = linkedMapOf<String, String>()
    text.lineSequence().forEachIndexed { index, rawLine ->
      val line = rawLine.trim()
      if (line.isEmpty()) {
        return@forEachIndexed
      }

      val separator = line.indexOf(':')
      if (separator <= 0) {
        fail("$streamLabel header line ${index + 1} must use the form Name: Value.")
      }

      val suppliedName = line.substring(0, separator).trim()
      val value = line.substring(separator + 1).trim()
      val rule =
        headerRules[suppliedName.lowercase(Locale.ROOT)]
          ?: fail("$streamLabel header line ${index + 1} is not in the allowlist.")

      if (headers.containsKey(rule.canonicalName)) {
        fail("Header ${rule.canonicalName} appears more than once.")
      }
      if (headers.size >= MAX_HEADER_COUNT) {
        fail("At most $MAX_HEADER_COUNT $streamLabel headers are allowed.")
      }
      validateHeaderValue(rule, value)
      headers[rule.canonicalName] = value
    }
    return headers.toMap()
  }

  private fun validateContainerMimeType(
    label: String,
    rawValue: String,
    allowed: Set<String>,
    defaultWhenBlank: String?,
  ): String? {
    val value = rawValue.trim().lowercase(Locale.ROOT)
    if (value.isEmpty()) {
      return defaultWhenBlank
    }
    if (value.any { it.isISOControl() || it.isWhitespace() }) {
      fail("$label contains whitespace or control characters.")
    }
    if (value !in allowed) {
      fail("$label is not supported by this spike.")
    }
    return value
  }

  private fun validateHttpsUrl(label: String, rawUrl: String): String {
    val value = rawUrl.trim()
    if (value.isEmpty()) {
      fail("$label is required.")
    }
    if (value.length > MAX_URL_LENGTH) {
      fail("$label is too long.")
    }
    if (value.any { it.isISOControl() || it.isWhitespace() }) {
      fail("$label contains whitespace or control characters.")
    }

    val uri =
      try {
        URI(value)
      } catch (_: URISyntaxException) {
        fail("$label is not a valid URI.")
      }

    if (!uri.isAbsolute || uri.scheme?.lowercase(Locale.ROOT) != "https") {
      fail("$label must use HTTPS.")
    }
    if (uri.host.isNullOrBlank()) {
      fail("$label must include a valid host.")
    }
    if (uri.rawUserInfo != null) {
      fail("$label must not contain user information.")
    }
    if (uri.rawFragment != null) {
      fail("$label must not contain a fragment.")
    }
    if (uri.port == 0 || uri.port > 65535) {
      fail("$label contains an invalid port.")
    }

    return uri.normalize().toASCIIString()
  }

  private fun canonicalUrlIdentity(url: String): String {
    val uri = URI(url).normalize()
    val effectivePort =
      if (uri.port >= 0) {
        uri.port
      } else {
        when (uri.scheme.lowercase(Locale.ROOT)) {
          "http" -> 80
          "https" -> 443
          else -> -1
        }
      }
    val path = uri.rawPath.orEmpty().ifEmpty { "/" }
    return buildString {
      append(uri.scheme.lowercase(Locale.ROOT))
      append("://")
      append(uri.host.lowercase(Locale.ROOT))
      append(':')
      append(effectivePort)
      append(path)
      uri.rawQuery?.let {
        append('?')
        append(it)
      }
    }
  }

  private fun validateHeaderValue(rule: HeaderRule, value: String) {
    if (value.isEmpty()) {
      fail("Header ${rule.canonicalName} must not be empty.")
    }
    if (value.length > rule.maxValueLength) {
      fail("Header ${rule.canonicalName} is too long.")
    }
    if (value.any { it.isISOControl() }) {
      fail("Header ${rule.canonicalName} contains control characters.")
    }

    when (rule.canonicalName) {
      "Referer" -> validateReferer(value)
      "Origin" -> validateOrigin(value)
    }
  }

  private fun validateReferer(value: String) {
    val uri = parseHeaderUri("Referer", value)
    if (uri.scheme?.lowercase(Locale.ROOT) !in setOf("http", "https")) {
      fail("Header Referer must use HTTP or HTTPS.")
    }
    if (uri.host.isNullOrBlank() || uri.rawUserInfo != null || uri.rawFragment != null) {
      fail("Header Referer must be an absolute URL without credentials or a fragment.")
    }
  }

  private fun validateOrigin(value: String) {
    val uri = parseHeaderUri("Origin", value)
    if (uri.scheme?.lowercase(Locale.ROOT) !in setOf("http", "https")) {
      fail("Header Origin must use HTTP or HTTPS.")
    }
    if (
      uri.host.isNullOrBlank() ||
        uri.rawUserInfo != null ||
        !uri.rawPath.isNullOrEmpty() ||
        uri.rawQuery != null ||
        uri.rawFragment != null
    ) {
      fail("Header Origin must contain only scheme, host, and optional port.")
    }
  }

  private fun parseHeaderUri(headerName: String, value: String): URI =
    try {
      URI(value)
    } catch (_: URISyntaxException) {
      fail("Header $headerName is not a valid URI.")
    }

  private fun fail(message: String): Nothing = throw InputProblem(message)

  private class InputProblem(override val message: String) : Exception(message)
}
