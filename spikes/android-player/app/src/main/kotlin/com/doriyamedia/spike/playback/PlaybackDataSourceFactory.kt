package com.doriyamedia.spike.playback

import android.content.Context
import android.net.Uri
import androidx.annotation.OptIn
import androidx.media3.common.util.UnstableApi
import androidx.media3.datasource.DataSource
import androidx.media3.datasource.DefaultDataSource
import androidx.media3.datasource.DataSpec
import androidx.media3.datasource.HttpDataSource
import androidx.media3.datasource.TransferListener
import androidx.media3.datasource.okhttp.OkHttpDataSource
import java.io.IOException
import okhttp3.OkHttpClient

@OptIn(UnstableApi::class)
internal class PlaybackDataSourceFactory(
  private val context: Context,
  private val httpClientFactory: (StreamRequest) -> OkHttpClient =
    PlaybackHttpClientFactory::create,
) {
  fun createHttp(stream: StreamRequest): HttpDataSource.Factory =
    OkHttpDataSource.Factory(httpClientFactory(stream))
      .setDefaultRequestProperties(stream.headers)

  fun create(stream: StreamRequest): DataSource.Factory {
    return HttpsOnlyDataSourceFactory(
      DefaultDataSource.Factory(context, createHttp(stream)),
    )
  }

  private class HttpsOnlyDataSourceFactory(
    private val delegate: DataSource.Factory,
  ) : DataSource.Factory {
    override fun createDataSource(): DataSource =
      HttpsOnlyDataSource(delegate.createDataSource())
  }

  private class HttpsOnlyDataSource(
    private val delegate: DataSource,
  ) : DataSource {
    override fun addTransferListener(transferListener: TransferListener) {
      delegate.addTransferListener(transferListener)
    }

    override fun open(dataSpec: DataSpec): Long {
      if (
        !dataSpec.uri.scheme.equals("https", ignoreCase = true) ||
          dataSpec.uri.userInfo != null
      ) {
        throw IOException(UNSUPPORTED_SOURCE)
      }
      return delegate.open(dataSpec)
    }

    override fun read(buffer: ByteArray, offset: Int, length: Int): Int =
      delegate.read(buffer, offset, length)

    override fun getUri(): Uri? = delegate.uri

    override fun getResponseHeaders(): Map<String, List<String>> =
      delegate.responseHeaders

    override fun close() {
      delegate.close()
    }
  }

  private companion object {
    const val UNSUPPORTED_SOURCE = "Playback data source requires HTTPS"
  }
}
