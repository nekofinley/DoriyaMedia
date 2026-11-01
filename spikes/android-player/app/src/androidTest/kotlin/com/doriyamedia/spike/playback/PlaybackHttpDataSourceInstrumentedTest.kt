package com.doriyamedia.spike.playback

import android.content.Context
import android.net.Uri
import androidx.media3.datasource.DataSpec
import androidx.media3.datasource.HttpDataSource
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import java.io.ByteArrayOutputStream
import java.io.IOException
import java.util.concurrent.TimeUnit
import okhttp3.Cookie
import okhttp3.CookieJar
import okhttp3.Dns
import okhttp3.HttpUrl
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.RecordedRequest
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertSame
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class PlaybackHttpDataSourceInstrumentedTest {
  private val context: Context =
    InstrumentationRegistry.getInstrumentation().context

  @Test
  fun getRequestsCarryIndependentPerStreamAllowlistHeaders() {
    HttpsPlaybackFixture().use { fixture ->
      fixture.server.enqueue(
        MockResponse()
          .setResponseCode(200)
          .setHeader("Content-Type", "video/mp4")
          .setBody("vide"),
      )
      fixture.server.enqueue(
        MockResponse()
          .setResponseCode(200)
          .setHeader("Content-Type", "audio/mp4")
          .setBody("aud"),
      )
      val video = stream(fixture, "/video.bin", TEST_VIDEO_HEADERS)
      val audio = stream(fixture, "/audio.bin", TEST_AUDIO_HEADERS)

      val videoResponse = openAndRead(fixture, video)
      val audioResponse = openAndRead(fixture, audio)

      assertArrayEquals("vide".toByteArray(), videoResponse.body)
      assertArrayEquals("aud".toByteArray(), audioResponse.body)
      assertEquals("video/mp4", videoResponse.contentType)
      assertEquals("audio/mp4", audioResponse.contentType)

      val videoRequest = requireRequest(fixture)
      val audioRequest = requireRequest(fixture)
      assertEquals("/video.bin", videoRequest.path)
      assertEquals("GET", videoRequest.method)
      assertEquals("/audio.bin", audioRequest.path)
      assertEquals("GET", audioRequest.method)
      assertVideoHeaders(videoRequest)
      assertAudioHeaders(audioRequest)
      assertNull(videoRequest.getHeader("Origin"))
      assertNull(audioRequest.getHeader("Referer"))
      assertNoSensitiveOrUnknownHeaders(videoRequest)
      assertNoSensitiveOrUnknownHeaders(audioRequest)
    }
  }

  @Test
  fun headRequestCarriesHeadersWithoutARangeAndExposesResponseMetadata() {
    HttpsPlaybackFixture().use { fixture ->
      fixture.server.enqueue(
        MockResponse()
          .setResponseCode(200)
          .setHeader("Content-Type", "video/mp4")
          .setHeader("Content-Length", "4"),
      )
      val stream = stream(fixture, "/metadata.bin", TEST_VIDEO_HEADERS)
      val dataSource = context.playbackHttpDataSource(stream, fixture)

      try {
        val opened =
          dataSource.open(
            DataSpec.Builder()
              .setUri(stream.url)
              .setHttpMethod(DataSpec.HTTP_METHOD_HEAD)
              .build(),
          )
        assertEquals(4L, opened)
        assertEquals(200, dataSource.responseCode)
        assertEquals(
          listOf("video/mp4"),
          dataSource.responseHeaders["Content-Type"],
        )
      } finally {
        dataSource.close()
      }

      val request = requireRequest(fixture)
      assertEquals("HEAD", request.method)
      assertNull(request.getHeader("Range"))
      assertVideoHeaders(request)
      assertNoSensitiveOrUnknownHeaders(request)
    }
  }

  @Test
  fun range206And416ExposeContentRangeAndSourceBytes() {
    HttpsPlaybackFixture().use { fixture ->
      fixture.server.enqueue(
        MockResponse()
          .setResponseCode(206)
          .setHeader("Content-Type", "video/mp4")
          .setHeader("Content-Range", "bytes 2-5/6")
          .setBody("cdef"),
      )
      fixture.server.enqueue(
        MockResponse()
          .setResponseCode(416)
          .setHeader("Content-Range", "bytes */6"),
      )
      val stream = stream(fixture, "/range.bin", TEST_VIDEO_HEADERS)

      val partial = openAndRead(fixture, stream, position = 2, length = 4)
      assertEquals(206, partial.responseCode)
      assertEquals("bytes 2-5/6", partial.contentRange)
      assertArrayEquals("cdef".toByteArray(), partial.body)

      val end = openAndRead(fixture, stream, position = 6, length = -1)
      assertEquals(416, end.responseCode)
      assertEquals("bytes */6", end.contentRange)
      assertArrayEquals(ByteArray(0), end.body)

      val partialRequest = requireRequest(fixture)
      val endRequest = requireRequest(fixture)
      assertEquals("bytes=2-5", partialRequest.getHeader("Range"))
      assertEquals("bytes=6-", endRequest.getHeader("Range"))
      assertVideoHeaders(partialRequest)
      assertVideoHeaders(endRequest)
      assertNoSensitiveOrUnknownHeaders(partialRequest)
      assertNoSensitiveOrUnknownHeaders(endRequest)
    }
  }

  @Test
  fun sameOriginRedirectRetainsHeadersWithoutReplayingServerCookies() {
    HttpsPlaybackFixture().use { fixture ->
      fixture.server.enqueue(
        MockResponse()
          .setResponseCode(302)
          .setHeader("Location", "/final.bin")
          .setHeader("Set-Cookie", "session=must-not-leak; Secure; Path=/"),
      )
      fixture.server.enqueue(
        MockResponse()
          .setResponseCode(206)
          .setHeader("Content-Type", "video/mp4")
          .setHeader("Content-Range", "bytes 0-3/4")
          .setBody("data"),
      )
      val stream = stream(fixture, "/redirect.bin", TEST_VIDEO_HEADERS)

      val response = openAndRead(fixture, stream)
      assertEquals(206, response.responseCode)
      assertArrayEquals("data".toByteArray(), response.body)

      val initialRequest = requireRequest(fixture)
      val redirectedRequest = requireRequest(fixture)
      assertEquals("/redirect.bin", initialRequest.path)
      assertEquals("/final.bin", redirectedRequest.path)
      assertVideoHeaders(initialRequest)
      assertVideoHeaders(redirectedRequest)
      assertNoSensitiveOrUnknownHeaders(initialRequest)
      assertNoSensitiveOrUnknownHeaders(redirectedRequest)
    }
  }

  @Test
  fun factoryOverridesAnInjectedCookieJarWithNoCookies() {
    HttpsPlaybackFixture().use { fixture ->
      val stream = stream(fixture, "/cookie.bin", TEST_VIDEO_HEADERS)
      val storingJar =
        object : CookieJar {
          override fun saveFromResponse(url: HttpUrl, cookies: List<Cookie>) = Unit

          override fun loadForRequest(url: HttpUrl): List<Cookie> = emptyList()
        }

      val client =
        PlaybackHttpClientFactory.create(
          stream,
          HttpsPlaybackFixture.trustedClientBuilder(fixture).cookieJar(storingJar),
          Dns.SYSTEM,
        )

      assertSame(CookieJar.NO_COOKIES, client.cookieJar)
    }
  }

  @Test
  fun crossOriginRedirectIsRejectedBeforeTargetReceivesRequest() {
    HttpsPlaybackFixture().use { origin ->
      HttpsPlaybackFixture().use { target ->
        origin.server.enqueue(
          MockResponse()
            .setResponseCode(302)
            .setHeader("Location", target.url("/sink.bin"))
            .setHeader("Set-Cookie", "session=must-not-leak; Secure; Path=/"),
        )
        target.server.enqueue(MockResponse().setResponseCode(200).setBody("unexpected"))
        val stream = stream(origin, "/start.bin", TEST_VIDEO_HEADERS)
        val dataSource = context.playbackHttpDataSource(stream, origin, target)

        val failure =
          assertThrows(HttpDataSource.HttpDataSourceException::class.java) {
            try {
              dataSource.open(DataSpec.Builder().setUri(stream.url).build())
            } finally {
              dataSource.close()
            }
          }

        assertTrue(causeMessages(failure).any { it.contains(REDIRECT_REJECTED) })
        assertEquals(1, origin.server.requestCount)
        assertEquals(0, target.server.requestCount)
        val initialRequest = requireRequest(origin)
        assertVideoHeaders(initialRequest)
        assertNoSensitiveOrUnknownHeaders(initialRequest)
      }
    }
  }

  @Test
  fun strictDataSourceRejectsNonHttpsAndUserInfoSegmentUris() {
    val targetContext = InstrumentationRegistry.getInstrumentation().targetContext
    val stream =
      StreamRequest(
        url = "https://media.example/video.m3u8",
        headers = emptyMap(),
        containerMimeType = "application/vnd.apple.mpegurl",
      )
    val rejectedUris =
      listOf(
        "file:///data/local/tmp/must-not-open",
        "content://com.example.private/secret",
        "udp://127.0.0.1:9999",
        "data:text/plain,secret",
        "https://user:secret@media.example/video.bin",
      )

    rejectedUris.forEach { rawUri ->
      HttpsPlaybackFixture().use { fixture ->
        val dataSource =
          PlaybackDataSourceFactory(targetContext) { request ->
            PlaybackHttpClientFactory.create(
              request,
              HttpsPlaybackFixture.trustedClientBuilder(fixture),
              Dns.SYSTEM,
            )
          }.create(stream).createDataSource()
        try {
          val failure =
            assertThrows(IOException::class.java) {
              dataSource.open(DataSpec.Builder().setUri(Uri.parse(rawUri)).build())
            }
          assertEquals("Playback data source requires HTTPS", failure.message)
        } finally {
          dataSource.close()
        }
        assertEquals(0, fixture.server.requestCount)
      }
    }
  }

  private fun openAndRead(
    fixture: HttpsPlaybackFixture,
    stream: StreamRequest,
    position: Long = 0,
    length: Long = -1,
  ): SourceObservation {
    val dataSource = context.playbackHttpDataSource(stream, fixture)
    try {
      dataSource.open(
        DataSpec.Builder()
          .setUri(stream.url)
          .setPosition(position)
          .setLength(length)
          .build(),
      )
      val output = ByteArrayOutputStream()
      val buffer = ByteArray(64)
      while (true) {
        val count = dataSource.read(buffer, 0, buffer.size)
        if (count == -1) {
          break
        }
        output.write(buffer, 0, count)
      }
      return SourceObservation(
        responseCode = dataSource.responseCode,
        contentType = dataSource.responseHeaders["Content-Type"]?.singleOrNull(),
        contentRange = dataSource.responseHeaders["Content-Range"]?.singleOrNull(),
        body = output.toByteArray(),
      )
    } finally {
      dataSource.close()
    }
  }

  private fun stream(
    fixture: HttpsPlaybackFixture,
    path: String,
    headers: Map<String, String>,
  ) =
    StreamRequest(
      url = fixture.url(path).toString(),
      headers = headers,
      containerMimeType = null,
    )

  private fun requireRequest(fixture: HttpsPlaybackFixture): RecordedRequest =
    checkNotNull(fixture.server.takeRequest(2, TimeUnit.SECONDS)) {
      "Expected request was not received"
    }

  private fun causeMessages(failure: Throwable): List<String> =
    generateSequence(failure) { it.cause }.mapNotNull { it.message }.toList()

  private data class SourceObservation(
    val responseCode: Int,
    val contentType: String?,
    val contentRange: String?,
    val body: ByteArray,
  )

  private companion object {
    const val REDIRECT_REJECTED = SameOriginRedirectPolicy.REDIRECT_REJECTED
  }
}
