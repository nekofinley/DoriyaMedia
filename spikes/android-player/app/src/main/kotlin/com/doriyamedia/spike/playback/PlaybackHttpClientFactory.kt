package com.doriyamedia.spike.playback

import java.net.Proxy
import okhttp3.CookieJar
import okhttp3.Dns
import okhttp3.HttpUrl.Companion.toHttpUrlOrNull
import okhttp3.OkHttpClient

internal object PlaybackHttpClientFactory {
  private val allowedHeaderNames =
    setOf("Referer", "Origin", "User-Agent", "Accept", "Accept-Language")

  fun create(stream: StreamRequest): OkHttpClient =
    create(stream, OkHttpClient.Builder())

  internal fun create(
    stream: StreamRequest,
    builder: OkHttpClient.Builder,
    dns: Dns = PlaybackDns.publicOnly(),
  ): OkHttpClient {
    val initialUrl =
      stream.url.toHttpUrlOrNull()
        ?: throw IllegalArgumentException("Playback URL is not a valid HTTP URL")
    require(initialUrl.scheme == "https") { "Playback URL must use HTTPS" }
    require(initialUrl.username.isEmpty() && initialUrl.password.isEmpty()) {
      "Playback URL must not contain user information"
    }
    PlaybackDns.requirePublicLiteral(initialUrl.host)
    stream.headers.forEach { (name, value) ->
      require(allowedHeaderNames.any { it.equals(name, ignoreCase = true) }) {
        "Playback header name is not allowlisted"
      }
      require(
        value.isNotEmpty() &&
          value.none { it.isISOControl() || it.code > 0x7E },
      ) {
        "Playback header value contains control or non-ASCII characters"
      }
    }

    return builder
      .dns(dns)
      .proxy(Proxy.NO_PROXY)
      .cookieJar(CookieJar.NO_COOKIES)
      .followRedirects(false)
      .followSslRedirects(false)
      .retryOnConnectionFailure(false)
      .addInterceptor(SameOriginRedirectPolicy(initialUrl))
      .build()
  }
}
