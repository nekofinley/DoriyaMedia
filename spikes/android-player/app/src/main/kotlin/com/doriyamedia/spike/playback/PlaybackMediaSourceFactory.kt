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
  private val mediaItemFactory: (StreamRequest) -> MediaItem = StreamRequest::toMediaItem,
) {
  fun create(request: PlaybackRequest): MediaSource =
    when (request) {
      is PlaybackRequest.Single -> {
        DefaultMediaSourceFactory(dataSourceFactory(request.stream))
          .createMediaSource(mediaItemFactory(request.stream))
      }

      is PlaybackRequest.Merge -> {
        val videoSource =
          ProgressiveMediaSource.Factory(dataSourceFactory(request.video))
            .createMediaSource(mediaItemFactory(request.video))
        val audioSource =
          ProgressiveMediaSource.Factory(dataSourceFactory(request.audio))
            .createMediaSource(mediaItemFactory(request.audio))
        MergingMediaSource(videoSource, audioSource)
      }
    }
}

internal fun StreamRequest.toMediaItem(): MediaItem =
  MediaItem.Builder()
    .setUri(url)
    .apply {
      containerMimeType?.let { setMimeType(it) }
    }
    .build()
