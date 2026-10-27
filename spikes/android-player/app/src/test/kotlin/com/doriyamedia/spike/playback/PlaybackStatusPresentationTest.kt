package com.doriyamedia.spike.playback

import org.junit.Assert.assertEquals
import org.junit.Test

class PlaybackStatusPresentationTest {
  @Test
  fun bufferingPlayIntentShowsPauseControlAndNotPaused() {
    val state =
      PlaybackUiState(
        phase = ProbePhase.BUFFERING,
        isPlaying = false,
        playWhenReady = true,
      )

    assertEquals("PLAYING (BUFFERING)", state.playbackStatusLabel())
    assertEquals("Pause", state.playPauseActionLabel())
  }

  @Test
  fun explicitlyPausedPlaybackShowsPlayControl() {
    val state =
      PlaybackUiState(
        phase = ProbePhase.READY,
        isPlaying = false,
        playWhenReady = false,
      )

    assertEquals("PAUSED", state.playbackStatusLabel())
    assertEquals("Play", state.playPauseActionLabel())
  }

  @Test
  fun endedPlaybackOffersReplayRegardlessOfIntent() {
    val state =
      PlaybackUiState(
        phase = ProbePhase.ENDED,
        playWhenReady = true,
      )

    assertEquals("ENDED", state.playbackStatusLabel())
    assertEquals("Replay from start", state.playPauseActionLabel())
  }
}
