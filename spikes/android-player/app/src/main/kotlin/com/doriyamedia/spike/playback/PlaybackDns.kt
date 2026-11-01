package com.doriyamedia.spike.playback

import java.net.InetAddress
import java.net.UnknownHostException
import okhttp3.Dns

internal object PlaybackDns {
  fun publicOnly(delegate: Dns = Dns.SYSTEM): Dns =
    object : Dns {
      override fun lookup(hostname: String): List<InetAddress> {
        val addresses = delegate.lookup(hostname)
        if (addresses.isEmpty() || addresses.any { !isPublicAddress(it) }) {
          throw UnknownHostException("Host does not resolve exclusively to public addresses")
        }
        return addresses
      }
    }

  fun requirePublicLiteral(host: String) {
    val isIpLiteral = host.contains(':') || host.all { it.isDigit() || it == '.' }
    if (!isIpLiteral) {
      return
    }
    val address =
      try {
        InetAddress.getByName(host)
      } catch (_: UnknownHostException) {
        throw IllegalArgumentException("Playback target contains an invalid IP address")
      }
    require(isPublicAddress(address)) {
      "Playback target must use a public IP address"
    }
  }

  private fun isPublicAddress(address: InetAddress): Boolean {
    if (
      address.isAnyLocalAddress ||
        address.isLoopbackAddress ||
        address.isLinkLocalAddress ||
        address.isSiteLocalAddress ||
        address.isMulticastAddress
    ) {
      return false
    }

    val bytes = address.address
    return when (bytes.size) {
      4 -> isPublicIpv4(bytes)
      16 -> IPV6_PUBLIC_ALLOCATIONS.any { it.matches(bytes) } &&
        IPV6_SPECIAL_EXCLUSIONS.none { it.matches(bytes) }
      else -> false
    }
  }

  private fun isPublicIpv4(bytes: ByteArray): Boolean {
    val first = bytes[0].toInt() and 0xFF
    val second = bytes[1].toInt() and 0xFF
    val third = bytes[2].toInt() and 0xFF
    return first !in intArrayOf(0, 10, 127) &&
      first < 224 &&
      !(first == 100 && second in 64..127) &&
      !(first == 169 && second == 254) &&
      !(first == 172 && second in 16..31) &&
      !(first == 192 && second == 0 && third in intArrayOf(0, 2)) &&
      !(first == 192 && second == 88 && third == 99) &&
      !(first == 192 && second == 168) &&
      !(first == 198 && second in intArrayOf(18, 19)) &&
      !(first == 198 && second == 51 && third == 100) &&
      !(first == 203 && second == 0 && third == 113)
  }

  private fun createPrefix(network: String, prefixLength: Int): Ipv6Prefix {
    require(prefixLength in 0..128)
    val bytes = InetAddress.getByName(network).address
    require(bytes.size == 16)
    return Ipv6Prefix(bytes, prefixLength)
  }

  private val IPV6_PUBLIC_ALLOCATIONS =
    listOf(
      createPrefix("2001:200::", 23),
      createPrefix("2001:400::", 23),
      createPrefix("2001:600::", 23),
      createPrefix("2001:800::", 22),
      createPrefix("2001:c00::", 23),
      createPrefix("2001:e00::", 23),
      createPrefix("2001:1200::", 23),
      createPrefix("2001:1400::", 22),
      createPrefix("2001:1800::", 23),
      createPrefix("2001:1a00::", 23),
      createPrefix("2001:1c00::", 22),
      createPrefix("2001:2000::", 19),
      createPrefix("2001:4000::", 23),
      createPrefix("2001:4200::", 23),
      createPrefix("2001:4400::", 23),
      createPrefix("2001:4600::", 23),
      createPrefix("2001:4800::", 23),
      createPrefix("2001:4a00::", 23),
      createPrefix("2001:4c00::", 23),
      createPrefix("2001:5000::", 20),
      createPrefix("2001:8000::", 19),
      createPrefix("2001:a000::", 20),
      createPrefix("2001:b000::", 20),
      createPrefix("2003::", 18),
      createPrefix("2400::", 12),
      createPrefix("2410::", 12),
      createPrefix("2600::", 12),
      createPrefix("2610::", 23),
      createPrefix("2620::", 23),
      createPrefix("2630::", 12),
      createPrefix("2800::", 12),
      createPrefix("2a00::", 12),
      createPrefix("2a10::", 12),
      createPrefix("2c00::", 12),
    )

  private val IPV6_SPECIAL_EXCLUSIONS =
    listOf(
      createPrefix("2001:db8::", 32),
    )

  private class Ipv6Prefix(
    private val network: ByteArray,
    private val prefixLength: Int,
  ) {
    fun matches(address: ByteArray): Boolean {
      val fullBytes = prefixLength / 8
      for (index in 0 until fullBytes) {
        if (address[index] != network[index]) {
          return false
        }
      }

      val remainingBits = prefixLength % 8
      if (remainingBits == 0) {
        return true
      }
      val mask = 0xFF shl (8 - remainingBits)
      return (address[fullBytes].toInt() and mask) ==
        (network[fullBytes].toInt() and mask)
    }
  }
}
