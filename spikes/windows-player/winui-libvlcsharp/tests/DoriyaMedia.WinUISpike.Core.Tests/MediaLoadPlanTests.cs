using DoriyaMedia.WinUISpike.Core;
using Xunit;

namespace DoriyaMedia.WinUISpike.Core.Tests;

public sealed class MediaLoadPlanTests
{
    [Fact]
    public void Build_SingleSource_CreatesNoSlaveOrHeaderOptions()
    {
        var result = MediaLoadPlanBuilder.Build(
            "https://media.example.test/video.mp4",
            rawAudioUri: null,
            rawHeaders: null);

        Assert.True(result.IsValid);
        Assert.NotNull(result.Plan);
        Assert.Equal(MediaSourceMode.SingleSource, result.Plan.Mode);
        Assert.Null(result.Plan.AudioSlaveUri);
        Assert.Empty(result.Plan.Headers);
        Assert.Empty(result.Plan.LibVlcMediaOptions);
    }

    [Fact]
    public void Build_DualSource_CreatesATypedAudioSlavePlan()
    {
        var result = MediaLoadPlanBuilder.Build(
            "https://media.example.test/video.mp4",
            "https://media.example.test/audio.mp4",
            rawHeaders: null);

        Assert.True(result.IsValid);
        Assert.NotNull(result.Plan);
        Assert.Equal(MediaSourceMode.VideoWithAudioSlave, result.Plan.Mode);
        Assert.Equal(
            "https://media.example.test/audio.mp4",
            result.Plan.AudioSlaveUri);
        Assert.Empty(result.Plan.LibVlcMediaOptions);
    }

    [Theory]
    [InlineData("https://media.example.test/video.mp4")]
    [InlineData("https://MEDIA.EXAMPLE.TEST:443/video.mp4")]
    public void Build_RejectsTheSameVideoAndAudioResource(string audioUri)
    {
        var result = MediaLoadPlanBuilder.Build(
            "https://media.example.test/video.mp4",
            audioUri,
            rawHeaders: null);

        Assert.False(result.IsValid);
        Assert.Null(result.Plan);
        Assert.Equal(PlaybackErrorCode.SameMediaUri, result.ErrorCode);
        Assert.DoesNotContain("media.example.test", result.SafeMessage, StringComparison.Ordinal);
    }

    [Theory]
    [InlineData("file:///C:/private-audio.mp4")]
    [InlineData("http://media.example.test/audio.mp4")]
    [InlineData("https://user:password@media.example.test/audio.mp4")]
    [InlineData("https://media.example.test/audio.mp4#fragment")]
    public void Build_RejectsInvalidAudioUris(string audioUri)
    {
        var result = MediaLoadPlanBuilder.Build(
            "https://media.example.test/video.mp4",
            audioUri,
            rawHeaders: null);

        Assert.False(result.IsValid);
        Assert.Null(result.Plan);
        Assert.Equal(PlaybackErrorCode.InvalidAudioUri, result.ErrorCode);
        Assert.DoesNotContain("password", result.SafeMessage, StringComparison.OrdinalIgnoreCase);
    }

    [Fact]
    public void Build_MapsRefererAndUserAgentToLibVlcMediaOptions()
    {
        const string referer = "https://referrer.example.test/watch?token=source-secret";
        const string userAgent =
            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) DoriyaSpike/1.0";

        var result = MediaLoadPlanBuilder.Build(
            "https://media.example.test/video.mp4",
            "https://media.example.test/audio.mp4",
            $"User-Agent: {userAgent}\nreferer: {referer}");

        Assert.True(result.IsValid);
        Assert.NotNull(result.Plan);
        Assert.Equal(
            [
                $":http-referrer={referer}",
                $":http-user-agent={userAgent}"
            ],
            result.Plan.LibVlcMediaOptions);
        Assert.Equal(
            [MediaHeaderName.Referer, MediaHeaderName.UserAgent],
            result.Plan.Headers.Select(header => header.Name));
    }

    [Theory]
    [InlineData("Origin: https://origin.example.test")]
    [InlineData("Accept: video/mp4")]
    [InlineData("Accept-Language: en-US")]
    public void Build_FailsClosedForHeadersLibVlcCannotInject(string rawHeader)
    {
        var result = MediaLoadPlanBuilder.Build(
            "https://media.example.test/video.mp4",
            "https://media.example.test/audio.mp4",
            rawHeader);

        Assert.False(result.IsValid);
        Assert.Null(result.Plan);
        Assert.Equal(PlaybackErrorCode.UnsupportedHeader, result.ErrorCode);
        Assert.Contains("does not expose", result.SafeMessage, StringComparison.Ordinal);
    }

    [Theory]
    [InlineData("Authorization: Bearer secret")]
    [InlineData("Cookie: SESSION=secret")]
    [InlineData("X-Forwarded-For: 127.0.0.1")]
    public void Parse_RejectsHeaderNamesOutsideTheAllowlist(string rawHeader)
    {
        var result = MediaHeaderInputParser.Parse(rawHeader);

        Assert.False(result.IsValid);
        Assert.Equal(PlaybackErrorCode.UnsupportedHeaderName, result.ErrorCode);
        Assert.DoesNotContain("secret", result.SafeMessage, StringComparison.OrdinalIgnoreCase);
    }

    [Fact]
    public void Build_RejectsDuplicateHeaderNamesCaseInsensitively()
    {
        var result = MediaLoadPlanBuilder.Build(
            "https://media.example.test/video.mp4",
            rawAudioUri: null,
            "Referer: https://first.example.test/\n" +
            "referer: https://second.example.test/");

        Assert.False(result.IsValid);
        Assert.Equal(PlaybackErrorCode.DuplicateHeader, result.ErrorCode);
        Assert.DoesNotContain("first.example.test", result.SafeMessage, StringComparison.Ordinal);
        Assert.DoesNotContain("second.example.test", result.SafeMessage, StringComparison.Ordinal);
    }

    [Theory]
    [InlineData("Doriya\r\nInjected/1.0")]
    [InlineData("Doriya\tSpike/1.0")]
    [InlineData(" DoriyaSpike/1.0")]
    [InlineData("DoriyaSpike/1.0 ")]
    [InlineData("DoriyaSpike/\u00e9")]
    [InlineData("Doriya;Secret/1.0")]
    [InlineData("DoriyaSpike/1.0 (unclosed")]
    public void Build_RejectsUnsafeOrNonExactUserAgentValues(string value)
    {
        var result = MediaLoadPlanBuilder.BuildFromHeaders(
            "https://media.example.test/video.mp4",
            rawAudioUri: null,
            [new MediaHeaderInput(MediaHeaderName.UserAgent, value)]);

        Assert.False(result.IsValid);
        Assert.Null(result.Plan);
        Assert.True(
            result.ErrorCode is
                PlaybackErrorCode.InvalidHeaderInput or
                PlaybackErrorCode.UnsupportedHeader);
        Assert.DoesNotContain(value, result.SafeMessage, StringComparison.Ordinal);
    }

    [Fact]
    public void Build_RejectsHeaderValuesAboveTheLengthLimit()
    {
        var value = new string('a', MediaLoadPlanBuilder.MaximumHeaderValueLength + 1);

        var result = MediaLoadPlanBuilder.BuildFromHeaders(
            "https://media.example.test/video.mp4",
            rawAudioUri: null,
            [new MediaHeaderInput(MediaHeaderName.UserAgent, value)]);

        Assert.False(result.IsValid);
        Assert.Equal(PlaybackErrorCode.HeaderValueTooLong, result.ErrorCode);
        Assert.DoesNotContain(value, result.SafeMessage, StringComparison.Ordinal);
    }

    [Theory]
    [InlineData(MediaHeaderScope.MainOnly)]
    [InlineData(MediaHeaderScope.SlaveOnly)]
    public void Build_RejectsPerSourceHeaderScopes(MediaHeaderScope scope)
    {
        var result = MediaLoadPlanBuilder.BuildFromHeaders(
            "https://media.example.test/video.mp4",
            "https://media.example.test/audio.mp4",
            [new MediaHeaderInput(MediaHeaderName.Referer, "https://ref.example.test/", scope)]);

        Assert.False(result.IsValid);
        Assert.Equal(PlaybackErrorCode.UnsupportedHeaderScope, result.ErrorCode);
    }

    [Theory]
    [InlineData("https://user:password@referrer.example.test/watch?token=secret")]
    [InlineData("http://referrer.example.test/watch")]
    [InlineData("https://referrer.example.test/watch#fragment")]
    public void Build_AppliesTheMediaUriPolicyToRefererWithoutLeakingIt(string referer)
    {
        var result = MediaLoadPlanBuilder.BuildFromHeaders(
            "https://media.example.test/video.mp4",
            rawAudioUri: null,
            [new MediaHeaderInput(MediaHeaderName.Referer, referer)]);

        Assert.False(result.IsValid);
        Assert.Equal(PlaybackErrorCode.UnsupportedHeader, result.ErrorCode);
        Assert.DoesNotContain("password", result.SafeMessage, StringComparison.OrdinalIgnoreCase);
        Assert.DoesNotContain("secret", result.SafeMessage, StringComparison.OrdinalIgnoreCase);
        Assert.DoesNotContain("referrer.example.test", result.SafeMessage, StringComparison.Ordinal);
    }

    [Fact]
    public void Build_AllowsExplicitLoopbackHttpForBothSourcesAndReferer()
    {
        var result = MediaLoadPlanBuilder.Build(
            "http://127.0.0.1:8080/video.mp4",
            "http://[::1]:8080/audio.mp4",
            "Referer: http://localhost:8080/watch",
            allowLoopbackHttp: true);

        Assert.True(result.IsValid);
        Assert.NotNull(result.Plan);
        Assert.Equal(MediaSourceMode.VideoWithAudioSlave, result.Plan.Mode);
        Assert.Equal(
            [":http-referrer=http://localhost:8080/watch"],
            result.Plan.LibVlcMediaOptions);
    }

    [Fact]
    public void Parse_CanonicalizesAllowedNamesAndIgnoresBlankLines()
    {
        var result = MediaHeaderInputParser.Parse(
            "\r\nREFERER: https://ref.example.test/\r\n" +
            "\r\nuser-agent: DoriyaSpike/1.0\r\n");

        Assert.True(result.IsValid);
        Assert.Equal(
            [MediaHeaderName.Referer, MediaHeaderName.UserAgent],
            result.Headers.Select(header => header.Name));
    }

    [Fact]
    public void Parse_RejectsMalformedHeaderLines()
    {
        var result = MediaHeaderInputParser.Parse("Referer https://ref.example.test/");

        Assert.False(result.IsValid);
        Assert.Equal(PlaybackErrorCode.InvalidHeaderInput, result.ErrorCode);
    }

    [Fact]
    public void Parse_RejectsOversizedRawHeaderInput()
    {
        var rawHeaders = new string('a', MediaHeaderInputParser.MaximumRawHeaderLength + 1);

        var result = MediaHeaderInputParser.Parse(rawHeaders);

        Assert.False(result.IsValid);
        Assert.Equal(PlaybackErrorCode.HeaderInputTooLong, result.ErrorCode);
    }

    [Fact]
    public void PlanAndHeaderToString_DoNotExposeUrlsOrSensitiveValues()
    {
        const string userAgent = "DoriyaSecretAgent/1.0";
        const string referer = "https://ref.example.test/watch?token=source-secret";
        var result = MediaLoadPlanBuilder.Build(
            "https://media.example.test/video.mp4?token=video-secret",
            "https://media.example.test/audio.mp4?token=audio-secret",
            $"Referer: {referer}\nUser-Agent: {userAgent}");

        Assert.True(result.IsValid);
        Assert.NotNull(result.Plan);
        var planText = result.Plan.ToString();
        var headerText = string.Join(
            "\n",
            result.Plan.Headers.Select(header => header.ToString()));

        foreach (var sensitive in new[]
                 {
                     "media.example.test",
                     "ref.example.test",
                     "video-secret",
                     "audio-secret",
                     "source-secret",
                     userAgent
                 })
        {
            Assert.DoesNotContain(sensitive, planText, StringComparison.Ordinal);
            Assert.DoesNotContain(sensitive, headerText, StringComparison.Ordinal);
        }
    }
}
