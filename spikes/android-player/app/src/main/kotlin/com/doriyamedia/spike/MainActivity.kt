package com.doriyamedia.spike

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import com.doriyamedia.spike.ui.PlaybackSpikeScreen
import com.doriyamedia.spike.ui.theme.DoriyaMediaSpikeTheme

class MainActivity : ComponentActivity() {
  override fun onCreate(savedInstanceState: Bundle?) {
    super.onCreate(savedInstanceState)

    enableEdgeToEdge()
    setContent {
      DoriyaMediaSpikeTheme { PlaybackSpikeScreen() }
    }
  }
}
