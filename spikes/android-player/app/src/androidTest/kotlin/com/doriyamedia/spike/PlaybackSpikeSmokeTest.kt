package com.doriyamedia.spike

import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.junit4.createAndroidComposeRule
import androidx.compose.ui.test.onNodeWithContentDescription
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import androidx.compose.ui.test.performScrollTo
import org.junit.Rule
import org.junit.Test

class PlaybackSpikeSmokeTest {
  @get:Rule val composeTestRule = createAndroidComposeRule<MainActivity>()

  @Test
  fun probeSurfaceIsVisible() {
    composeTestRule.onNodeWithText("Playback mechanism probe").assertIsDisplayed()
    composeTestRule.onNodeWithText("Prepare single stream").performScrollTo().assertIsDisplayed()
    composeTestRule
      .onNodeWithContentDescription("Media3 video surface")
      .performScrollTo()
      .assertIsDisplayed()
  }

  @Test
  fun mergeModeExposesSeparateHeadersAndContainerHints() {
    composeTestRule.onNodeWithText("Video + audio merge").performClick()

    composeTestRule.onNodeWithText("Video container MIME").performScrollTo().assertIsDisplayed()
    composeTestRule.onNodeWithText("Video headers").performScrollTo().assertIsDisplayed()
    composeTestRule.onNodeWithText("Audio container MIME").performScrollTo().assertIsDisplayed()
    composeTestRule.onNodeWithText("Audio headers").performScrollTo().assertIsDisplayed()
  }
}
