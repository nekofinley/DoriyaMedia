package com.doriyamedia.spike.playback

import android.app.Instrumentation
import android.os.SystemClock
import androidx.annotation.OptIn
import androidx.media3.common.MediaItem
import androidx.media3.common.MimeTypes
import androidx.media3.common.PlaybackException
import androidx.media3.common.Player
import androidx.media3.common.util.UnstableApi
import androidx.media3.datasource.AssetDataSource
import androidx.media3.datasource.DataSource
import androidx.media3.exoplayer.DefaultRenderersFactory
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.exoplayer.mediacodec.MediaCodecSelector
import androidx.media3.exoplayer.source.MergingMediaSource
import androidx.media3.exoplayer.source.ProgressiveMediaSource
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger
import java.util.concurrent.atomic.AtomicLong
import java.util.concurrent.atomic.AtomicReference
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith

@OptIn(UnstableApi::class)
@RunWith(AndroidJUnit4::class)
class MergePlaybackInstrumentedTest {
  @Test
  fun separateVideoAndAudioAssetsReachReadyAndAcceptSeek() {
    val instrumentation = InstrumentationRegistry.getInstrumentation()
    val testContext = instrumentation.context
    val targetContext = instrumentation.targetContext
    val ready = CountDownLatch(1)
    val seekDiscontinuity = CountDownLatch(1)
    val failure = AtomicReference<PlaybackException>()
    val seekDiscontinuityPosition = AtomicLong(-1)
    val seekDiscontinuityReason = AtomicInteger(-1)
    var player: ExoPlayer? = null

    instrumentation.runOnMainSync {
      // The API 37 ATD image has a broken goldfish H.264 Codec2 path. Prefer
      // software codecs so this test exercises merging rather than that bug.
      val renderersFactory =
        DefaultRenderersFactory(targetContext)
          .setMediaCodecSelector(MediaCodecSelector.PREFER_SOFTWARE)
          .setEnableDecoderFallback(true)
      val created = ExoPlayer.Builder(targetContext, renderersFactory).build()
      player = created
      created.addListener(
        object : Player.Listener {
          override fun onPlaybackStateChanged(playbackState: Int) {
            if (playbackState == Player.STATE_READY) {
              ready.countDown()
            }
          }

          override fun onPlayerError(error: PlaybackException) {
            failure.set(error)
            ready.countDown()
          }

          override fun onPositionDiscontinuity(
            oldPosition: Player.PositionInfo,
            newPosition: Player.PositionInfo,
            reason: Int,
          ) {
            if (reason == Player.DISCONTINUITY_REASON_SEEK) {
              seekDiscontinuityPosition.set(newPosition.positionMs)
              seekDiscontinuityReason.set(reason)
              seekDiscontinuity.countDown()
            }
          }
        },
      )

      // AndroidTest assets live in the test APK, not the target app package.
      val dataSourceFactory = DataSource.Factory { AssetDataSource(testContext) }
      val videoSource =
        ProgressiveMediaSource.Factory(dataSourceFactory)
          .createMediaSource(
            MediaItem.Builder()
              .setUri("asset:///smoke-video.mp4")
              .setMimeType(MimeTypes.VIDEO_MP4)
              .build(),
          )
      val audioSource =
        ProgressiveMediaSource.Factory(dataSourceFactory)
          .createMediaSource(
            MediaItem.Builder()
              .setUri("asset:///smoke-audio.mp4")
              .setMimeType(MimeTypes.AUDIO_MP4)
              .build(),
          )

      created.setMediaSource(MergingMediaSource(videoSource, audioSource))
      created.prepare()
    }

    val activePlayer = checkNotNull(player)
    try {
      assertTrue("merge did not reach Ready", ready.await(30, TimeUnit.SECONDS))
      failure.get()?.let {
        throw AssertionError("Media3 merge failed with ${it.errorCodeName}")
      }

      var durationMs = 0L
      var mimeTypes = emptyList<String>()
      instrumentation.runOnMainSync {
        assertEquals(Player.STATE_READY, activePlayer.playbackState)
        durationMs = activePlayer.duration
        mimeTypes =
          activePlayer.currentTracks.groups.flatMap { group ->
            (0 until group.length).mapNotNull { index ->
              group.getTrackFormat(index).sampleMimeType
            }
          }
        activePlayer.seekTo(SEEK_POSITION_MS)
      }

      assertTrue(
        "seek discontinuity was not delivered",
        seekDiscontinuity.await(10, TimeUnit.SECONDS),
      )
      assertEquals(
        Player.DISCONTINUITY_REASON_SEEK,
        seekDiscontinuityReason.get(),
      )
      assertTrue(
        "seek discontinuity reported ${seekDiscontinuityPosition.get()}ms",
        seekDiscontinuityPosition.get() in (SEEK_POSITION_MS - 100)..(SEEK_POSITION_MS + 500),
      )
      awaitPlaybackState(
        instrumentation,
        activePlayer,
        Player.STATE_READY,
        failure,
        timeoutMs = 10_000,
      )

      instrumentation.runOnMainSync {
        throwIfFailed(failure)
        activePlayer.play()
      }
      val progressedPosition =
        awaitPositionAfter(
          instrumentation,
          activePlayer,
          minimumPositionMs = SEEK_POSITION_MS + 300,
          failure = failure,
          timeoutMs = 10_000,
        )
      instrumentation.runOnMainSync {
        assertTrue(
          "position only advanced to ${progressedPosition}ms",
          progressedPosition >= SEEK_POSITION_MS + 300,
        )
        assertTrue(activePlayer.isPlaying)
        activePlayer.pause()
      }
      awaitIsPlaying(
        instrumentation,
        activePlayer,
        expected = false,
        failure = failure,
        timeoutMs = 2_000,
      )

      assertTrue("duration was not exposed", durationMs > 0)
      assertTrue("video track missing", mimeTypes.any { it.startsWith("video/") })
      assertTrue("audio track missing", mimeTypes.any { it.startsWith("audio/") })
    } finally {
      instrumentation.runOnMainSync { activePlayer.release() }
    }
  }

  private fun awaitPlaybackState(
    instrumentation: Instrumentation,
    player: ExoPlayer,
    expectedState: Int,
    failure: AtomicReference<PlaybackException>,
    timeoutMs: Long,
  ) {
    val deadline = SystemClock.uptimeMillis() + timeoutMs
    var observedState = Player.STATE_IDLE
    while (SystemClock.uptimeMillis() < deadline) {
      throwIfFailed(failure)
      instrumentation.runOnMainSync { observedState = player.playbackState }
      if (observedState == expectedState) {
        return
      }
      SystemClock.sleep(POLL_INTERVAL_MS)
    }
    throwIfFailed(failure)
    throw AssertionError("playback state did not reach $expectedState; observed $observedState")
  }

  private fun awaitPositionAfter(
    instrumentation: Instrumentation,
    player: ExoPlayer,
    minimumPositionMs: Long,
    failure: AtomicReference<PlaybackException>,
    timeoutMs: Long,
  ): Long {
    val deadline = SystemClock.uptimeMillis() + timeoutMs
    var observedPositionMs = 0L
    while (SystemClock.uptimeMillis() < deadline) {
      throwIfFailed(failure)
      instrumentation.runOnMainSync { observedPositionMs = player.currentPosition }
      if (observedPositionMs >= minimumPositionMs) {
        return observedPositionMs
      }
      SystemClock.sleep(POLL_INTERVAL_MS)
    }
    throwIfFailed(failure)
    throw AssertionError(
      "position did not advance to ${minimumPositionMs}ms; observed ${observedPositionMs}ms",
    )
  }

  private fun awaitIsPlaying(
    instrumentation: Instrumentation,
    player: ExoPlayer,
    expected: Boolean,
    failure: AtomicReference<PlaybackException>,
    timeoutMs: Long,
  ) {
    val deadline = SystemClock.uptimeMillis() + timeoutMs
    var observed = !expected
    while (SystemClock.uptimeMillis() < deadline) {
      throwIfFailed(failure)
      instrumentation.runOnMainSync { observed = player.isPlaying }
      if (observed == expected) {
        return
      }
      SystemClock.sleep(POLL_INTERVAL_MS)
    }
    throwIfFailed(failure)
    throw AssertionError("isPlaying did not become $expected")
  }

  private fun throwIfFailed(failure: AtomicReference<PlaybackException>) {
    failure.get()?.let {
      throw AssertionError("Media3 merge failed with ${it.errorCodeName}")
    }
  }

  private companion object {
    const val SEEK_POSITION_MS = 1_500L
    const val POLL_INTERVAL_MS = 25L
  }
}
