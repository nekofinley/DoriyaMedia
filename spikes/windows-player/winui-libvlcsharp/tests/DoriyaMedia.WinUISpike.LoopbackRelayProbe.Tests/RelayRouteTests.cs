using Xunit;

namespace DoriyaMedia.WinUISpike.LoopbackRelayProbe.Tests;

public sealed class RelayRouteTests
{
    private static readonly Uri Upstream = new("https://media.example.test/video.mp4");

    [Theory]
    [InlineData("Origin", " https://media.example.test")]
    [InlineData("Origin", "https://media.example.test/path")]
    [InlineData("Origin", "https://media.example.test?query=secret")]
    [InlineData("Origin", "https://user:secret@media.example.test")]
    [InlineData("Origin", "file://media.example.test")]
    [InlineData("Origin", "https:\\media.example.test")]
    [InlineData("Origin", "https://media.example.test/.")]
    [InlineData("Origin", "https://media.example.test/%2e%2e")]
    [InlineData("Origin", "https://media.example.test/")]
    [InlineData("Origin", "https://MEDIA.EXAMPLE.TEST")]
    [InlineData("Origin", "https://media.example.test:443")]
    [InlineData("Referer", "https://media.example.test/watch#secret")]
    [InlineData("Referer", "https://user:secret@media.example.test/watch")]
    [InlineData("Referer", "not a uri")]
    [InlineData("Referer", "ftp://media.example.test/watch")]
    [InlineData("Referer", "https:\\media.example.test/watch")]
    [InlineData("Referer", "https://media.example.test/a/../watch")]
    [InlineData("Referer", "https://media.example.test/watch%20later")]
    [InlineData("Referer", "https://media.example.test")]
    [InlineData("User-Agent", " agent ")]
    [InlineData("User-Agent", "agent\tvalue")]
    [InlineData("User-Agent", "agent\u007Fvalue")]
    [InlineData("User-Agent", "agent\u00A0value")]
    [InlineData("Host", "media.example.test")]
    [InlineData("Cookie", "session=secret")]
    [InlineData("Authorization", "Bearer secret")]
    public void InjectedHeaders_FailClosedForMalformedOrAmbiguousValues(
        string name,
        string value)
    {
        Assert.Throws<ArgumentException>(() =>
            new RelayRoute(
                Upstream,
                new Dictionary<string, string> { [name] = value }));
    }

    [Theory]
    [InlineData("Origin", "https://media.example.test")]
    [InlineData("Origin", "http://127.0.0.1:8080")]
    [InlineData("Origin", "https://[2001:db8::1]:8443")]
    [InlineData("Referer", "https://media.example.test/watch?episode=1")]
    [InlineData("Referer", "https://media.example.test/watch?episode=1&lang=zh")]
    [InlineData("Referer", "https://media.example.test/")]
    [InlineData("Referer", "http://127.0.0.1:8080/watch")]
    [InlineData("User-Agent", "DoriyaMedia.WinUISpike/1.0")]
    public void InjectedHeaders_AcceptReviewedValues(string name, string value)
    {
        var route = new RelayRoute(
            Upstream,
            new Dictionary<string, string> { [name] = value });

        Assert.Equal(value, route.InjectedHeaders[name]);
    }

    [Fact]
    public void InjectedHeaders_FailClosedForCaseInsensitiveDuplicates()
    {
        var headers = new Dictionary<string, string>(StringComparer.Ordinal)
        {
            ["Referer"] = "https://media.example.test/watch",
            ["referer"] = "https://other.example.test/watch",
        };

        Assert.Throws<ArgumentException>(() => new RelayRoute(Upstream, headers));
    }

    [Fact]
    public void PlainHttpTarget_IsAllowedOnlyForTheExactLoopbackFixture()
    {
        Assert.Throws<ArgumentException>(() =>
            new RelayRoute(new Uri("http://media.example.test/video.mp4")));

        var fixture = new RelayRoute(new Uri("http://127.0.0.1:8080/video.mp4"));
        Assert.Equal("http", fixture.UpstreamUri.Scheme);
    }
}
