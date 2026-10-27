using System.Net;

namespace DoriyaMedia.WinUISpike.Core;

public sealed class MediaUriValidationResult
{
    private MediaUriValidationResult(bool isValid, string? mediaUri, string safeMessage)
    {
        IsValid = isValid;
        MediaUri = mediaUri;
        SafeMessage = safeMessage;
    }

    public bool IsValid { get; }

    // This value is only for the playback backend. It must never be logged.
    public string? MediaUri { get; }

    public string SafeMessage { get; }

    public static MediaUriValidationResult Valid(string mediaUri) =>
        new(true, mediaUri, "Media address accepted.");

    public static MediaUriValidationResult Invalid(string safeMessage) =>
        new(false, null, safeMessage);
}

public static class MediaUriValidator
{
    public const int MaximumUriLength = 8192;

    public static MediaUriValidationResult Validate(
        string? rawUri,
        bool allowLoopbackHttp = false)
    {
        if (string.IsNullOrWhiteSpace(rawUri))
        {
            return MediaUriValidationResult.Invalid("Enter an HTTPS media address.");
        }

        if (rawUri.Length > MaximumUriLength)
        {
            return MediaUriValidationResult.Invalid("The media address is too long.");
        }

        if (rawUri.Any(char.IsControl) ||
            rawUri.Any(char.IsWhiteSpace) ||
            rawUri.Contains('\\') ||
            rawUri.Contains('#'))
        {
            return MediaUriValidationResult.Invalid(
                "The media address contains an unsupported character, fragment, or escaped separator.");
        }

        if (!Uri.TryCreate(rawUri, UriKind.Absolute, out var uri))
        {
            return MediaUriValidationResult.Invalid("The media address is not an absolute URI.");
        }

        if (string.IsNullOrWhiteSpace(uri.Host))
        {
            return MediaUriValidationResult.Invalid("The media address must include a host.");
        }

        if (!string.IsNullOrEmpty(uri.UserInfo))
        {
            return MediaUriValidationResult.Invalid("Credentials embedded in the media address are not supported.");
        }

        if (string.Equals(uri.Scheme, Uri.UriSchemeHttps, StringComparison.OrdinalIgnoreCase))
        {
            return MediaUriValidationResult.Valid(uri.AbsoluteUri);
        }

        if (!string.Equals(uri.Scheme, Uri.UriSchemeHttp, StringComparison.OrdinalIgnoreCase))
        {
            return MediaUriValidationResult.Invalid(
                "Only HTTPS is supported; plain HTTP is limited to explicit loopback test mode.");
        }

        if (!allowLoopbackHttp)
        {
            return MediaUriValidationResult.Invalid(
                "Plain HTTP is not allowed. Use HTTPS unless loopback test mode is explicitly enabled.");
        }

        if (!IsStrictLoopbackHost(uri))
        {
            return MediaUriValidationResult.Invalid(
                "Plain HTTP is only allowed for the exact loopback hosts 127.0.0.1, ::1, or localhost.");
        }

        return MediaUriValidationResult.Valid(uri.AbsoluteUri);
    }

    private static bool IsStrictLoopbackHost(Uri uri)
    {
        var host = uri.DnsSafeHost;
        if (string.Equals(host, "localhost", StringComparison.OrdinalIgnoreCase))
        {
            return true;
        }

        return IPAddress.TryParse(host, out var address) &&
            (IPAddress.Loopback.Equals(address) || IPAddress.IPv6Loopback.Equals(address));
    }
}
