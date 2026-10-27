package com.doriyamedia.spike.ui

import android.graphics.Color
import android.view.ViewGroup
import androidx.annotation.OptIn
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.aspectRatio
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawingPadding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.FilterChip
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Slider
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableFloatStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color as ComposeColor
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.LifecycleEventObserver
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.compose.LocalLifecycleOwner
import androidx.lifecycle.repeatOnLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import androidx.media3.common.util.UnstableApi
import androidx.media3.ui.PlayerView
import com.doriyamedia.spike.playback.PlaybackDraft
import com.doriyamedia.spike.playback.PlaybackMode
import com.doriyamedia.spike.playback.PlaybackUiState
import com.doriyamedia.spike.playback.PlaybackViewModel
import com.doriyamedia.spike.playback.ProbePhase
import com.doriyamedia.spike.playback.playPauseActionLabel
import com.doriyamedia.spike.playback.playbackStatusLabel
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import java.util.Locale

@Composable
fun PlaybackSpikeScreen(viewModel: PlaybackViewModel = viewModel()) {
  val state by viewModel.uiState.collectAsStateWithLifecycle()
  val lifecycleOwner = LocalLifecycleOwner.current

  LaunchedEffect(lifecycleOwner, viewModel) {
    lifecycleOwner.lifecycle.repeatOnLifecycle(Lifecycle.State.STARTED) {
      while (isActive) {
        viewModel.refreshProgress()
        delay(500)
      }
    }
  }
  DisposableEffect(lifecycleOwner, viewModel) {
    val observer =
      LifecycleEventObserver { _, event ->
        if (event == Lifecycle.Event.ON_STOP) {
          viewModel.pause()
        }
      }
    lifecycleOwner.lifecycle.addObserver(observer)
    onDispose { lifecycleOwner.lifecycle.removeObserver(observer) }
  }

  Scaffold(
    containerColor = MaterialTheme.colorScheme.background,
    modifier = Modifier.fillMaxSize(),
  ) { innerPadding ->
    PlaybackSpikeContent(
      state = state,
      playerView = { playerView(viewModel) },
      onPrepare = viewModel::prepare,
      onTogglePlayPause = viewModel::togglePlayPause,
      onSeek = viewModel::seekTo,
      modifier = Modifier.padding(innerPadding),
    )
  }
}

@Composable
private fun PlaybackSpikeContent(
  state: PlaybackUiState,
  playerView: @Composable () -> Unit,
  onPrepare: (PlaybackDraft) -> Unit,
  onTogglePlayPause: () -> Unit,
  onSeek: (Long) -> Unit,
  modifier: Modifier = Modifier,
) {
  var mode by remember { mutableStateOf(PlaybackMode.SINGLE) }
  var primaryUrl by remember { mutableStateOf("") }
  var secondaryUrl by remember { mutableStateOf("") }
  var singleHeadersText by remember { mutableStateOf("") }
  var videoHeadersText by remember { mutableStateOf("") }
  var audioHeadersText by remember { mutableStateOf("") }
  var singleContainerMimeType by remember { mutableStateOf("") }
  var videoContainerMimeType by remember { mutableStateOf("video/mp4") }
  var audioContainerMimeType by remember { mutableStateOf("audio/mp4") }
  var scrubPosition by remember { mutableFloatStateOf(0f) }
  var isScrubbing by remember { mutableStateOf(false) }

  LaunchedEffect(state.positionMs, state.durationMs, isScrubbing) {
    if (!isScrubbing) {
      val max = state.durationMs.coerceAtLeast(0)
      scrubPosition = state.positionMs.coerceIn(0, max).toFloat()
    }
  }

  Column(
    modifier =
      modifier
        .fillMaxSize()
        .safeDrawingPadding()
        .imePadding()
        .verticalScroll(rememberScrollState())
        .padding(horizontal = 20.dp, vertical = 18.dp),
    verticalArrangement = Arrangement.spacedBy(16.dp),
  ) {
    ProbeHeader(state.phase)

    Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
      Text(
        text = "Source",
        style = MaterialTheme.typography.titleMedium,
        fontWeight = FontWeight.SemiBold,
      )
      Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
        FilterChip(
          selected = mode == PlaybackMode.SINGLE,
          onClick = { mode = PlaybackMode.SINGLE },
          label = { Text("Single") },
        )
        FilterChip(
          selected = mode == PlaybackMode.MERGE,
          onClick = { mode = PlaybackMode.MERGE },
          label = { Text("Video + audio merge") },
        )
      }

      OutlinedTextField(
        value = primaryUrl,
        onValueChange = { primaryUrl = it },
        modifier = Modifier.fillMaxWidth(),
        label = { Text(if (mode == PlaybackMode.SINGLE) "Media URL" else "Video URL") },
        supportingText = { Text("HTTPS only. Query strings are accepted but never logged.") },
        singleLine = true,
        keyboardOptions = androidx.compose.foundation.text.KeyboardOptions(keyboardType = KeyboardType.Uri),
      )
      if (mode == PlaybackMode.MERGE) {
        OutlinedTextField(
          value = secondaryUrl,
          onValueChange = { secondaryUrl = it },
          modifier = Modifier.fillMaxWidth(),
          label = { Text("Audio URL") },
          supportingText = { Text("Merged locally with Media3 MergingMediaSource.") },
          singleLine = true,
          keyboardOptions =
            androidx.compose.foundation.text.KeyboardOptions(keyboardType = KeyboardType.Uri),
        )
      }
      if (mode == PlaybackMode.SINGLE) {
        OutlinedTextField(
          value = singleContainerMimeType,
          onValueChange = { singleContainerMimeType = it },
          modifier = Modifier.fillMaxWidth(),
          label = { Text("Container MIME (optional)") },
          supportingText = {
            Text("Leave blank to infer from the URL, or use an HTTPS progressive, HLS, or DASH MIME.")
          },
          singleLine = true,
        )
        HeaderField(
          value = singleHeadersText,
          onValueChange = { singleHeadersText = it },
          label = "Stream headers",
        )
      } else {
        OutlinedTextField(
          value = videoContainerMimeType,
          onValueChange = { videoContainerMimeType = it },
          modifier = Modifier.fillMaxWidth(),
          label = { Text("Video container MIME") },
          supportingText = { Text("Supported: video/mp4 or video/webm.") },
          singleLine = true,
        )
        HeaderField(
          value = videoHeadersText,
          onValueChange = { videoHeadersText = it },
          label = "Video headers",
        )
        OutlinedTextField(
          value = audioContainerMimeType,
          onValueChange = { audioContainerMimeType = it },
          modifier = Modifier.fillMaxWidth(),
          label = { Text("Audio container MIME") },
          supportingText = { Text("Supported: audio/mp4 or audio/webm.") },
          singleLine = true,
        )
        HeaderField(
          value = audioHeadersText,
          onValueChange = { audioHeadersText = it },
          label = "Audio headers",
        )
      }
      Button(
        onClick = {
          onPrepare(
            PlaybackDraft(
              mode = mode,
              primaryUrl = primaryUrl,
              secondaryUrl = secondaryUrl,
              primaryHeadersText =
                if (mode == PlaybackMode.SINGLE) singleHeadersText else videoHeadersText,
              secondaryHeadersText = audioHeadersText,
              primaryContainerMimeType =
                if (mode == PlaybackMode.SINGLE) {
                  singleContainerMimeType
                } else {
                  videoContainerMimeType
                },
              secondaryContainerMimeType = audioContainerMimeType,
            ),
          )
        },
        modifier = Modifier.fillMaxWidth(),
      ) {
        Text(
          if (mode == PlaybackMode.SINGLE) {
            "Prepare single stream"
          } else {
            "Prepare merged video + audio"
          },
        )
      }
    }

    PlayerPanel(state = state, playerView = playerView)

    ControlPanel(
      state = state,
      scrubPosition = scrubPosition,
      isScrubbing = isScrubbing,
      onScrub = {
        isScrubbing = true
        scrubPosition = it
      },
      onScrubFinished = {
        onSeek(scrubPosition.toLong())
        isScrubbing = false
      },
      onTogglePlayPause = onTogglePlayPause,
    )

    StatusPanel(state)
  }
}

@Composable
private fun HeaderField(
  value: String,
  onValueChange: (String) -> Unit,
  label: String,
) {
  OutlinedTextField(
    value = value,
    onValueChange = onValueChange,
    modifier = Modifier.fillMaxWidth(),
    label = { Text(label) },
    supportingText = {
      Text(
        "One Name: Value per line. Allowed: Referer, Origin, User-Agent, Accept, Accept-Language.",
      )
    },
    minLines = 3,
    maxLines = 6,
  )
}

@Composable
private fun ProbeHeader(phase: ProbePhase) {
  Box(
    modifier =
      Modifier.fillMaxWidth()
        .clip(RoundedCornerShape(24.dp))
        .background(
          Brush.linearGradient(
            listOf(
              MaterialTheme.colorScheme.primaryContainer,
              MaterialTheme.colorScheme.secondaryContainer,
            ),
          ),
        )
        .padding(20.dp),
  ) {
    Column(verticalArrangement = Arrangement.spacedBy(10.dp)) {
      Row(
        modifier = Modifier.fillMaxWidth(),
        horizontalArrangement = Arrangement.SpaceBetween,
        verticalAlignment = Alignment.CenterVertically,
      ) {
        Text(
          text = "A0 / MEDIA3",
          fontFamily = FontFamily.Monospace,
          fontWeight = FontWeight.Bold,
          color = MaterialTheme.colorScheme.onPrimaryContainer,
        )
        PhaseBadge(phase)
      }
      Text(
        text = "Playback mechanism probe",
        style = MaterialTheme.typography.headlineSmall,
        fontWeight = FontWeight.Bold,
        color = MaterialTheme.colorScheme.onPrimaryContainer,
      )
      Text(
        text =
          "Direct URL and local video/audio merge only. No accounts, library, persistence, " +
            "telemetry, or production fallback policy.",
        style = MaterialTheme.typography.bodyMedium,
        color = MaterialTheme.colorScheme.onPrimaryContainer,
      )
    }
  }
}

@Composable
private fun PhaseBadge(phase: ProbePhase) {
  val isProblem = phase == ProbePhase.ERROR
  val text = phase.displayName()
  Surface(
    shape = RoundedCornerShape(999.dp),
    color =
      if (isProblem) {
        MaterialTheme.colorScheme.errorContainer
      } else {
        MaterialTheme.colorScheme.surface.copy(alpha = 0.82f)
      },
  ) {
    Text(
      text = text,
      modifier = Modifier.padding(horizontal = 10.dp, vertical = 6.dp),
      style = MaterialTheme.typography.labelMedium,
      fontFamily = FontFamily.Monospace,
      color =
        if (isProblem) {
          MaterialTheme.colorScheme.onErrorContainer
        } else {
          MaterialTheme.colorScheme.onSurface
        },
    )
  }
}

@OptIn(UnstableApi::class)
@Composable
private fun playerView(viewModel: PlaybackViewModel): @Composable () -> Unit = {
  val context = LocalContext.current
  AndroidView(
    factory = {
      PlayerView(context).apply {
        player = viewModel.player
        useController = false
        setBackgroundColor(Color.BLACK)
        layoutParams =
          ViewGroup.LayoutParams(
            ViewGroup.LayoutParams.MATCH_PARENT,
            ViewGroup.LayoutParams.MATCH_PARENT,
          )
      }
    },
    update = { it.player = viewModel.player },
    onRelease = { it.player = null },
    modifier = Modifier.fillMaxSize(),
  )
}

@Composable
private fun PlayerPanel(state: PlaybackUiState, playerView: @Composable () -> Unit) {
  Card(
    shape = RoundedCornerShape(20.dp),
    colors = CardDefaults.cardColors(containerColor = ComposeColor(0xFF11191B)),
  ) {
    Box(
      modifier =
        Modifier.fillMaxWidth()
          .aspectRatio(16f / 9f)
          .semantics { contentDescription = "Media3 video surface" }
          .clip(RoundedCornerShape(20.dp))
          .background(ComposeColor(0xFF11191B)),
      contentAlignment = Alignment.Center,
    ) {
      playerView()
      if (state.phase == ProbePhase.PREPARING || state.phase == ProbePhase.BUFFERING) {
        Column(
          horizontalAlignment = Alignment.CenterHorizontally,
          verticalArrangement = Arrangement.spacedBy(10.dp),
        ) {
          CircularProgressIndicator(color = ComposeColor(0xFFE7B66A))
          Text(
            text = state.phase.displayName(),
            color = ComposeColor(0xFFF4EFE4),
            fontFamily = FontFamily.Monospace,
          )
        }
      } else if (state.preparedMode == null) {
        Text(
          text = "NO SOURCE PREPARED",
          color = ComposeColor(0xFF9BAAA8),
          fontFamily = FontFamily.Monospace,
        )
      }
    }
  }
}

@Composable
private fun ControlPanel(
  state: PlaybackUiState,
  scrubPosition: Float,
  isScrubbing: Boolean,
  onScrub: (Float) -> Unit,
  onScrubFinished: () -> Unit,
  onTogglePlayPause: () -> Unit,
) {
  val duration = state.durationMs.coerceAtLeast(0)
  val canControl =
    state.preparedMode != null &&
      state.phase != ProbePhase.IDLE &&
      state.phase != ProbePhase.ERROR
  val shownPosition = if (isScrubbing) scrubPosition.toLong() else state.positionMs

  Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
    Row(
      modifier = Modifier.fillMaxWidth(),
      horizontalArrangement = Arrangement.SpaceBetween,
      verticalAlignment = Alignment.CenterVertically,
    ) {
      Text(
        text = "${formatPosition(shownPosition)} / ${formatPosition(duration)}",
        fontFamily = FontFamily.Monospace,
        style = MaterialTheme.typography.labelLarge,
      )
      Text(
        text = state.playbackStatusLabel(),
        fontFamily = FontFamily.Monospace,
        style = MaterialTheme.typography.labelLarge,
        color =
          if (state.playWhenReady || state.isPlaying) {
            MaterialTheme.colorScheme.primary
          } else {
            MaterialTheme.colorScheme.onSurfaceVariant
          },
      )
    }
    Slider(
      value = scrubPosition.coerceIn(0f, duration.coerceAtLeast(1).toFloat()),
      onValueChange = onScrub,
      onValueChangeFinished = onScrubFinished,
      valueRange = 0f..duration.coerceAtLeast(1).toFloat(),
      enabled = canControl && duration > 0,
    )
    OutlinedButton(
      onClick = onTogglePlayPause,
      enabled = canControl,
      modifier = Modifier.fillMaxWidth(),
    ) {
      Text(
        state.playPauseActionLabel(),
      )
    }
  }
}

@Composable
private fun StatusPanel(state: PlaybackUiState) {
  Card(
    shape = RoundedCornerShape(18.dp),
    colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainer),
  ) {
    Column(
      modifier = Modifier.fillMaxWidth().padding(16.dp),
      verticalArrangement = Arrangement.spacedBy(10.dp),
    ) {
      Text(
        text = "Observer",
        style = MaterialTheme.typography.titleSmall,
        fontWeight = FontWeight.Bold,
      )
      HorizontalDivider()
      StatusRow("Phase", state.phase.displayName())
      StatusRow("Mode", state.preparedMode?.displayName() ?: "not prepared")
      StatusRow("Play intent", if (state.playWhenReady) "play" else "pause")
      StatusRow("Position", formatPosition(state.positionMs))
      StatusRow("Duration", formatPosition(state.durationMs))
      StatusRow("Error code", state.playbackErrorCode ?: "none")
      state.validationError?.let {
        Spacer(Modifier.height(2.dp))
        Text(
          text = it,
          color = MaterialTheme.colorScheme.error,
          style = MaterialTheme.typography.bodyMedium,
        )
      }
    }
  }
}

@Composable
private fun StatusRow(label: String, value: String) {
  Row(
    modifier = Modifier.fillMaxWidth(),
    horizontalArrangement = Arrangement.SpaceBetween,
    verticalAlignment = Alignment.CenterVertically,
  ) {
    Text(text = label, color = MaterialTheme.colorScheme.onSurfaceVariant)
    Text(
      text = value,
      fontFamily = FontFamily.Monospace,
      fontWeight = FontWeight.Medium,
      modifier = Modifier.padding(start = 16.dp),
    )
  }
}

private fun ProbePhase.displayName(): String =
  when (this) {
    ProbePhase.IDLE -> "Idle"
    ProbePhase.PREPARING -> "Preparing"
    ProbePhase.BUFFERING -> "Buffering"
    ProbePhase.READY -> "Ready"
    ProbePhase.ENDED -> "Ended"
    ProbePhase.ERROR -> "Error"
  }

private fun PlaybackMode.displayName(): String =
  when (this) {
    PlaybackMode.SINGLE -> "single"
    PlaybackMode.MERGE -> "video + audio merge"
  }

private fun formatPosition(positionMs: Long): String {
  val totalSeconds = positionMs.coerceAtLeast(0) / 1000
  val hours = totalSeconds / 3600
  val minutes = (totalSeconds % 3600) / 60
  val seconds = totalSeconds % 60
  return if (hours > 0) {
    String.format(Locale.ROOT, "%d:%02d:%02d", hours, minutes, seconds)
  } else {
    String.format(Locale.ROOT, "%02d:%02d", minutes, seconds)
  }
}
