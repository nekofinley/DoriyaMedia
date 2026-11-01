package com.doriyamedia.spike.playback

import java.io.IOException
import okhttp3.HttpUrl
import okhttp3.Interceptor
import okhttp3.Response

/**
 * Follows same-origin redirects and rejects every redirect that changes the origin. Automatic
 * redirects are disabled so the next request is validated before DNS or a socket connection.
 */
internal class SameOriginRedirectPolicy(initialUrl: HttpUrl) : Interceptor {
  private val initialOrigin = Origin(initialUrl)

  override fun intercept(chain: Interceptor.Chain): Response {
    var request = chain.request()
    var redirectCount = 0

    while (true) {
      validateSameOrigin(request.url)
      val response = chain.proceed(request)
      if (response.code !in REDIRECT_STATUS_CODES) {
        return response
      }

      val location = response.header("Location") ?: return response
      if (redirectCount >= MAX_REDIRECTS) {
        response.close()
        throw IOException(TOO_MANY_REDIRECTS)
      }

      val nextUrl =
        request.url.resolve(location)
          ?: run {
            response.close()
            throw IOException(MALFORMED_REDIRECT)
          }
      response.close()
      validateSameOrigin(nextUrl)
      request = request.newBuilder().url(nextUrl).build()
      redirectCount += 1
    }
  }

  private fun validateSameOrigin(url: HttpUrl) {
    if (
      Origin(url) != initialOrigin ||
        url.username.isNotEmpty() ||
        url.password.isNotEmpty()
    ) {
      throw IOException(REDIRECT_REJECTED)
    }
  }

  private data class Origin(
    val scheme: String,
    val host: String,
    val port: Int,
  ) {
    constructor(url: HttpUrl) : this(url.scheme, url.host, url.port)
  }

  internal companion object {
    private const val MAX_REDIRECTS = 20
    private val REDIRECT_STATUS_CODES = setOf(300, 301, 302, 303, 307, 308)

    const val REDIRECT_REJECTED = "Cross-origin redirect rejected"
    const val MALFORMED_REDIRECT = "Malformed redirect target rejected"
    const val TOO_MANY_REDIRECTS = "Too many same-origin redirects"
  }
}
