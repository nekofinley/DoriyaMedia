using System.Collections.ObjectModel;

namespace DoriyaMedia.WinUISpike.LoopbackRelayProbe;

public sealed class RelayRoute
{
    private const int MaximumHeaderValueLength = 4096;

    private static readonly string[] AllowedHeaders =
    [
        "Origin",
        "Referer",
        "User-Agent",
    ];

    public RelayRoute(Uri upstreamUri, IReadOnlyDictionary<string, string>? injectedHeaders = null)
    {
        ArgumentNullException.ThrowIfNull(upstreamUri);

        if (!upstreamUri.IsAbsoluteUri ||
            (!string.Equals(upstreamUri.Scheme, Uri.UriSchemeHttp, StringComparison.OrdinalIgnoreCase) &&
             !string.Equals(upstreamUri.Scheme, Uri.UriSchemeHttps, StringComparison.OrdinalIgnoreCase)) ||
            !string.IsNullOrEmpty(upstreamUri.UserInfo) ||
            !string.IsNullOrEmpty(upstreamUri.Fragment) ||
            string.IsNullOrWhiteSpace(upstreamUri.Host))
        {
            throw new ArgumentException("The upstream must be an absolute HTTP(S) URI without user info or a fragment.", nameof(upstreamUri));
        }

        if (!string.Equals(upstreamUri.Scheme, Uri.UriSchemeHttps, StringComparison.OrdinalIgnoreCase) &&
            !string.Equals(upstreamUri.IdnHost, "127.0.0.1", StringComparison.Ordinal))
        {
            throw new ArgumentException(
                "Plain HTTP is allowed only for the exact 127.0.0.1 relay fixture.",
                nameof(upstreamUri));
        }

        UpstreamUri = upstreamUri;
        InjectedHeaders = CopyAndValidateHeaders(injectedHeaders);
    }

    public Uri UpstreamUri { get; }

    public IReadOnlyDictionary<string, string> InjectedHeaders { get; }

    public static IReadOnlyList<string> AllowedInjectedHeaderNames { get; } =
        Array.AsReadOnly(AllowedHeaders);

    private static IReadOnlyDictionary<string, string> CopyAndValidateHeaders(
        IReadOnlyDictionary<string, string>? headers)
    {
        var copy = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
        if (headers is null)
        {
            return new ReadOnlyDictionary<string, string>(copy);
        }

        foreach (var (name, value) in headers)
        {
            if (!AllowedHeaders.Contains(name, StringComparer.OrdinalIgnoreCase))
            {
                throw new ArgumentException("An injected header is not on the relay allowlist.", nameof(headers));
            }

            if (string.IsNullOrWhiteSpace(value) || value.Length > MaximumHeaderValueLength)
            {
                throw new ArgumentException("An injected header value is empty, too long, or invalid.", nameof(headers));
            }

            if (!string.Equals(value, value.Trim(' ', '\t'), StringComparison.Ordinal))
            {
                throw new ArgumentException("An injected header value must not contain leading or trailing whitespace.", nameof(headers));
            }

            foreach (var character in value)
            {
                if (character is '\r' or '\n' or '\0' ||
                    character > '\x7E' ||
                    char.IsControl(character))
                {
                    throw new ArgumentException("An injected header value contains a non-ASCII or control character.", nameof(headers));
                }
            }

            ValidateStructuredHeaderValue(name, value);

            if (!copy.TryAdd(name, value))
            {
                throw new ArgumentException("Injected header names must be unique.", nameof(headers));
            }
        }

        return new ReadOnlyDictionary<string, string>(copy);
    }

    private static void ValidateStructuredHeaderValue(string name, string value)
    {
        if (string.Equals(name, "Origin", StringComparison.OrdinalIgnoreCase))
        {
            RejectAmbiguousUrlCharacters(value);

            if (!Uri.TryCreate(value, UriKind.Absolute, out var origin) ||
                !IsHttpUri(origin) ||
                !string.IsNullOrEmpty(origin.UserInfo) ||
                !string.IsNullOrEmpty(origin.Fragment) ||
                !string.Equals(origin.AbsolutePath, "/", StringComparison.Ordinal) ||
                !string.IsNullOrEmpty(origin.Query))
            {
                throw new ArgumentException("The Origin header must contain only an HTTP(S) scheme, host, and optional port.", nameof(value));
            }

            if (!string.Equals(value, GetCanonicalOrigin(origin), StringComparison.Ordinal))
            {
                throw new ArgumentException("The Origin header must use its canonical serialization.", nameof(value));
            }

            return;
        }

        if (string.Equals(name, "Referer", StringComparison.OrdinalIgnoreCase))
        {
            RejectAmbiguousUrlCharacters(value);

            if (!Uri.TryCreate(value, UriKind.Absolute, out var referer) ||
                !IsHttpUri(referer) ||
                !string.IsNullOrEmpty(referer.UserInfo) ||
                !string.IsNullOrEmpty(referer.Fragment))
            {
                throw new ArgumentException("The Referer header must be an absolute HTTP(S) URL without user info or a fragment.", nameof(value));
            }

            var canonicalReferer = referer.GetComponents(
                UriComponents.HttpRequestUrl,
                UriFormat.UriEscaped);
            if (!string.Equals(value, canonicalReferer, StringComparison.Ordinal))
            {
                throw new ArgumentException("The Referer header must use its canonical serialization.", nameof(value));
            }
        }
    }

    private static void RejectAmbiguousUrlCharacters(string value)
    {
        if (value.Contains('\\') || value.Contains('%'))
        {
            throw new ArgumentException("URL header values must not contain backslashes or percent escapes.", nameof(value));
        }
    }

    private static string GetCanonicalOrigin(Uri origin)
    {
        var host = origin.IdnHost.ToLowerInvariant();
        var formattedHost = host.Contains(':') ? $"[{host}]" : host;
        var port = origin.IsDefaultPort ? string.Empty : $":{origin.Port}";
        return $"{origin.Scheme.ToLowerInvariant()}://{formattedHost}{port}";
    }

    private static bool IsHttpUri(Uri uri)
    {
        return string.Equals(uri.Scheme, Uri.UriSchemeHttp, StringComparison.OrdinalIgnoreCase) ||
               string.Equals(uri.Scheme, Uri.UriSchemeHttps, StringComparison.OrdinalIgnoreCase);
    }
}
