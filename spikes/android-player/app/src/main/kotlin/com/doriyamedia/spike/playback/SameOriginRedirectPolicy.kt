package com.doriyamedia.spike.playback

import java.io.IOException
import okhttp3.HttpUrl
import okhttp3.Interceptor
import okhttp3.Response

/**
 * Rejects every redirect that changes the origin. OkHttp network interceptors run for each
 * redirected network request, so no allowlisted header can reach a different host or port.
 */
internal class SameOriginRedirectPolicy(initialUrl: HttpUrl) : Interceptor {
  private val initialOrigin = Origin(initialUrl)

  override fun intercept(chain: Interceptor.Chain): Response {
    val request = chain.request()
    val requestOrigin = Origin(request.url)
    if (
      requestOrigin != initialOrigin ||
        request.url.username.isNotEmpty() ||
        request.url.password.isNotEmpty()
    ) {
      throw IOException(REDIRECT_REJECTED)
    }
    return chain.proceed(request)
  }

  private data class Origin(
    val scheme: String,
    val host: String,
    val port: Int,
  ) {
    constructor(url: HttpUrl) : this(url.scheme, url.host, url.port)
  }

  internal companion object {
    const val REDIRECT_REJECTED = "Cross-origin redirect rejected"
  }
}
