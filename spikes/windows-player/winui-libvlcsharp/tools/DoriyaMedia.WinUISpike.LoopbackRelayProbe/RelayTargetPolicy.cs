using System.Globalization;
using System.Net;
using System.Net.Sockets;

namespace DoriyaMedia.WinUISpike.LoopbackRelayProbe;

public sealed class RelayTargetPolicy
{
    private static readonly Ipv6Prefix[] Ipv6GlobalUnicastAllocations =
    [
        CreateIpv6Prefix("2001:200::", 23),
        CreateIpv6Prefix("2001:400::", 23),
        CreateIpv6Prefix("2001:600::", 23),
        CreateIpv6Prefix("2001:800::", 22),
        CreateIpv6Prefix("2001:c00::", 23),
        CreateIpv6Prefix("2001:e00::", 23),
        CreateIpv6Prefix("2001:1200::", 23),
        CreateIpv6Prefix("2001:1400::", 22),
        CreateIpv6Prefix("2001:1800::", 23),
        CreateIpv6Prefix("2001:1a00::", 23),
        CreateIpv6Prefix("2001:1c00::", 22),
        CreateIpv6Prefix("2001:2000::", 19),
        CreateIpv6Prefix("2001:4000::", 23),
        CreateIpv6Prefix("2001:4200::", 23),
        CreateIpv6Prefix("2001:4400::", 23),
        CreateIpv6Prefix("2001:4600::", 23),
        CreateIpv6Prefix("2001:4800::", 23),
        CreateIpv6Prefix("2001:4a00::", 23),
        CreateIpv6Prefix("2001:4c00::", 23),
        CreateIpv6Prefix("2001:5000::", 20),
        CreateIpv6Prefix("2001:8000::", 19),
        CreateIpv6Prefix("2001:a000::", 20),
        CreateIpv6Prefix("2001:b000::", 20),
        CreateIpv6Prefix("2003::", 18),
        CreateIpv6Prefix("2400::", 12),
        CreateIpv6Prefix("2410::", 12),
        CreateIpv6Prefix("2600::", 12),
        CreateIpv6Prefix("2610::", 23),
        CreateIpv6Prefix("2620::", 23),
        CreateIpv6Prefix("2630::", 12),
        CreateIpv6Prefix("2800::", 12),
        CreateIpv6Prefix("2a00::", 12),
        CreateIpv6Prefix("2a10::", 12),
        CreateIpv6Prefix("2c00::", 12),
    ];

    private static readonly Ipv6Prefix[] Ipv6SpecialPurposeExclusions =
    [
        CreateIpv6Prefix("2001:db8::", 32),
    ];

    private readonly HashSet<string> _allowedOrigins;
    private readonly bool _allowExactLoopbackFixture;

    private RelayTargetPolicy(IEnumerable<Uri> allowedOrigins, bool allowExactLoopbackFixture)
    {
        _allowExactLoopbackFixture = allowExactLoopbackFixture;
        _allowedOrigins = new HashSet<string>(StringComparer.Ordinal);

        foreach (var origin in allowedOrigins)
        {
            if (!TryGetOrigin(origin, requireOriginOnly: true, out var canonicalOrigin))
            {
                throw new ArgumentException("An allowed target must be an absolute HTTP(S) origin.", nameof(allowedOrigins));
            }

            if (allowExactLoopbackFixture)
            {
                if (!string.Equals(origin.IdnHost, "127.0.0.1", StringComparison.Ordinal))
                {
                    throw new ArgumentException("The loopback fixture policy accepts only the exact 127.0.0.1 host.", nameof(allowedOrigins));
                }
            }
            else
            {
                if (!string.Equals(origin.Scheme, Uri.UriSchemeHttps, StringComparison.OrdinalIgnoreCase))
                {
                    throw new ArgumentException("Public relay targets must use HTTPS.", nameof(allowedOrigins));
                }

                if (IsLoopbackOrLocalHost(origin.IdnHost) ||
                    (IPAddress.TryParse(origin.IdnHost, out var address) &&
                     !IsGlobalUnicastAddress(address)))
                {
                    throw new ArgumentException("Private, loopback, and special-use targets require an explicit exact-loopback fixture policy.", nameof(allowedOrigins));
                }
            }

            _allowedOrigins.Add(canonicalOrigin);
        }

        if (_allowedOrigins.Count == 0)
        {
            throw new ArgumentException("At least one exact upstream origin must be allowlisted.", nameof(allowedOrigins));
        }
    }

    public static RelayTargetPolicy CreateExactOrigins(IEnumerable<Uri> allowedOrigins)
    {
        ArgumentNullException.ThrowIfNull(allowedOrigins);
        return new RelayTargetPolicy(allowedOrigins, allowExactLoopbackFixture: false);
    }

    public static RelayTargetPolicy CreateLoopbackFixture(IEnumerable<Uri> allowedOrigins)
    {
        ArgumentNullException.ThrowIfNull(allowedOrigins);
        return new RelayTargetPolicy(allowedOrigins, allowExactLoopbackFixture: true);
    }

    public void Validate(Uri target)
    {
        ArgumentNullException.ThrowIfNull(target);

        if (!TryGetOrigin(target, requireOriginOnly: false, out var canonicalOrigin) ||
            !_allowedOrigins.Contains(canonicalOrigin))
        {
            throw new ArgumentException("The upstream target is not on the exact-origin allowlist.", nameof(target));
        }

        if (_allowExactLoopbackFixture)
        {
            if (!string.Equals(target.IdnHost, "127.0.0.1", StringComparison.Ordinal))
            {
                throw new ArgumentException("The upstream target is a loopback target without fixture authorization.", nameof(target));
            }
        }
        else if (!string.Equals(target.Scheme, Uri.UriSchemeHttps, StringComparison.OrdinalIgnoreCase) ||
                 IsLoopbackOrLocalHost(target.IdnHost) ||
                 (IPAddress.TryParse(target.IdnHost, out var address) &&
                  !IsGlobalUnicastAddress(address)))
        {
            throw new ArgumentException("The upstream target is not a public HTTPS target.", nameof(target));
        }
    }

    public bool IsConnectAddressAllowed(string upstreamHost, IPAddress address)
    {
        ArgumentNullException.ThrowIfNull(address);

        if (_allowExactLoopbackFixture)
        {
            return string.Equals(upstreamHost, "127.0.0.1", StringComparison.Ordinal) &&
                   address.Equals(IPAddress.Loopback);
        }

        return IsGlobalUnicastAddress(address);
    }

    private static bool TryGetOrigin(Uri uri, bool requireOriginOnly, out string canonicalOrigin)
    {
        canonicalOrigin = string.Empty;

        if (!uri.IsAbsoluteUri ||
            (!string.Equals(uri.Scheme, Uri.UriSchemeHttp, StringComparison.OrdinalIgnoreCase) &&
             !string.Equals(uri.Scheme, Uri.UriSchemeHttps, StringComparison.OrdinalIgnoreCase)) ||
            !string.IsNullOrEmpty(uri.UserInfo) ||
            !string.IsNullOrEmpty(uri.Fragment) ||
            string.IsNullOrWhiteSpace(uri.Host))
        {
            return false;
        }

        if (requireOriginOnly &&
            (!string.Equals(uri.AbsolutePath, "/", StringComparison.Ordinal) ||
             !string.IsNullOrEmpty(uri.Query)))
        {
            return false;
        }

        var host = uri.IdnHost;
        var formattedHost = host.Contains(':') ? $"[{host}]" : host;
        canonicalOrigin = string.Create(
            CultureInfo.InvariantCulture,
            $"{uri.Scheme.ToLowerInvariant()}://{formattedHost.ToLowerInvariant()}:{uri.Port}");
        return true;
    }

    private static bool IsLoopbackOrLocalHost(string host)
    {
        return string.Equals(host, "localhost", StringComparison.OrdinalIgnoreCase) ||
               host.EndsWith(".localhost", StringComparison.OrdinalIgnoreCase) ||
               (IPAddress.TryParse(host, out var address) && IPAddress.IsLoopback(address));
    }

    private static bool IsGlobalUnicastAddress(IPAddress address)
    {
        var bytes = address.GetAddressBytes();
        if (address.AddressFamily == AddressFamily.InterNetwork)
        {
            return bytes[0] is not (0 or 10 or 127 or >= 224) &&
                   !(bytes[0] == 100 && bytes[1] is >= 64 and <= 127) &&
                   !(bytes[0] == 169 && bytes[1] == 254) &&
                   !(bytes[0] == 172 && bytes[1] is >= 16 and <= 31) &&
                   !(bytes[0] == 192 && bytes[1] == 0 && bytes[2] is 0 or 2) &&
                   !(bytes[0] == 192 && bytes[1] == 88 && bytes[2] == 99) &&
                   !(bytes[0] == 192 && bytes[1] == 168) &&
                   !(bytes[0] == 198 && bytes[1] is 18 or 19) &&
                   !(bytes[0] == 198 && bytes[1] == 51 && bytes[2] == 100) &&
                   !(bytes[0] == 203 && bytes[1] == 0 && bytes[2] == 113);
        }

        if (address.AddressFamily != AddressFamily.InterNetworkV6)
        {
            return false;
        }

        foreach (var exclusion in Ipv6SpecialPurposeExclusions)
        {
            if (exclusion.Matches(bytes))
            {
                return false;
            }
        }

        foreach (var allocation in Ipv6GlobalUnicastAllocations)
        {
            if (allocation.Matches(bytes))
            {
                return true;
            }
        }

        return false;
    }

    private static Ipv6Prefix CreateIpv6Prefix(string network, int prefixLength)
    {
        if (prefixLength is < 0 or > 128)
        {
            throw new ArgumentOutOfRangeException(nameof(prefixLength));
        }

        var address = IPAddress.Parse(network);
        if (address.AddressFamily != AddressFamily.InterNetworkV6)
        {
            throw new InvalidOperationException($"{network} is not an IPv6 network.");
        }

        return new Ipv6Prefix(address.GetAddressBytes(), prefixLength);
    }

    private readonly record struct Ipv6Prefix(ReadOnlyMemory<byte> NetworkBytes, int PrefixLength)
    {
        public bool Matches(ReadOnlySpan<byte> address)
        {
            var networkBytes = NetworkBytes.Span;
            if (address.Length != networkBytes.Length)
            {
                return false;
            }

            var fullBytes = PrefixLength / 8;
            if (!address[..fullBytes].SequenceEqual(networkBytes[..fullBytes]))
            {
                return false;
            }

            var remainingBits = PrefixLength % 8;
            if (remainingBits == 0)
            {
                return true;
            }

            var mask = (byte)(0xFF << (8 - remainingBits));
            return (address[fullBytes] & mask) == (networkBytes[fullBytes] & mask);
        }
    }
}
