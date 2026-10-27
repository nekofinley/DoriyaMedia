using System.Text.RegularExpressions;

namespace DoriyaMedia.WinUISpike.Core;

public static partial class SensitiveDataRedactor
{
    private const string RedactedUri = "<redacted-uri>";
    private const string RedactedValue = "<redacted>";

    public static string Redact(string? value)
    {
        if (string.IsNullOrEmpty(value))
        {
            return string.Empty;
        }

        var redacted = HeaderPattern().Replace(value, "${header}: <redacted>");
        redacted = UriPattern().Replace(redacted, RedactedUri);
        redacted = BearerPattern().Replace(redacted, "Bearer <redacted>");
        redacted = QuerySecretPattern().Replace(redacted, "${key}<redacted>");
        return redacted;
    }

    [GeneratedRegex(
        @"(?im)(?<![A-Za-z0-9_-])(?<header>Authorization|Proxy-Authorization|Cookie|Set-Cookie|Referer)\s*:\s*[^\r\n]*",
        RegexOptions.CultureInvariant)]
    private static partial Regex HeaderPattern();

    [GeneratedRegex(
        @"(?i)\bhttps?://[^\s<>""']+",
        RegexOptions.CultureInvariant)]
    private static partial Regex UriPattern();

    [GeneratedRegex(
        @"(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+",
        RegexOptions.CultureInvariant)]
    private static partial Regex BearerPattern();

    [GeneratedRegex(
        @"(?i)(?<![A-Za-z0-9_-])(?<key>(?:access_token|refresh_token|token|auth|authorization|cookie|signature|sig|expires|key)=)[^&#;\s]+",
        RegexOptions.CultureInvariant)]
    private static partial Regex QuerySecretPattern();
}
