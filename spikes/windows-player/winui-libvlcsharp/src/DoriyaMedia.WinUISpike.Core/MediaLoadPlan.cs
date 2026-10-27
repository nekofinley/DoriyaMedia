namespace DoriyaMedia.WinUISpike.Core;

public enum PlaybackErrorCode
{
    None,
    InvalidVideoUri,
    InvalidAudioUri,
    SameMediaUri,
    InvalidHeaderInput,
    DuplicateHeader,
    UnsupportedHeaderName,
    UnsupportedHeader,
    UnsupportedHeaderScope,
    HeaderValueTooLong,
    HeaderInputTooLong,
    MediaMergeUnavailable,
    MediaMergeFailed,
    LibVlcUnavailable,
    LibVlcRejectedMedia,
    PlaybackFailed
}

public enum MediaSourceMode
{
    SingleSource,
    VideoWithAudioSlave
}

public enum MediaHeaderName
{
    Referer,
    Origin,
    UserAgent,
    Accept,
    AcceptLanguage
}

public enum MediaHeaderScope
{
    BothStreams,
    MainOnly,
    SlaveOnly
}

public sealed class MediaHeaderInput
{
    public MediaHeaderInput(
        MediaHeaderName name,
        string value,
        MediaHeaderScope scope = MediaHeaderScope.BothStreams)
    {
        Name = name;
        Value = value ?? throw new ArgumentNullException(nameof(value));
        Scope = scope;
    }

    public MediaHeaderName Name { get; }

    // This value is only for LibVLC media options. It must never be logged.
    public string Value { get; }

    public MediaHeaderScope Scope { get; }

    public override string ToString() => $"{Name}: <redacted>";
}

public sealed class MediaHeaderParseResult
{
    private MediaHeaderParseResult(
        bool isValid,
        IReadOnlyList<MediaHeaderInput> headers,
        PlaybackErrorCode errorCode,
        string safeMessage)
    {
        IsValid = isValid;
        Headers = headers;
        ErrorCode = errorCode;
        SafeMessage = safeMessage;
    }

    public bool IsValid { get; }

    public IReadOnlyList<MediaHeaderInput> Headers { get; }

    public PlaybackErrorCode ErrorCode { get; }

    public string SafeMessage { get; }

    public static MediaHeaderParseResult Valid(IReadOnlyList<MediaHeaderInput> headers) =>
        new(true, headers, PlaybackErrorCode.None, "Header input accepted.");

    public static MediaHeaderParseResult Invalid(
        PlaybackErrorCode errorCode,
        string safeMessage) =>
        new(false, Array.Empty<MediaHeaderInput>(), errorCode, safeMessage);
}

public static class MediaHeaderInputParser
{
    public const int MaximumRawHeaderLength = 8192;
    public const int MaximumHeaderLineLength = 4096;

    public static MediaHeaderParseResult Parse(string? rawHeaders)
    {
        if (string.IsNullOrWhiteSpace(rawHeaders))
        {
            return MediaHeaderParseResult.Valid(Array.Empty<MediaHeaderInput>());
        }

        if (rawHeaders.Length > MaximumRawHeaderLength)
        {
            return MediaHeaderParseResult.Invalid(
                PlaybackErrorCode.HeaderInputTooLong,
                "The header input is too long.");
        }

        var headers = new List<MediaHeaderInput>();
        using var reader = new StringReader(rawHeaders);
        string? line;
        while ((line = reader.ReadLine()) is not null)
        {
            if (string.IsNullOrWhiteSpace(line))
            {
                continue;
            }

            if (line.Length > MaximumHeaderLineLength)
            {
                return MediaHeaderParseResult.Invalid(
                    PlaybackErrorCode.HeaderInputTooLong,
                    "A header line is too long.");
            }

            var separator = line.IndexOf(':');
            if (separator <= 0)
            {
                return MediaHeaderParseResult.Invalid(
                    PlaybackErrorCode.InvalidHeaderInput,
                    "Each header must use the Name: Value format.");
            }

            var rawName = line[..separator];
            if (rawName.Length == 0 ||
                rawName.Any(char.IsWhiteSpace) ||
                rawName.Any(char.IsControl))
            {
                return MediaHeaderParseResult.Invalid(
                    PlaybackErrorCode.InvalidHeaderInput,
                    "A header name contains an unsupported character.");
            }

            if (!TryParseHeaderName(rawName, out var name))
            {
                return MediaHeaderParseResult.Invalid(
                    PlaybackErrorCode.UnsupportedHeaderName,
                    "The header name is not allowed.");
            }

            var value = line[(separator + 1)..].Trim(' ');
            headers.Add(new MediaHeaderInput(name, value));
        }

        return MediaHeaderParseResult.Valid(headers);
    }

    private static bool TryParseHeaderName(string rawName, out MediaHeaderName name)
    {
        if (string.Equals(rawName, "Referer", StringComparison.OrdinalIgnoreCase))
        {
            name = MediaHeaderName.Referer;
            return true;
        }

        if (string.Equals(rawName, "Origin", StringComparison.OrdinalIgnoreCase))
        {
            name = MediaHeaderName.Origin;
            return true;
        }

        if (string.Equals(rawName, "User-Agent", StringComparison.OrdinalIgnoreCase))
        {
            name = MediaHeaderName.UserAgent;
            return true;
        }

        if (string.Equals(rawName, "Accept", StringComparison.OrdinalIgnoreCase))
        {
            name = MediaHeaderName.Accept;
            return true;
        }

        if (string.Equals(rawName, "Accept-Language", StringComparison.OrdinalIgnoreCase))
        {
            name = MediaHeaderName.AcceptLanguage;
            return true;
        }

        name = default;
        return false;
    }
}

public sealed class MediaLoadPlan
{
    internal MediaLoadPlan(
        string videoUri,
        string? audioSlaveUri,
        IReadOnlyList<MediaHeaderInput> headers,
        IReadOnlyList<string> libVlcMediaOptions)
    {
        VideoUri = videoUri;
        AudioSlaveUri = audioSlaveUri;
        Headers = headers;
        LibVlcMediaOptions = libVlcMediaOptions;
    }

    public string VideoUri { get; }

    public string? AudioSlaveUri { get; }

    public MediaSourceMode Mode =>
        AudioSlaveUri is null
            ? MediaSourceMode.SingleSource
            : MediaSourceMode.VideoWithAudioSlave;

    // Values are retained only for the playback backend and must never be logged.
    public IReadOnlyList<MediaHeaderInput> Headers { get; }

    // These options contain source URLs or header values and must never be logged.
    public IReadOnlyList<string> LibVlcMediaOptions { get; }

    public override string ToString() =>
        $"MediaLoadPlan({Mode}, sources and headers redacted)";
}

public sealed class MediaLoadPlanResult
{
    private MediaLoadPlanResult(
        MediaLoadPlan? plan,
        PlaybackErrorCode errorCode,
        string safeMessage)
    {
        Plan = plan;
        ErrorCode = errorCode;
        SafeMessage = safeMessage;
    }

    public bool IsValid => Plan is not null;

    public MediaLoadPlan? Plan { get; }

    public PlaybackErrorCode ErrorCode { get; }

    public string SafeMessage { get; }

    public static MediaLoadPlanResult Valid(MediaLoadPlan plan) =>
        new(plan, PlaybackErrorCode.None, "Media request accepted.");

    public static MediaLoadPlanResult Invalid(
        PlaybackErrorCode errorCode,
        string safeMessage) =>
        new(null, errorCode, safeMessage);
}

public static class MediaLoadPlanBuilder
{
    public const int MaximumHeaderCount = 5;
    public const int MaximumHeaderValueLength = 2048;

    public static MediaLoadPlanResult Build(
        string? rawVideoUri,
        string? rawAudioUri,
        string? rawHeaders,
        bool allowLoopbackHttp = false)
    {
        var parsedHeaders = MediaHeaderInputParser.Parse(rawHeaders);
        if (!parsedHeaders.IsValid)
        {
            return MediaLoadPlanResult.Invalid(
                parsedHeaders.ErrorCode,
                parsedHeaders.SafeMessage);
        }

        return BuildFromHeaders(
            rawVideoUri,
            rawAudioUri,
            parsedHeaders.Headers,
            allowLoopbackHttp);
    }

    public static MediaLoadPlanResult BuildFromHeaders(
        string? rawVideoUri,
        string? rawAudioUri,
        IReadOnlyList<MediaHeaderInput>? headers,
        bool allowLoopbackHttp = false)
    {
        var videoValidation = MediaUriValidator.Validate(rawVideoUri, allowLoopbackHttp);
        if (!videoValidation.IsValid)
        {
            return MediaLoadPlanResult.Invalid(
                PlaybackErrorCode.InvalidVideoUri,
                videoValidation.SafeMessage);
        }

        var videoUri = new Uri(videoValidation.MediaUri!, UriKind.Absolute);
        Uri? audioUri = null;
        if (!string.IsNullOrWhiteSpace(rawAudioUri))
        {
            var audioValidation = MediaUriValidator.Validate(rawAudioUri, allowLoopbackHttp);
            if (!audioValidation.IsValid)
            {
                return MediaLoadPlanResult.Invalid(
                    PlaybackErrorCode.InvalidAudioUri,
                    "The audio media address is invalid.");
            }

            audioUri = new Uri(audioValidation.MediaUri!, UriKind.Absolute);
            if (SameResource(videoUri, audioUri))
            {
                return MediaLoadPlanResult.Invalid(
                    PlaybackErrorCode.SameMediaUri,
                    "The video and audio addresses must be different.");
            }
        }

        var headerInputs = headers ?? Array.Empty<MediaHeaderInput>();
        if (headerInputs.Count > MaximumHeaderCount)
        {
            return MediaLoadPlanResult.Invalid(
                PlaybackErrorCode.InvalidHeaderInput,
                "Too many source headers were supplied.");
        }

        var seenNames = new HashSet<MediaHeaderName>();
        MediaHeaderInput? referer = null;
        MediaHeaderInput? userAgent = null;

        foreach (var header in headerInputs)
        {
            if (!seenNames.Add(header.Name))
            {
                return MediaLoadPlanResult.Invalid(
                    PlaybackErrorCode.DuplicateHeader,
                    "Duplicate source headers are not allowed.");
            }

            if (header.Scope != MediaHeaderScope.BothStreams)
            {
                return MediaLoadPlanResult.Invalid(
                    PlaybackErrorCode.UnsupportedHeaderScope,
                    "LibVLC media options cannot target headers to only one source.");
            }

            var valueValidation = ValidateHeaderValue(header, allowLoopbackHttp);
            if (valueValidation.ErrorCode != PlaybackErrorCode.None)
            {
                return MediaLoadPlanResult.Invalid(
                    valueValidation.ErrorCode,
                    valueValidation.SafeMessage);
            }

            if (valueValidation.Header is null)
            {
                return MediaLoadPlanResult.Invalid(
                    PlaybackErrorCode.UnsupportedHeader,
                    "The source header is not supported by this LibVLC path.");
            }

            if (valueValidation.Header.Name == MediaHeaderName.Referer)
            {
                referer = valueValidation.Header;
            }
            else
            {
                userAgent = valueValidation.Header;
            }
        }

        var normalizedHeaders = new List<MediaHeaderInput>(2);
        var options = new List<string>(2);

        if (referer is not null)
        {
            normalizedHeaders.Add(referer);
            options.Add($":http-referrer={referer.Value}");
        }

        if (userAgent is not null)
        {
            normalizedHeaders.Add(userAgent);
            options.Add($":http-user-agent={userAgent.Value}");
        }

        var plan = new MediaLoadPlan(
            videoUri.AbsoluteUri,
            audioUri?.AbsoluteUri,
            normalizedHeaders,
            options);
        return MediaLoadPlanResult.Valid(plan);
    }

    private static HeaderValueValidationResult ValidateHeaderValue(
        MediaHeaderInput header,
        bool allowLoopbackHttp)
    {
        var value = header.Value;
        if (value.Length == 0 ||
            value.Length > MaximumHeaderValueLength)
        {
            return HeaderValueValidationResult.Invalid(
                PlaybackErrorCode.HeaderValueTooLong,
                "A source header value is empty or too long.");
        }

        if (char.IsWhiteSpace(value[0]) ||
            char.IsWhiteSpace(value[^1]))
        {
            return HeaderValueValidationResult.Invalid(
                PlaybackErrorCode.InvalidHeaderInput,
                "A source header value has surrounding whitespace.");
        }

        foreach (var character in value)
        {
            if (character is < ' ' or > '~')
            {
                return HeaderValueValidationResult.Invalid(
                    PlaybackErrorCode.InvalidHeaderInput,
                    "A source header value contains an unsupported character.");
            }
        }

        switch (header.Name)
        {
            case MediaHeaderName.Referer:
            {
                var refererValidation = MediaUriValidator.Validate(
                    value,
                    allowLoopbackHttp);
                if (!refererValidation.IsValid)
                {
                    return HeaderValueValidationResult.Invalid(
                        PlaybackErrorCode.UnsupportedHeader,
                        "The Referer value must follow the media URL policy.");
                }

                return HeaderValueValidationResult.Valid(
                    new MediaHeaderInput(
                        MediaHeaderName.Referer,
                        refererValidation.MediaUri!,
                        header.Scope));
            }

            case MediaHeaderName.UserAgent:
                if (!IsVlcSafeUserAgent(value))
                {
                    return HeaderValueValidationResult.Invalid(
                        PlaybackErrorCode.UnsupportedHeader,
                        "The User-Agent value cannot be represented exactly by LibVLC.");
                }

                return HeaderValueValidationResult.Valid(header);

            case MediaHeaderName.Origin:
            case MediaHeaderName.Accept:
            case MediaHeaderName.AcceptLanguage:
                return HeaderValueValidationResult.Invalid(
                    PlaybackErrorCode.UnsupportedHeader,
                    "LibVLC 3.x does not expose a safe option for this header.");

            default:
                return HeaderValueValidationResult.Invalid(
                    PlaybackErrorCode.UnsupportedHeaderName,
                    "The source header is not supported.");
        }
    }

    private static bool IsVlcSafeUserAgent(string value)
    {
        var commentLevel = 0;
        foreach (var character in value)
        {
            if (commentLevel == 0)
            {
                if (")<>@,;:\\\"[]?={}".Contains(character))
                {
                    return false;
                }
            }
            else if (character == ')')
            {
                commentLevel--;
            }

            if (character == '(')
            {
                commentLevel++;
            }
        }

        return commentLevel == 0;
    }

    private static bool SameResource(Uri first, Uri second) =>
        string.Equals(first.Scheme, second.Scheme, StringComparison.OrdinalIgnoreCase) &&
        string.Equals(first.IdnHost, second.IdnHost, StringComparison.OrdinalIgnoreCase) &&
        first.Port == second.Port &&
        string.Equals(first.PathAndQuery, second.PathAndQuery, StringComparison.Ordinal);

    private sealed class HeaderValueValidationResult
    {
        private HeaderValueValidationResult(
            MediaHeaderInput? header,
            PlaybackErrorCode errorCode,
            string safeMessage)
        {
            Header = header;
            ErrorCode = errorCode;
            SafeMessage = safeMessage;
        }

        public MediaHeaderInput? Header { get; }

        public PlaybackErrorCode ErrorCode { get; }

        public string SafeMessage { get; }

        public static HeaderValueValidationResult Valid(MediaHeaderInput header) =>
            new(header, PlaybackErrorCode.None, string.Empty);

        public static HeaderValueValidationResult Invalid(
            PlaybackErrorCode errorCode,
            string safeMessage) =>
            new(null, errorCode, safeMessage);
    }
}
