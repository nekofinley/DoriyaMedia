namespace DoriyaMedia.WinUISpike.LoopbackRelayProbe;

public static class RelayRoles
{
    public const string Video = "video";
    public const string Audio = "audio";
}

public sealed class RelayOptions
{
    public static readonly TimeSpan DefaultUpstreamTimeout = TimeSpan.FromSeconds(30);

    public RelayOptions(
        string capabilityToken,
        RelayRoute video,
        RelayRoute audio,
        RelayTargetPolicy targetPolicy,
        TimeSpan? upstreamTimeout = null,
        IRelayAuditSink? auditSink = null)
    {
        if (!RelayCapabilityToken.IsWellFormed(capabilityToken))
        {
            throw new ArgumentException("The capability token must contain 256 bits encoded as base64url.", nameof(capabilityToken));
        }

        ArgumentNullException.ThrowIfNull(video);
        ArgumentNullException.ThrowIfNull(audio);
        ArgumentNullException.ThrowIfNull(targetPolicy);

        targetPolicy.Validate(video.UpstreamUri);
        targetPolicy.Validate(audio.UpstreamUri);

        var effectiveTimeout = upstreamTimeout ?? DefaultUpstreamTimeout;
        if (effectiveTimeout <= TimeSpan.Zero || effectiveTimeout > TimeSpan.FromMinutes(5))
        {
            throw new ArgumentOutOfRangeException(nameof(upstreamTimeout));
        }

        CapabilityToken = capabilityToken;
        Video = video;
        Audio = audio;
        TargetPolicy = targetPolicy;
        UpstreamTimeout = effectiveTimeout;
        AuditSink = auditSink ?? NullRelayAuditSink.Instance;
    }

    public string CapabilityToken { get; }

    public RelayRoute Video { get; }

    public RelayRoute Audio { get; }

    public RelayTargetPolicy TargetPolicy { get; }

    public TimeSpan UpstreamTimeout { get; }

    public IRelayAuditSink AuditSink { get; }

    public bool TryGetRoute(string role, out RelayRoute? route)
    {
        route = role switch
        {
            RelayRoles.Video => Video,
            RelayRoles.Audio => Audio,
            _ => null,
        };

        return route is not null;
    }

    public Uri GetClientUri(Uri relayBaseAddress, string role)
    {
        ArgumentNullException.ThrowIfNull(relayBaseAddress);

        if (!TryGetRoute(role, out _))
        {
            throw new ArgumentException("The stream role is not recognized.", nameof(role));
        }

        return new Uri(relayBaseAddress, $"{CapabilityToken}/{role}");
    }
}
