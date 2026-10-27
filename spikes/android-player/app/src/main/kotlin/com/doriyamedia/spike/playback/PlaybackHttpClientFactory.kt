package com.doriyamedia.spike.playback

import okhttp3.CookieJar
import okhttp3.HttpUrl.Companion.toHttpUrlOrNull
import okhttp3.OkHttpClient

internal object PlaybackHttpClientFactory {
  fun create(stream: StreamRequest): OkHttpClient {
    val initialUrl =
      stream.url.toHttpUrlOrNull()
        ?: throw IllegalArgumentException("Playback URL is not a valid HTTP URL")
    require(initialUrl.scheme == "https") { "Playback URL must use HTTPS" }
    require(initialUrl.username.isEmpty() && initialUrl.password.isEmpty()) {
      "Playback URL must not contain user information"
    }

    return OkHttpClient.Builder()
      .cookieJar(CookieJar.NO_COOKIES)
      .followRedirects(true)
      .followSslRedirects(false)
      .retryOnConnectionFailure(false)
      .addNetworkInterceptor(SameOriginRedirectPolicy(initialUrl))
      .build()
  }
}
