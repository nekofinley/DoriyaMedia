package com.doriyamedia.spike.playback

import java.net.InetAddress
import java.net.Proxy
import java.net.UnknownHostException
import okhttp3.Dns
import okhttp3.OkHttpClient
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertThrows
import org.junit.Test

class PlaybackDnsTest {
  @Test
  fun acceptsOnlyPublicAddressResults() {
    val expected =
      listOf(
        InetAddress.getByName("8.8.8.8"),
        InetAddress.getByName("2001:4860:4860::8888"),
      )
    val resolver = PlaybackDns.publicOnly(dnsReturning(expected))

    assertEquals(expected, resolver.lookup("media.example"))
  }

  @Test
  fun rejectsAnyPrivateOrSpecialAddressResult() {
    val disallowed =
      listOf(
        "0.0.0.0",
        "10.0.0.1",
        "100.64.0.1",
        "127.0.0.1",
        "169.254.1.1",
        "172.16.0.1",
        "192.0.0.1",
        "192.0.2.1",
        "192.88.99.1",
        "192.168.1.1",
        "198.18.0.1",
        "198.51.100.1",
        "203.0.113.1",
        "224.0.0.1",
        "255.255.255.255",
        "::",
        "::1",
        "fc00::1",
        "fe80::1",
        "ff02::1",
        "64:ff9b::7f00:1",
        "2001::1",
        "2001:db8::1",
        "2001:1000::1",
        "2002:7f00:1::",
        "24ff::1",
        "3fff::1",
      )

    disallowed.forEach { rawAddress ->
      val address = InetAddress.getByName(rawAddress)
      val resolver = PlaybackDns.publicOnly(dnsReturning(listOf(address)))
      val failure =
        assertThrows(UnknownHostException::class.java) {
          resolver.lookup("media.example")
        }

      assertFalse(failure.message.orEmpty().contains("media.example"))
      assertFalse(failure.message.orEmpty().contains(rawAddress))
    }
  }

  @Test
  fun rejectsMixedPublicAndPrivateAddressResults() {
    val resolver =
      PlaybackDns.publicOnly(
        dnsReturning(
          listOf(
            InetAddress.getByName("8.8.8.8"),
            InetAddress.getByName("10.0.0.1"),
          )
        ),
      )

    assertThrows(UnknownHostException::class.java) {
      resolver.lookup("media.example")
    }
  }

  @Test
  fun ipLiteralsAreValidatedWithoutUsingDnsLookup() {
    val client = OkHttpClient.Builder().build()
    val rejected =
      listOf(
        "https://127.0.0.1/video.mp4",
        "https://[::1]/video.mp4",
        "https://[::ffff:127.0.0.1]/video.mp4",
        "https://[2001:db8::1]/video.mp4",
      )

    rejected.forEach { url ->
      val failure =
        assertThrows(IllegalArgumentException::class.java) {
          PlaybackHttpClientFactory.create(
            StreamRequest(url = url, headers = emptyMap(), containerMimeType = null),
            client.newBuilder(),
          )
        }
      assertFalse(failure.message.orEmpty().contains(url))
    }
    PlaybackHttpClientFactory.create(
      StreamRequest(
        url = "https://8.8.8.8/video.mp4",
        headers = emptyMap(),
        containerMimeType = null,
      ),
      client.newBuilder(),
    )
  }

  @Test
  fun productionClientUsesNoProxy() {
    val client =
      PlaybackHttpClientFactory.create(
        StreamRequest(
          url = "https://media.example/video.mp4",
          headers = emptyMap(),
          containerMimeType = null,
        ),
      )

    assertEquals(Proxy.NO_PROXY, client.proxy)
  }

  private fun dnsReturning(addresses: List<InetAddress>): Dns =
    object : Dns {
      override fun lookup(hostname: String): List<InetAddress> = addresses
    }
}
