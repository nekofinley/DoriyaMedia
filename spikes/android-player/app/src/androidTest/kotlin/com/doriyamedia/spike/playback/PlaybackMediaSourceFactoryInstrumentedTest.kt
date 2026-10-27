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
      PlaybackMediaSourceFactory { stream ->
        seenStreams += stream
        DataSource.Factory { AssetDataSource(testContext) }
      }
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
}
