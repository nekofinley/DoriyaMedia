package com.doriyamedia.spike.playback

import androidx.media3.datasource.AssetDataSource
import androidx.media3.datasource.DataSource
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import org.junit.Assert.assertEquals
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class PlaybackMediaSourceFactoryInstrumentedTest {
  @Test
  fun mergeCreatesSeparateDataSourcesFromEachStreamRequest() {
    val testContext = InstrumentationRegistry.getInstrumentation().context
    val seenStreams = mutableListOf<StreamRequest>()
    val factory =
      PlaybackMediaSourceFactory(
        dataSourceFactory = { stream ->
          seenStreams += stream
          DataSource.Factory { AssetDataSource(testContext) }
        },
      )
    val video =
      StreamRequest(
        url = "https://video.example/video.mp4",
        headers =
          mapOf(
            "Referer" to "https://www.example.com/video",
            "User-Agent" to "Video-Agent",
          ),
        containerMimeType = "video/mp4",
      )
    val audio =
      StreamRequest(
        url = "https://audio.example/audio.mp4",
        headers =
          mapOf(
            "Origin" to "https://www.example.com",
            "Accept" to "audio/mp4",
          ),
        containerMimeType = "audio/mp4",
      )

    factory.create(PlaybackRequest.Merge(video = video, audio = audio))

    assertEquals(listOf(video, audio), seenStreams)
  }

  @Test
  fun eachStreamSuppliesItsOwnContainerMimeHint() {
    val video =
      StreamRequest(
        url = "https://video.example/video.mp4",
        headers = emptyMap(),
        containerMimeType = "video/mp4",
      )
    val audio =
      StreamRequest(
        url = "https://audio.example/audio.mp4",
        headers = emptyMap(),
        containerMimeType = "audio/mp4",
      )

    assertEquals("video/mp4", video.toMediaItem().localConfiguration?.mimeType)
    assertEquals("audio/mp4", audio.toMediaItem().localConfiguration?.mimeType)
  }

  @Test
  fun factoryUsesTheMediaItemMapperForBothMergeStreams() {
    val testContext = InstrumentationRegistry.getInstrumentation().context
    val seenMimeTypes = mutableListOf<String?>()
    val video =
      StreamRequest(
        url = "https://video.example/video.mp4",
        headers = emptyMap(),
        containerMimeType = "video/mp4",
      )
    val audio =
      StreamRequest(
        url = "https://audio.example/audio.mp4",
        headers = emptyMap(),
        containerMimeType = "audio/mp4",
      )
    val factory =
      PlaybackMediaSourceFactory(
        dataSourceFactory = { DataSource.Factory { AssetDataSource(testContext) } },
        mediaItemFactory = { stream ->
          stream.toMediaItem().also {
            seenMimeTypes += it.localConfiguration?.mimeType
          }
        },
      )

    factory.create(PlaybackRequest.Merge(video = video, audio = audio))

    assertEquals(listOf("video/mp4", "audio/mp4"), seenMimeTypes)
  }

  @Test
  fun factoryUsesTheMediaItemMapperForASingleStream() {
    val testContext = InstrumentationRegistry.getInstrumentation().context
    val seenMimeTypes = mutableListOf<String?>()
    val stream =
      StreamRequest(
        url = "https://media.example/video.webm",
        headers = emptyMap(),
        containerMimeType = "video/webm",
      )
    val factory =
      PlaybackMediaSourceFactory(
        dataSourceFactory = { DataSource.Factory { AssetDataSource(testContext) } },
        mediaItemFactory = { request ->
          request.toMediaItem().also {
            seenMimeTypes += it.localConfiguration?.mimeType
          }
        },
      )

    factory.create(PlaybackRequest.Single(stream))

    assertEquals(listOf("video/webm"), seenMimeTypes)
  }
}
