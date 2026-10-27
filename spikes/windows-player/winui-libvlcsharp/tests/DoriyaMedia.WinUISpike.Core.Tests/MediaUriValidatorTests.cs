using DoriyaMedia.WinUISpike.Core;
using Xunit;

namespace DoriyaMedia.WinUISpike.Core.Tests;

public sealed class MediaUriValidatorTests
{
    [Theory]
    [InlineData("https://media.example.test/video.m3u8")]
    [InlineData("https://127.0.0.1:8443/media.mp4?token=local-test")]
    [InlineData("https://[::1]:8443/media.mp4")]
    public void Validate_AcceptsHttpsUris(string uri)
    {
        var result = MediaUriValidator.Validate(uri);

        Assert.True(result.IsValid);
        Assert.False(string.IsNullOrWhiteSpace(result.MediaUri));
        Assert.DoesNotContain(uri, result.SafeMessage, StringComparison.Ordinal);
    }

    [Theory]
    [InlineData("http://media.example.test/video.mp4")]
    [InlineData("http://127.0.0.1:8080/media.mp4")]
    [InlineData("http://[::1]:8080/media.mp4")]
    [InlineData("http://localhost:8080/media.mp4")]
    public void Validate_RejectsHttpWhenLoopbackModeIsNotExplicitlyEnabled(string uri)
    {
        var result = MediaUriValidator.Validate(uri);

        Assert.False(result.IsValid);
        Assert.Null(result.MediaUri);
        Assert.Contains("HTTP", result.SafeMessage, StringComparison.OrdinalIgnoreCase);
    }

    [Theory]
    [InlineData("http://127.0.0.1:8080/media.mp4")]
    [InlineData("http://[::1]:8080/media.mp4")]
    [InlineData("http://LOCALHOST:8080/media.mp4")]
    public void Validate_AllowsExactLoopbackHttpOnlyWhenExplicitlyEnabled(string uri)
    {
        var result = MediaUriValidator.Validate(uri, allowLoopbackHttp: true);

        Assert.True(result.IsValid);
        Assert.False(string.IsNullOrWhiteSpace(result.MediaUri));
    }

    [Theory]
    [InlineData("http://192.0.2.1/media.mp4")]
    [InlineData("http://127.0.0.2/media.mp4")]
    [InlineData("http://[::2]/media.mp4")]
    [InlineData("http://[::ffff:127.0.0.1]/media.mp4")]
    [InlineData("http://foo.localhost/media.mp4")]
    [InlineData("http://localhost.evil/media.mp4")]
    [InlineData("http://localhost./media.mp4")]
    [InlineData("http://127.0.0.1.evil/media.mp4")]
    public void Validate_RejectsNonExactLoopbackHostsEvenInLoopbackMode(string uri)
    {
        var result = MediaUriValidator.Validate(uri, allowLoopbackHttp: true);

        Assert.False(result.IsValid);
        Assert.Null(result.MediaUri);
        Assert.Contains("loopback", result.SafeMessage, StringComparison.OrdinalIgnoreCase);
    }

    [Theory]
    [InlineData(null)]
    [InlineData("")]
    [InlineData("   ")]
    [InlineData("media.mp4")]
    [InlineData("/relative/media.mp4")]
    [InlineData("ftp://media.example.test/media.mp4")]
    [InlineData("file:///C:/media.mp4")]
    [InlineData("https://")]
    [InlineData("https://[::1")]
    [InlineData("https://media.example.test:notaport/media.mp4")]
    [InlineData("https:\\\\media.example.test\\media.mp4")]
    [InlineData("https://user:password@media.example.test/media.mp4")]
    [InlineData("http://user@127.0.0.1/media.mp4")]
    [InlineData("https://media.example.test/media.mp4#fragment")]
    [InlineData("https://media.example.test/media file.mp4")]
    [InlineData("https://media.example.test/media.mp4\r\nAuthorization: Bearer secret")]
    public void Validate_RejectsUnsafeOrUnsupportedValues(string? uri)
    {
        var result = MediaUriValidator.Validate(uri, allowLoopbackHttp: true);

        Assert.False(result.IsValid);
        Assert.Null(result.MediaUri);
        Assert.False(string.IsNullOrWhiteSpace(result.SafeMessage));
        Assert.DoesNotContain("password", result.SafeMessage, StringComparison.OrdinalIgnoreCase);
        Assert.DoesNotContain("secret", result.SafeMessage, StringComparison.OrdinalIgnoreCase);
    }

    [Fact]
    public void Validate_RejectsValuesAboveTheLengthLimit()
    {
        var uri = "https://media.example.test/" + new string('a', MediaUriValidator.MaximumUriLength);

        var result = MediaUriValidator.Validate(uri);

        Assert.False(result.IsValid);
        Assert.Contains("too long", result.SafeMessage, StringComparison.OrdinalIgnoreCase);
    }
}
