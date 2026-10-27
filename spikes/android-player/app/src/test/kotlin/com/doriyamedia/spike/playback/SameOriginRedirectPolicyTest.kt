package com.doriyamedia.spike.playback

import java.io.IOException
import java.util.concurrent.TimeUnit
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test

class SameOriginRedirectPolicyTest {
  private lateinit var server: MockWebServer

  @Before
  fun setUp() {
    server = MockWebServer()
    server.start()
  }

  @After
  fun tearDown() {
    server.shutdown()
  }

  @Test
  fun sameOriginRedirectRetainsAllowlistedHeaders() {
    server.enqueue(
      MockResponse()
        .setResponseCode(302)
        .setHeader("Location", "/final?token=redirect"),
    )
    server.enqueue(MockResponse().setResponseCode(200).setBody("ok"))

    val initialUrl = server.url("/start?token=initial")
    val request =
      Request.Builder()
        .url(initialUrl)
        .header("Referer", "https://www.example.com/watch")
        .header("Origin", "https://www.example.com")
        .header("User-Agent", "DoriyaMedia/0.1")
        .header("Accept", "video/mp4")
        .header("Accept-Language", "zh-CN")
        .build()

    client(initialUrl.toString()).newCall(request).execute().use {
      assertEquals(200, it.code)
    }

    val initialRequest = server.takeRequest(2, TimeUnit.SECONDS)
    val redirectedRequest = server.takeRequest(2, TimeUnit.SECONDS)
    assertEquals("/start?token=initial", initialRequest?.path)
    assertEquals("/final?token=redirect", redirectedRequest?.path)
    assertAllowlistedHeaders(redirectedRequest?.getHeader("Referer"), redirectedRequest)
  }

  @Test
  fun crossOriginRedirectIsRejectedBeforeTargetReceivesARequest() {
    val target = MockWebServer()
    target.start()
    try {
      target.enqueue(MockResponse().setResponseCode(200).setBody("must not be reached"))
      server.enqueue(
        MockResponse()
          .setResponseCode(302)
          .setHeader("Location", target.url("/sink")),
      )
      val initialUrl = server.url("/start")
      val request =
        Request.Builder()
          .url(initialUrl)
          .header("Referer", "https://private.example/watch")
          .header("Origin", "https://private.example")
          .header("User-Agent", "DoriyaMedia/0.1")
          .header("Accept", "video/mp4")
          .header("Accept-Language", "zh-CN")
          .build()

      val failure =
        assertThrows(IOException::class.java) {
          client(initialUrl.toString()).newCall(request).execute().close()
        }

      assertEquals(SameOriginRedirectPolicy.REDIRECT_REJECTED, failure.message)
      assertEquals(1, server.requestCount)
      assertEquals(0, target.requestCount)
    } finally {
      target.shutdown()
    }
  }

  @Test
  fun httpAndCredentialBearingInitialUrlsAreRejected() {
    val http =
      assertThrows(IllegalArgumentException::class.java) {
        PlaybackHttpClientFactory.create(
          StreamRequest(
            url = "http://media.example/video.mp4",
            headers = emptyMap(),
            containerMimeType = null,
          ),
        )
      }
    assertEquals("Playback URL must use HTTPS", http.message)

    val credentials =
      assertThrows(IllegalArgumentException::class.java) {
        PlaybackHttpClientFactory.create(
          StreamRequest(
            url = "https://user:secret@example.com/video.mp4",
            headers = emptyMap(),
            containerMimeType = null,
          ),
        )
      }
    assertEquals("Playback URL must not contain user information", credentials.message)
  }

  private fun client(initialUrl: String): OkHttpClient =
    OkHttpClient.Builder()
      .followRedirects(true)
      .followSslRedirects(false)
      .retryOnConnectionFailure(false)
      .addNetworkInterceptor(
        SameOriginRedirectPolicy(checkNotNull(initialUrl.toHttpUrl())),
      )
      .build()

  private fun assertAllowlistedHeaders(
    referer: String?,
    request: okhttp3.mockwebserver.RecordedRequest?,
  ) {
    assertEquals("https://www.example.com/watch", referer)
    assertEquals("https://www.example.com", request?.getHeader("Origin"))
    assertEquals("DoriyaMedia/0.1", request?.getHeader("User-Agent"))
    assertEquals("video/mp4", request?.getHeader("Accept"))
    assertEquals("zh-CN", request?.getHeader("Accept-Language"))
    assertTrue(request?.getHeader("Authorization").isNullOrEmpty())
    assertNull(request?.getHeader("X-Source-Token"))
  }
}
