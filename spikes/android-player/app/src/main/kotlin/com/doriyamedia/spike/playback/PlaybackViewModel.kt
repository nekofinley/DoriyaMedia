package com.doriyamedia.spike.playback

import android.app.Application
import android.util.Log
import androidx.annotation.OptIn
import androidx.lifecycle.AndroidViewModel
import androidx.media3.common.PlaybackException
import androidx.media3.common.Player
import androidx.media3.common.util.UnstableApi
import androidx.media3.datasource.DataSource
import androidx.media3.datasource.DefaultDataSource
import androidx.media3.datasource.okhttp.OkHttpDataSource
import androidx.media3.exoplayer.ExoPlayer
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update

enum class ProbePhase {
  IDLE,
  PREPARING,
  BUFFERING,
  READY,
  ENDED,
  ERROR,
}

data class PlaybackUiState(
  val phase: ProbePhase = ProbePhase.IDLE,
  val isPlaying: Boolean = false,
  val playWhenReady: Boolean = false,
  val positionMs: Long = 0,
  val durationMs: Long = 0,
  val preparedMode: PlaybackMode? = null,
  val validationError: String? = null,
  val playbackErrorCode: String? = null,
)

@OptIn(UnstableApi::class)
class PlaybackViewModel(application: Application) : AndroidViewModel(application) {
  private val _uiState = MutableStateFlow(PlaybackUiState())
  val uiState: StateFlow<PlaybackUiState> = _uiState.asStateFlow()
  private val mediaSourceFactory = PlaybackMediaSourceFactory(::dataSourceFactory)

  private val playerListener =
    object : Player.Listener {
      override fun onPlaybackStateChanged(playbackState: Int) {
        val phase =
          when (playbackState) {
            Player.STATE_IDLE -> ProbePhase.IDLE
            Player.STATE_BUFFERING -> ProbePhase.BUFFERING
            Player.STATE_READY -> ProbePhase.READY
            Player.STATE_ENDED -> ProbePhase.ENDED
            else -> ProbePhase.IDLE
          }
        _uiState.update {
          it.copy(
            phase = phase,
            isPlaying = player.isPlaying,
            playWhenReady = player.playWhenReady,
            positionMs = player.currentPosition.coerceAtLeast(0),
            durationMs = safeDuration(),
          )
        }
      }

      override fun onIsPlayingChanged(isPlaying: Boolean) {
        _uiState.update { it.copy(isPlaying = isPlaying) }
      }

      override fun onPlayWhenReadyChanged(playWhenReady: Boolean, reason: Int) {
        _uiState.update { it.copy(playWhenReady = playWhenReady) }
      }

      override fun onPlayerError(error: PlaybackException) {
        val safeCode = error.errorCodeName
        _uiState.update {
          it.copy(
            phase = ProbePhase.ERROR,
            isPlaying = false,
            playbackErrorCode = safeCode,
          )
        }
        Log.w(TAG, SafePlaybackLog.errorSummary(error.javaClass.name, safeCode))
      }
    }

  val player: ExoPlayer =
    ExoPlayer.Builder(application).build().also {
      it.addListener(playerListener)
      it.playWhenReady = false
    }

  fun prepare(draft: PlaybackDraft) {
    when (val result = PlaybackInputValidator.validate(draft)) {
      is ValidationResult.Failure -> {
        _uiState.update {
          it.copy(
            validationError = result.reason,
            playbackErrorCode = null,
          )
        }
      }

      is ValidationResult.Success -> {
        val request = result.value
        val mode = draft.mode
        player.stop()
        player.clearMediaItems()
        _uiState.update {
          it.copy(
            phase = ProbePhase.PREPARING,
            isPlaying = false,
            playWhenReady = true,
            positionMs = 0,
            durationMs = 0,
            preparedMode = mode,
            validationError = null,
            playbackErrorCode = null,
          )
        }

        val source =
          try {
            mediaSourceFactory.create(request)
          } catch (error: RuntimeException) {
            player.stop()
            player.clearMediaItems()
            _uiState.update {
              it.copy(
                phase = ProbePhase.ERROR,
                isPlaying = false,
                playWhenReady = false,
                positionMs = 0,
                durationMs = 0,
                validationError = null,
                playbackErrorCode = "SOURCE_SETUP_FAILED",
              )
            }
            Log.w(TAG, SafePlaybackLog.errorSummary(error.javaClass.name, "SOURCE_SETUP_FAILED"))
            return
          }

        player.setMediaSource(source)
        player.prepare()
        player.playWhenReady = true

        val (videoOrigin, audioOrigin, videoHeaders, audioHeaders) =
          when (request) {
            is PlaybackRequest.Single ->
              PrepareLogFields(
                SafePlaybackLog.urlOrigin(request.stream.url),
                null,
                request.stream.headers,
                null,
              )

            is PlaybackRequest.Merge ->
              PrepareLogFields(
                SafePlaybackLog.urlOrigin(request.video.url),
                SafePlaybackLog.urlOrigin(request.audio.url),
                request.video.headers,
                request.audio.headers,
              )
          }
        Log.i(
          TAG,
          SafePlaybackLog.prepareSummary(
            mode = mode,
            videoOrigin = videoOrigin,
            audioOrigin = audioOrigin,
            videoHeaders = videoHeaders,
            audioHeaders = audioHeaders,
          ),
        )
      }
    }
  }

  fun togglePlayPause() {
    if (player.playbackState == Player.STATE_ENDED) {
      player.seekTo(0)
      player.play()
      return
    }
    if (player.playWhenReady) {
      player.pause()
    } else {
      player.play()
    }
  }

  fun pause() {
    player.pause()
  }

  fun seekTo(positionMs: Long) {
    val duration = safeDuration()
    val upperBound = if (duration > 0) duration else Long.MAX_VALUE
    player.seekTo(positionMs.coerceIn(0, upperBound))
    refreshProgress()
  }

  fun refreshProgress() {
    _uiState.update {
      it.copy(
        isPlaying = player.isPlaying,
        playWhenReady = player.playWhenReady,
        positionMs = player.currentPosition.coerceAtLeast(0),
        durationMs = safeDuration(),
      )
    }
  }

  private fun safeDuration(): Long {
    val duration = player.duration
    return if (duration > 0 && duration != Long.MIN_VALUE) duration else 0
  }

  private fun dataSourceFactory(stream: StreamRequest): DataSource.Factory {
    val httpFactory =
      OkHttpDataSource.Factory(PlaybackHttpClientFactory.create(stream))
        .setDefaultRequestProperties(stream.headers)
    return DefaultDataSource.Factory(getApplication<Application>(), httpFactory)
  }

  override fun onCleared() {
    player.removeListener(playerListener)
    player.release()
    super.onCleared()
  }

  private companion object {
    const val TAG = "DoriyaA0"
  }
}

private data class PrepareLogFields(
  val videoOrigin: String,
  val audioOrigin: String?,
  val videoHeaders: Map<String, String>,
  val audioHeaders: Map<String, String>?,
)
