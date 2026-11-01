package com.doriyamedia.spike.playback

import android.content.Context
import androidx.media3.datasource.HttpDataSource
import java.util.concurrent.TimeUnit
import okhttp3.Dns
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.MockWebServer
import okhttp3.mockwebserver.RecordedRequest
import okhttp3.tls.HandshakeCertificates
import okhttp3.tls.HeldCertificate
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull

internal class HttpsPlaybackFixture : AutoCloseable {
  private val heldCertificate =
    HeldCertificate.Builder()
      .commonName("localhost")
      .addSubjectAlternativeName("localhost")
      .build()

  val server: MockWebServer =
    MockWebServer().also {
      val serverCertificates =
        HandshakeCertificates.Builder()
          .heldCertificate(heldCertificate)
          .build()
      it.useHttps(serverCertificates.sslSocketFactory(), false)
      it.start()
    }

  fun url(path: String) = server.url(path)

  override fun close() {
    server.shutdown()
  }

  companion object {
    fun trustedClientBuilder(vararg fixtures: HttpsPlaybackFixture): OkHttpClient.Builder {
      val clientCertificates = HandshakeCertificates.Builder()
      fixtures.forEach { fixture ->
        clientCertificates.addTrustedCertificate(fixture.heldCertificate.certificate)
      }
      val trustedCertificates = clientCertificates.build()
      return OkHttpClient.Builder()
        .sslSocketFactory(
          trustedCertificates.sslSocketFactory(),
          trustedCertificates.trustManager,
        )
        .connectTimeout(5, TimeUnit.SECONDS)
        .readTimeout(5, TimeUnit.SECONDS)
        .writeTimeout(5, TimeUnit.SECONDS)
    }
  }
}

internal fun Context.playbackHttpDataSource(
  stream: StreamRequest,
  vararg fixtures: HttpsPlaybackFixture,
): HttpDataSource {
  val factory =
    PlaybackDataSourceFactory(this) { request ->
      PlaybackHttpClientFactory.create(
        request,
        HttpsPlaybackFixture.trustedClientBuilder(*fixtures),
        Dns.SYSTEM,
      )
    }
  return factory.createHttp(stream).createDataSource()
}

internal fun assertNoSensitiveOrUnknownHeaders(request: RecordedRequest) {
  assertNull(request.getHeader("Cookie"))
  assertNull(request.getHeader("Authorization"))
  assertNull(request.getHeader("X-Source-Token"))
  assertNull(request.getHeader("Content-Type"))
}

internal fun assertVideoHeaders(request: RecordedRequest) {
  assertEquals(TEST_VIDEO_HEADERS["Referer"], request.getHeader("Referer"))
  assertEquals(TEST_VIDEO_HEADERS["User-Agent"], request.getHeader("User-Agent"))
  assertEquals(TEST_VIDEO_HEADERS["Accept"], request.getHeader("Accept"))
  assertEquals(TEST_VIDEO_HEADERS["Accept-Language"], request.getHeader("Accept-Language"))
}

internal fun assertAudioHeaders(request: RecordedRequest) {
  assertEquals(TEST_AUDIO_HEADERS["Origin"], request.getHeader("Origin"))
  assertEquals(TEST_AUDIO_HEADERS["User-Agent"], request.getHeader("User-Agent"))
  assertEquals(TEST_AUDIO_HEADERS["Accept"], request.getHeader("Accept"))
  assertEquals(TEST_AUDIO_HEADERS["Accept-Language"], request.getHeader("Accept-Language"))
}

internal val TEST_VIDEO_HEADERS =
  mapOf(
    "Referer" to "https://www.example.com/video",
    "User-Agent" to "DoriyaMedia-Video/0.1",
    "Accept" to "video/mp4",
    "Accept-Language" to "zh-CN, en",
  )

internal val TEST_AUDIO_HEADERS =
  mapOf(
    "Origin" to "https://www.example.com",
    "User-Agent" to "DoriyaMedia-Audio/0.1",
    "Accept" to "audio/mp4",
    "Accept-Language" to "ja-JP, en",
  )
