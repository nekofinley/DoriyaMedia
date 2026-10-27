package com.doriyamedia.spike.playback

import androidx.annotation.OptIn
import androidx.media3.common.MediaItem
import androidx.media3.common.util.UnstableApi
import androidx.media3.datasource.DataSource
import androidx.media3.exoplayer.source.DefaultMediaSourceFactory
import androidx.media3.exoplayer.source.MediaSource
import androidx.media3.exoplayer.source.MergingMediaSource
import androidx.media3.exoplayer.source.ProgressiveMediaSource

@OptIn(UnstableApi::class)
internal class PlaybackMediaSourceFactory(
  private val dataSourceFactory: (StreamRequest) -> DataSource.Factory,
) {
  fun create(request: PlaybackRequest): MediaSource =
    when (request) {
      is PlaybackRequest.Single -> {
        DefaultMediaSourceFactory(dataSourceFactory(request.stream))
          .createMediaSource(request.stream.toMediaItem())
      }

      is PlaybackRequest.Merge -> {
        val videoSource =
          ProgressiveMediaSource.Factory(dataSourceFactory(request.video))
            .createMediaSource(request.video.toMediaItem())
        val audioSource =
          ProgressiveMediaSource.Factory(dataSourceFactory(request.audio))
            .createMediaSource(request.audio.toMediaItem())
        MergingMediaSource(videoSource, audioSource)
      }
    }

  private fun StreamRequest.toMediaItem(): MediaItem =
    MediaItem.Builder()
      .setUri(url)
      .apply {
        containerMimeType?.let { setMimeType(it) }
      }
      .build()
}
