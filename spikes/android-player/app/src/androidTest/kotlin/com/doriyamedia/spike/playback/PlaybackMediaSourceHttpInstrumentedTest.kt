package com.doriyamedia.spike.playback

import android.app.Instrumentation
import androidx.annotation.OptIn
import androidx.media3.common.PlaybackException
import androidx.media3.common.Player
import androidx.media3.common.util.UnstableApi
import androidx.media3.exoplayer.DefaultRenderersFactory
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.exoplayer.mediacodec.MediaCodecSelector
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicReference
import okhttp3.Dns
import okhttp3.mockwebserver.Dispatcher
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.RecordedRequest
import okio.Buffer
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith

@OptIn(UnstableApi::class)
@RunWith(AndroidJUnit4::class)
class PlaybackMediaSourceHttpInstrumentedTest {
  @Test
  fun mergePlaybackUsesIndependentHeadersForEachRequest() {
    HttpsPlaybackFixture().use { fixture ->
      val instrumentation = InstrumentationRegistry.getInstrumentation()
      val testContext = instrumentation.context
      val targetContext = instrumentation.targetContext
      val videoBytes = testContext.assets.open("smoke-video.mp4").use { it.readBytes() }
      val audioBytes = testContext.assets.open("smoke-audio.mp4").use { it.readBytes() }
      fixture.server.dispatcher =
        MediaRangeDispatcher(
          mapOf(
            "/video.bin" to MediaBody(videoBytes, "video/mp4"),
            "/audio.bin" to MediaBody(audioBytes, "audio/mp4"),
          ),
        )
      val video =
        StreamRequest(
          url = fixture.url("/video.bin").toString(),
          headers = TEST_VIDEO_HEADERS,
          containerMimeType = "video/mp4",
        )
      val audio =
        StreamRequest(
          url = fixture.url("/audio.bin").toString(),
          headers = TEST_AUDIO_HEADERS,
          containerMimeType = "audio/mp4",
        )
      val dataSourceFactory =
        PlaybackDataSourceFactory(targetContext) { stream ->
          PlaybackHttpClientFactory.create(
            stream,
            HttpsPlaybackFixture.trustedClientBuilder(fixture),
            Dns.SYSTEM,
          )
        }
      val mediaSource =
        PlaybackMediaSourceFactory(dataSourceFactory::create)
          .create(PlaybackRequest.Merge(video = video, audio = audio))
      val ready = CountDownLatch(1)
      val failure = AtomicReference<PlaybackException>()
      var createdPlayer: ExoPlayer? = null

      instrumentation.runOnMainSync {
        val renderersFactory =
          DefaultRenderersFactory(targetContext)
            .setMediaCodecSelector(MediaCodecSelector.PREFER_SOFTWARE)
            .setEnableDecoderFallback(true)
        val player = ExoPlayer.Builder(targetContext, renderersFactory).build()
        createdPlayer = player
        player.addListener(
          object : Player.Listener {
            override fun onPlaybackStateChanged(playbackState: Int) {
              if (playbackState == Player.STATE_READY) {
                ready.countDown()
              }
            }

            override fun onPlayerError(error: PlaybackException) {
              failure.set(error)
              ready.countDown()
            }
          },
        )
        player.setMediaSource(mediaSource)
        player.prepare()
      }

      val player = checkNotNull(createdPlayer)
      try {
        assertTrue("merge playback did not reach Ready", ready.await(30, TimeUnit.SECONDS))
        failure.get()?.let {
          throw AssertionError("Media3 merge failed with ${it.errorCodeName}")
        }

        var trackMimeTypes = emptyList<String>()
        instrumentation.runOnMainSync {
          assertEquals(Player.STATE_READY, player.playbackState)
          trackMimeTypes =
            player.currentTracks.groups.flatMap { group ->
              (0 until group.length).mapNotNull { index ->
                group.getTrackFormat(index).sampleMimeType
              }
            }
        }
        assertTrue(
          "video track missing from $trackMimeTypes",
          trackMimeTypes.any { it.startsWith("video/") },
        )
        assertTrue(
          "audio track missing from $trackMimeTypes",
          trackMimeTypes.any { it.startsWith("audio/") },
        )

        val requests = drainRequests(fixture)
        val videoRequests = requests.filter { it.encodedPath() == "/video.bin" }
        val audioRequests = requests.filter { it.encodedPath() == "/audio.bin" }
        assertTrue("video URL was not requested", videoRequests.isNotEmpty())
        assertTrue("audio URL was not requested", audioRequests.isNotEmpty())

        videoRequests.forEach { request ->
          assertVideoHeaders(request)
          assertNull(request.getHeader("Origin"))
          assertNoSensitiveOrUnknownHeaders(request)
        }
        audioRequests.forEach { request ->
          assertAudioHeaders(request)
          assertNull(request.getHeader("Referer"))
          assertNoSensitiveOrUnknownHeaders(request)
        }
      } finally {
        instrumentation.runOnMainSync { player.release() }
      }
    }
  }

  private fun drainRequests(fixture: HttpsPlaybackFixture): List<RecordedRequest> {
    val requests = mutableListOf<RecordedRequest>()
    while (true) {
      val request = fixture.server.takeRequest(250, TimeUnit.MILLISECONDS) ?: break
      requests += request
    }
    return requests
  }

  private fun RecordedRequest.encodedPath(): String =
    requestUrl?.encodedPath ?: checkNotNull(path).substringBefore('?')

  private data class MediaBody(
    val bytes: ByteArray,
    val contentType: String,
  )

  private class MediaRangeDispatcher(
    private val mediaByPath: Map<String, MediaBody>,
  ) : Dispatcher() {
    override fun dispatch(request: RecordedRequest): MockResponse {
      val media = mediaByPath[request.requestUrl?.encodedPath] ?: return notFound()
      if (request.method == "HEAD") {
        return metadataResponse(media)
      }
      val range = request.getHeader("Range") ?: return fullResponse(media)
      val bounds = parseRange(range, media.bytes.size) ?: return notSatisfiable(media.bytes.size)
      val start = bounds.first
      val end = bounds.last
      val body = media.bytes.copyOfRange(start, end + 1)
      return MockResponse()
        .setResponseCode(206)
        .setHeader("Accept-Ranges", "bytes")
        .setHeader("Content-Type", media.contentType)
        .setHeader("Content-Range", "bytes $start-$end/${media.bytes.size}")
        .setBody(Buffer().write(body))
    }

    private fun fullResponse(media: MediaBody): MockResponse =
      MockResponse()
        .setResponseCode(200)
        .setHeader("Accept-Ranges", "bytes")
        .setHeader("Content-Type", media.contentType)
        .setBody(Buffer().write(media.bytes))

    private fun metadataResponse(media: MediaBody): MockResponse =
      MockResponse()
        .setResponseCode(200)
        .setHeader("Accept-Ranges", "bytes")
        .setHeader("Content-Type", media.contentType)
        .setHeader("Content-Length", media.bytes.size.toString())

    private fun notSatisfiable(size: Int): MockResponse =
      MockResponse()
        .setResponseCode(416)
        .setHeader("Content-Range", "bytes */$size")

    private fun notFound(): MockResponse =
      MockResponse().setResponseCode(404).setBody("unknown media path")

    private fun parseRange(header: String, size: Int): IntRange? {
      if (!header.startsWith("bytes=") || header.contains(',')) {
        return null
      }
      val value = header.removePrefix("bytes=")
      val separator = value.indexOf('-')
      if (separator < 0) {
        return null
      }
      val rawStart = value.substring(0, separator)
      val rawEnd = value.substring(separator + 1)
      if (rawStart.isEmpty()) {
        val suffixLength = rawEnd.toIntOrNull() ?: return null
        if (suffixLength <= 0 || size == 0) {
          return null
        }
        val start = (size - suffixLength).coerceAtLeast(0)
        return start until size
      }

      val start = rawStart.toIntOrNull() ?: return null
      if (start < 0 || start >= size) {
        return null
      }
      val requestedEnd = rawEnd.toIntOrNull() ?: (size - 1)
      val end = requestedEnd.coerceAtMost(size - 1)
      if (end < start) {
        return null
      }
      return start..end
    }
  }
}
