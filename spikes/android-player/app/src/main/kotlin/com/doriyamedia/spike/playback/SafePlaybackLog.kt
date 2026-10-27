package com.doriyamedia.spike.playback

import java.net.URI
import java.net.URISyntaxException
import java.util.Locale

object SafePlaybackLog {
  fun urlOrigin(rawUrl: String): String {
    val uri =
      try {
        URI(rawUrl.trim())
      } catch (_: URISyntaxException) {
        return "[invalid-url]"
      }
    val scheme = uri.scheme?.lowercase(Locale.ROOT)
    val host = uri.host?.lowercase(Locale.ROOT)
    if (scheme.isNullOrBlank() || host.isNullOrBlank()) {
      return "[invalid-url]"
    }
    val port = if (uri.port >= 0) ":${uri.port}" else ""
    return "$scheme://$host$port"
  }

  fun headerNames(headers: Map<String, String>): String =
    headers.keys.sorted().joinToString(",").ifEmpty { "none" }

  fun prepareSummary(
    mode: PlaybackMode,
    videoOrigin: String,
    audioOrigin: String?,
    videoHeaders: Map<String, String>,
    audioHeaders: Map<String, String>?,
  ): String =
    buildString {
      append("event=prepare mode=")
      append(mode.name.lowercase(Locale.ROOT))
      append(" video_origin=")
      append(videoOrigin)
      if (audioOrigin != null) {
        append(" audio_origin=")
        append(audioOrigin)
      }
      if (audioHeaders == null) {
        append(" headers=")
        append(headerNames(videoHeaders))
      } else {
        append(" video_headers=")
        append(headerNames(videoHeaders))
        append(" audio_headers=")
        append(headerNames(audioHeaders))
      }
    }

  fun errorSummary(errorClassName: String, errorCodeName: String): String =
    "event=playback_error type=${safeToken(errorClassName)} code=${safeToken(errorCodeName)}"

  private fun safeToken(value: String): String =
    if (
      value.isEmpty() ||
        value.length > 96 ||
        value.any { !(it.isLetterOrDigit() || it == '_' || it == '.' || it == '-') }
    ) {
      "unknown"
    } else {
      value
    }
}
