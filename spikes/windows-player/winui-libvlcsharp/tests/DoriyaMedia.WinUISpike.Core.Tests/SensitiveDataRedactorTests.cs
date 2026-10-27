using DoriyaMedia.WinUISpike.Core;
using Xunit;

namespace DoriyaMedia.WinUISpike.Core.Tests;

public sealed class SensitiveDataRedactorTests
{
    [Fact]
    public void Redact_RemovesUriHeadersBearerValuesAndQuerySecrets()
    {
        const string input =
            "GET https://cdn.example.test/video.m3u8?token=top-secret&part=1\r\n" +
            "Authorization: Bearer header-secret\r\n" +
            "Cookie: SESSION=cookie-secret\r\n" +
            "Referer: https://referrer.example.test/path";

        var output = SensitiveDataRedactor.Redact(input);

        Assert.Contains("<redacted-uri>", output, StringComparison.Ordinal);
        Assert.Contains("Authorization: <redacted>", output, StringComparison.Ordinal);
        Assert.Contains("Cookie: <redacted>", output, StringComparison.Ordinal);
        Assert.Contains("Referer: <redacted>", output, StringComparison.Ordinal);
        Assert.DoesNotContain("top-secret", output, StringComparison.Ordinal);
        Assert.DoesNotContain("header-secret", output, StringComparison.Ordinal);
        Assert.DoesNotContain("cookie-secret", output, StringComparison.Ordinal);
        Assert.DoesNotContain("referrer.example.test", output, StringComparison.Ordinal);
    }

    [Fact]
    public void Redact_RemovesStandaloneBearerAndQueryValues()
    {
        const string input = "token=query-secret; Authorization=Bearer standalone-secret";

        var output = SensitiveDataRedactor.Redact(input);

        Assert.DoesNotContain("query-secret", output, StringComparison.Ordinal);
        Assert.DoesNotContain("standalone-secret", output, StringComparison.Ordinal);
    }

    [Fact]
    public void RedactingDiagnosticWriter_OnlyPassesSanitizedTextToTheSink()
    {
        var sink = new CapturingSink();
        var writer = new RedactingDiagnosticWriter(sink);
        var exception = new InvalidOperationException(
            "open https://cdn.example.test/video?token=secret failed; Cookie: SESSION=secret");

        writer.WriteException("playback", exception);

        var line = Assert.Single(sink.Lines);
        Assert.Contains("<redacted-uri>", line, StringComparison.Ordinal);
        Assert.Contains("Cookie: <redacted>", line, StringComparison.Ordinal);
        Assert.DoesNotContain("cdn.example.test", line, StringComparison.Ordinal);
        Assert.DoesNotContain("SESSION=secret", line, StringComparison.Ordinal);
    }

    private sealed class CapturingSink : IDiagnosticSink
    {
        public List<string> Lines { get; } = [];

        public void WriteLine(string line) => Lines.Add(line);
    }
}
