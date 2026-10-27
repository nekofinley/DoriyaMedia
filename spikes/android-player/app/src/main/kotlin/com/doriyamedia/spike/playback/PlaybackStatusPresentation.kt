package com.doriyamedia.spike.playback

internal fun PlaybackUiState.playbackStatusLabel(): String =
  when {
    phase == ProbePhase.ERROR -> "ERROR"
    phase == ProbePhase.ENDED -> "ENDED"
    playWhenReady && phase == ProbePhase.BUFFERING -> "PLAYING (BUFFERING)"
    playWhenReady -> "PLAYING"
    else -> "PAUSED"
  }

internal fun PlaybackUiState.playPauseActionLabel(): String =
  when {
    phase == ProbePhase.ENDED -> "Replay from start"
    playWhenReady -> "Pause"
    else -> "Play"
  }
