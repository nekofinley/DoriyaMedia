using System.Collections.Concurrent;
using System.Net;
using System.Net.Http.Headers;
using System.Text;
using Microsoft.AspNetCore.Http;
using Xunit;

namespace DoriyaMedia.WinUISpike.LoopbackRelayProbe.Tests;

public sealed class RelayServerTests
{
    [Fact]
    public async Task Routes_InjectIndependentHeaders_AndDropClientHeaders()
    {
        await using var upstream = await TestUpstreamServer.StartAsync(async context =>
        {
            context.Response.ContentType = "application/octet-stream";
            await context.Response.WriteAsync(
                context.Request.Path == "/video" ? "video-body" : "audio-body");
        });
        var targetPolicy = RelayTargetPolicy.CreateLoopbackFixture([upstream.BaseAddress]);
        var video = new RelayRoute(
            new Uri(upstream.BaseAddress, "video?source=video"),
            new Dictionary<string, string>
            {
                ["Origin"] = "https://video-origin.example.test",
                ["Referer"] = "https://video.example.test/",
                ["User-Agent"] = "video-route-agent",
            });
        var audio = new RelayRoute(
            new Uri(upstream.BaseAddress, "audio?source=audio"),
            new Dictionary<string, string>
            {
                ["Origin"] = "https://audio-origin.example.test",
                ["Referer"] = "https://audio.example.test/",
                ["User-Agent"] = "audio-route-agent",
            });
        var options = new RelayOptions(
            RelayCapabilityToken.Generate(),
            video,
            audio,
            targetPolicy);

        await using var relay = await RelayServer.StartAsync(options);
        using var client = CreateClient();

        using var videoRequest = new HttpRequestMessage(HttpMethod.Get, relay.GetClientUri(RelayRoles.Video));
        videoRequest.Headers.TryAddWithoutValidation("X-Client-Secret", "CLIENT_HEADER_SENTINEL");
        videoRequest.Headers.Authorization = new AuthenticationHeaderValue("Bearer", "CLIENT_AUTH_SENTINEL");
        videoRequest.Headers.TryAddWithoutValidation("Origin", "https://client-origin.example.test");
        videoRequest.Headers.TryAddWithoutValidation("Referer", "https://client-referer.example.test/");
        videoRequest.Headers.TryAddWithoutValidation("User-Agent", "client-user-agent");
        using var videoResponse = await client.SendAsync(videoRequest);

        using var audioResponse = await client.GetAsync(relay.GetClientUri(RelayRoles.Audio));

        Assert.Equal(HttpStatusCode.OK, videoResponse.StatusCode);
        Assert.Equal("application/octet-stream", videoResponse.Content.Headers.ContentType?.MediaType);
        Assert.Equal("video-body", await videoResponse.Content.ReadAsStringAsync());
        Assert.Equal(HttpStatusCode.OK, audioResponse.StatusCode);
        Assert.Equal("audio-body", await audioResponse.Content.ReadAsStringAsync());
        Assert.Equal("127.0.0.1", relay.BaseAddress.Host);
        Assert.NotEqual(0, relay.BaseAddress.Port);

        var requests = upstream.Requests.ToArray();
        Assert.Equal(2, requests.Length);
        var videoUpstreamRequest = Assert.Single(requests, request => request.Path == "/video");
        var audioUpstreamRequest = Assert.Single(requests, request => request.Path == "/audio");

        Assert.Equal("https://video-origin.example.test", videoUpstreamRequest.Headers["Origin"].Single());
        Assert.Equal("https://video.example.test/", videoUpstreamRequest.Headers["Referer"].Single());
        Assert.Equal("video-route-agent", videoUpstreamRequest.Headers["User-Agent"].Single());
        Assert.Equal("https://audio-origin.example.test", audioUpstreamRequest.Headers["Origin"].Single());
        Assert.Equal("https://audio.example.test/", audioUpstreamRequest.Headers["Referer"].Single());
        Assert.Equal("audio-route-agent", audioUpstreamRequest.Headers["User-Agent"].Single());
        Assert.False(videoUpstreamRequest.Headers.ContainsKey("X-Client-Secret"));
        Assert.False(videoUpstreamRequest.Headers.ContainsKey("Authorization"));
        Assert.Equal("?source=video", videoUpstreamRequest.QueryString);
        Assert.Equal("?source=audio", audioUpstreamRequest.QueryString);
    }

    [Fact]
    public async Task GetHeadRangeIfRangeAndTerminalStatuses_AreForwarded()
    {
        await using var upstream = await TestUpstreamServer.StartAsync(async context =>
        {
            context.Response.Headers.AcceptRanges = "bytes";
            context.Response.Headers.ETag = "\"fixture-v1\"";
            context.Response.Headers.LastModified = "Wed, 21 Oct 2015 07:28:00 GMT";

            var range = context.Request.Headers.Range.ToString();
            var ifRange = context.Request.Headers["If-Range"].ToString();
            if (HttpMethods.IsHead(context.Request.Method) && range == "bytes=10-19")
            {
                context.Response.StatusCode = StatusCodes.Status206PartialContent;
                context.Response.Headers.ContentRange = "bytes 10-19/100";
                context.Response.ContentLength = 10;
                return;
            }

            if (HttpMethods.IsHead(context.Request.Method) && range == "bytes=999-")
            {
                context.Response.StatusCode = StatusCodes.Status416RangeNotSatisfiable;
                context.Response.Headers.ContentRange = "bytes */100";
                context.Response.ContentLength = 0;
                return;
            }

            if (HttpMethods.IsHead(context.Request.Method))
            {
                context.Response.ContentLength = 100;
                return;
            }

            if (range == "bytes=10-19" && ifRange == "\"fixture-v1\"")
            {
                context.Response.StatusCode = StatusCodes.Status206PartialContent;
                context.Response.Headers.ContentRange = "bytes 10-19/100";
                context.Response.ContentLength = 10;
                await context.Response.WriteAsync("0123456789");
                return;
            }

            if (range == "bytes=999-")
            {
                context.Response.StatusCode = StatusCodes.Status416RangeNotSatisfiable;
                context.Response.Headers.ContentRange = "bytes */100";
                context.Response.ContentLength = 0;
                return;
            }

            context.Response.ContentLength = 100;
            await context.Response.WriteAsync(new string('x', 100));
        });
        var targetPolicy = RelayTargetPolicy.CreateLoopbackFixture([upstream.BaseAddress]);
        var options = new RelayOptions(
            RelayCapabilityToken.Generate(),
            new RelayRoute(new Uri(upstream.BaseAddress, "video")),
            new RelayRoute(new Uri(upstream.BaseAddress, "audio")),
            targetPolicy);

        await using var relay = await RelayServer.StartAsync(options);
        using var client = CreateClient();
        var videoUri = relay.GetClientUri(RelayRoles.Video);

        using var headRequest = new HttpRequestMessage(HttpMethod.Head, videoUri);
        using var headResponse = await client.SendAsync(headRequest);
        Assert.Equal(HttpStatusCode.OK, headResponse.StatusCode);
        Assert.Equal(100, headResponse.Content.Headers.ContentLength);
        Assert.Equal("bytes", headResponse.Headers.AcceptRanges.Single());
        Assert.Empty(await headResponse.Content.ReadAsByteArrayAsync());

        using var rangedHeadRequest = new HttpRequestMessage(HttpMethod.Head, videoUri);
        rangedHeadRequest.Headers.Range = new RangeHeaderValue(10, 19);
        using var rangedHeadResponse = await client.SendAsync(rangedHeadRequest);
        Assert.Equal(HttpStatusCode.PartialContent, rangedHeadResponse.StatusCode);
        Assert.Equal("bytes 10-19/100", rangedHeadResponse.Content.Headers.ContentRange?.ToString());
        Assert.Equal(10, rangedHeadResponse.Content.Headers.ContentLength);
        Assert.Empty(await rangedHeadResponse.Content.ReadAsByteArrayAsync());

        using var unsatisfiedHeadRequest = new HttpRequestMessage(HttpMethod.Head, videoUri);
        unsatisfiedHeadRequest.Headers.Range = new RangeHeaderValue(999, null);
        using var unsatisfiedHeadResponse = await client.SendAsync(unsatisfiedHeadRequest);
        Assert.Equal(HttpStatusCode.RequestedRangeNotSatisfiable, unsatisfiedHeadResponse.StatusCode);
        Assert.Equal("bytes */100", unsatisfiedHeadResponse.Content.Headers.ContentRange?.ToString());
        Assert.Empty(await unsatisfiedHeadResponse.Content.ReadAsByteArrayAsync());

        using var rangeRequest = new HttpRequestMessage(HttpMethod.Get, videoUri);
        rangeRequest.Headers.Range = new RangeHeaderValue(10, 19);
        rangeRequest.Headers.IfRange = new RangeConditionHeaderValue(new EntityTagHeaderValue("\"fixture-v1\""));
        using var rangeResponse = await client.SendAsync(rangeRequest);
        Assert.Equal(HttpStatusCode.PartialContent, rangeResponse.StatusCode);
        Assert.Equal("bytes 10-19/100", rangeResponse.Content.Headers.ContentRange?.ToString());
        Assert.Equal("0123456789", await rangeResponse.Content.ReadAsStringAsync());

        using var unsatisfiedRequest = new HttpRequestMessage(HttpMethod.Get, videoUri);
        unsatisfiedRequest.Headers.Range = new RangeHeaderValue(999, null);
        using var unsatisfiedResponse = await client.SendAsync(unsatisfiedRequest);
        Assert.Equal(HttpStatusCode.RequestedRangeNotSatisfiable, unsatisfiedResponse.StatusCode);
        Assert.Equal("bytes */100", unsatisfiedResponse.Content.Headers.ContentRange?.ToString());

        var requests = upstream.Requests.ToArray();
        Assert.Equal(5, requests.Length);
        Assert.Equal("HEAD", requests[0].Method);
        Assert.False(requests[0].Headers.ContainsKey("Range"));
        Assert.Equal("HEAD", requests[1].Method);
        Assert.Equal("bytes=10-19", requests[1].Headers["Range"].Single());
        Assert.Equal("HEAD", requests[2].Method);
        Assert.Equal("bytes=999-", requests[2].Headers["Range"].Single());
        Assert.Equal("GET", requests[3].Method);
        Assert.Equal("bytes=10-19", requests[3].Headers["Range"].Single());
        Assert.Equal("\"fixture-v1\"", requests[3].Headers["If-Range"].Single());
        Assert.Equal("GET", requests[4].Method);
        Assert.Equal("bytes=999-", requests[4].Headers["Range"].Single());
    }

    [Fact]
    public async Task MultiRangeHeader_IsForwardedAndReturnedWithoutAlteration()
    {
        await using var upstream = await TestUpstreamServer.StartAsync(async context =>
        {
            context.Response.StatusCode = StatusCodes.Status206PartialContent;
            context.Response.ContentType = "multipart/byteranges; boundary=probe";
            context.Response.ContentLength = 4;
            await context.Response.WriteAsync("body");
        });
        var options = new RelayOptions(
            RelayCapabilityToken.Generate(),
            new RelayRoute(new Uri(upstream.BaseAddress, "video")),
            new RelayRoute(new Uri(upstream.BaseAddress, "audio")),
            RelayTargetPolicy.CreateLoopbackFixture([upstream.BaseAddress]));

        await using var relay = await RelayServer.StartAsync(options);
        using var client = CreateClient();
        using var request = new HttpRequestMessage(HttpMethod.Get, relay.GetClientUri(RelayRoles.Video));
        Assert.True(request.Headers.TryAddWithoutValidation("Range", "bytes=0-1,3-4"));

        using var response = await client.SendAsync(request);

        Assert.Equal(HttpStatusCode.PartialContent, response.StatusCode);
        Assert.Equal("body", await response.Content.ReadAsStringAsync());
        Assert.Equal(
            "multipart/byteranges; boundary=probe",
            response.Content.Headers.ContentType?.ToString());
        var upstreamRequest = Assert.Single(upstream.Requests);
        Assert.Equal("bytes=0-1,3-4", upstreamRequest.Headers["Range"].Single());
    }

    [Fact]
    public async Task AllowedResponses_StripUnsafeResponseHeaders()
    {
        await using var upstream = await TestUpstreamServer.StartAsync(async context =>
        {
            context.Response.StatusCode = StatusCodes.Status200OK;
            context.Response.ContentType = "video/mp4";
            context.Response.Headers.CacheControl = "private, max-age=60";
            context.Response.Headers.Location = "https://unsafe.example.test/";
            context.Response.Headers.Append("Set-Cookie", "session=UNSAFE_COOKIE");
            context.Response.Headers.Append("WWW-Authenticate", "Basic realm=UNSAFE_REALM");
            context.Response.Headers.Append("X-Unsafe-Response", "UNSAFE_HEADER");
            context.Response.ContentLength = 4;
            await context.Response.WriteAsync("body");
        });
        var options = new RelayOptions(
            RelayCapabilityToken.Generate(),
            new RelayRoute(new Uri(upstream.BaseAddress, "video")),
            new RelayRoute(new Uri(upstream.BaseAddress, "audio")),
            RelayTargetPolicy.CreateLoopbackFixture([upstream.BaseAddress]));

        await using var relay = await RelayServer.StartAsync(options);
        using var client = CreateClient();
        using var response = await client.GetAsync(relay.GetClientUri(RelayRoles.Video));

        Assert.Equal(HttpStatusCode.OK, response.StatusCode);
        Assert.Equal("body", await response.Content.ReadAsStringAsync());
        Assert.Equal("video/mp4", response.Content.Headers.ContentType?.MediaType);
        Assert.True(response.Headers.CacheControl?.Private);
        Assert.Equal(TimeSpan.FromSeconds(60), response.Headers.CacheControl?.MaxAge);
        Assert.Null(response.Headers.Location);
        Assert.False(response.Headers.Contains("Set-Cookie"));
        Assert.False(response.Headers.Contains("WWW-Authenticate"));
        Assert.False(response.Headers.Contains("X-Unsafe-Response"));
    }

    [Fact]
    public async Task HeaderPhaseTimeout_ReturnsGatewayTimeout()
    {
        await using var upstream = await TestUpstreamServer.StartAsync(async context =>
        {
            try
            {
                await Task.Delay(TimeSpan.FromMilliseconds(1500), context.RequestAborted);
            }
            catch (OperationCanceledException)
            {
                return;
            }

            context.Response.StatusCode = StatusCodes.Status200OK;
        });
        var audit = new CapturingAuditSink();
        var options = new RelayOptions(
            RelayCapabilityToken.Generate(),
            new RelayRoute(new Uri(upstream.BaseAddress, "video")),
            new RelayRoute(new Uri(upstream.BaseAddress, "audio")),
            RelayTargetPolicy.CreateLoopbackFixture([upstream.BaseAddress]),
            upstreamTimeout: TimeSpan.FromMilliseconds(500),
            auditSink: audit);

        await using var relay = await RelayServer.StartAsync(options);
        using var client = CreateClient();
        using var request = new HttpRequestMessage(HttpMethod.Get, relay.GetClientUri(RelayRoles.Video));
        using var response = await client.SendAsync(request, HttpCompletionOption.ResponseHeadersRead);

        Assert.Equal(HttpStatusCode.GatewayTimeout, response.StatusCode);
        Assert.Contains("upstream_header_timeout", audit.Text, StringComparison.Ordinal);
    }

    [Fact]
    public async Task BodyIdleTimeout_DoesNotImposeATotalResponseDeadline()
    {
        const int chunkCount = 5;
        await using var upstream = await TestUpstreamServer.StartAsync(async context =>
        {
            context.Response.StatusCode = StatusCodes.Status200OK;
            context.Response.ContentType = "video/mp4";
            context.Response.ContentLength = chunkCount;
            for (var index = 0; index < chunkCount; index++)
            {
                await context.Response.WriteAsync("x");
                await context.Response.Body.FlushAsync();
                if (index < chunkCount - 1)
                {
                    await Task.Delay(TimeSpan.FromMilliseconds(300));
                }
            }
        });
        var options = new RelayOptions(
            RelayCapabilityToken.Generate(),
            new RelayRoute(new Uri(upstream.BaseAddress, "video")),
            new RelayRoute(new Uri(upstream.BaseAddress, "audio")),
            RelayTargetPolicy.CreateLoopbackFixture([upstream.BaseAddress]),
            upstreamTimeout: TimeSpan.FromSeconds(1));

        await using var relay = await RelayServer.StartAsync(options);
        using var client = CreateClient();
        using var response = await client.GetAsync(relay.GetClientUri(RelayRoles.Video));
        var body = await response.Content.ReadAsStringAsync();

        Assert.Equal(HttpStatusCode.OK, response.StatusCode);
        Assert.Equal(new string('x', chunkCount), body);
    }

    [Fact]
    public async Task BodyIdleTimeout_AbortsAStalledBody()
    {
        await using var upstream = await TestUpstreamServer.StartAsync(async context =>
        {
            context.Response.StatusCode = StatusCodes.Status200OK;
            context.Response.ContentType = "video/mp4";
            context.Response.ContentLength = 2;
            await context.Response.WriteAsync("x");
            await context.Response.Body.FlushAsync();
            try
            {
                await Task.Delay(TimeSpan.FromSeconds(3), context.RequestAborted);
            }
            catch (OperationCanceledException)
            {
                return;
            }

            await context.Response.WriteAsync("y");
        });
        var audit = new CapturingAuditSink();
        var options = new RelayOptions(
            RelayCapabilityToken.Generate(),
            new RelayRoute(new Uri(upstream.BaseAddress, "video")),
            new RelayRoute(new Uri(upstream.BaseAddress, "audio")),
            RelayTargetPolicy.CreateLoopbackFixture([upstream.BaseAddress]),
            upstreamTimeout: TimeSpan.FromMilliseconds(750),
            auditSink: audit);

        await using var relay = await RelayServer.StartAsync(options);
        using var client = CreateClient();
        using var request = new HttpRequestMessage(HttpMethod.Get, relay.GetClientUri(RelayRoles.Video));
        using var response = await client.SendAsync(request, HttpCompletionOption.ResponseHeadersRead);

        Assert.Equal(HttpStatusCode.OK, response.StatusCode);
        await Assert.ThrowsAnyAsync<Exception>(() => response.Content.ReadAsByteArrayAsync());

        var deadline = DateTime.UtcNow.AddSeconds(3);
        while (!audit.Text.Contains("upstream_body_idle_timeout", StringComparison.Ordinal) &&
               DateTime.UtcNow < deadline)
        {
            await Task.Delay(25);
        }

        Assert.Contains("upstream_body_idle_timeout", audit.Text, StringComparison.Ordinal);
    }

    [Fact]
    public async Task ChunkedBodyIdleTimeout_ResetsTheClientConnection()
    {
        await using var upstream = await TestUpstreamServer.StartAsync(async context =>
        {
            context.Response.StatusCode = StatusCodes.Status200OK;
            context.Response.ContentType = "video/mp4";
            await context.Response.WriteAsync("partial");
            await context.Response.Body.FlushAsync();
            try
            {
                await Task.Delay(TimeSpan.FromSeconds(3), context.RequestAborted);
            }
            catch (OperationCanceledException)
            {
                return;
            }

            await context.Response.WriteAsync("rest");
        });
        var audit = new CapturingAuditSink();
        var options = new RelayOptions(
            RelayCapabilityToken.Generate(),
            new RelayRoute(new Uri(upstream.BaseAddress, "video")),
            new RelayRoute(new Uri(upstream.BaseAddress, "audio")),
            RelayTargetPolicy.CreateLoopbackFixture([upstream.BaseAddress]),
            upstreamTimeout: TimeSpan.FromMilliseconds(750),
            auditSink: audit);

        await using var relay = await RelayServer.StartAsync(options);
        using var client = CreateClient();
        using var request = new HttpRequestMessage(HttpMethod.Get, relay.GetClientUri(RelayRoles.Video));
        using var response = await client.SendAsync(request, HttpCompletionOption.ResponseHeadersRead);

        Assert.Equal(HttpStatusCode.OK, response.StatusCode);
        Assert.Null(response.Content.Headers.ContentLength);
        await Assert.ThrowsAnyAsync<Exception>(() => response.Content.ReadAsByteArrayAsync());

        var deadline = DateTime.UtcNow.AddSeconds(3);
        while (!audit.Text.Contains("upstream_body_idle_timeout", StringComparison.Ordinal) &&
               DateTime.UtcNow < deadline)
        {
            await Task.Delay(25);
        }

        Assert.Contains("upstream_body_idle_timeout", audit.Text, StringComparison.Ordinal);
    }

    [Fact]
    public async Task CompressedUpstreamResponse_IsRejectedWithoutForwardingBytes()
    {
        await using var upstream = await TestUpstreamServer.StartAsync(async context =>
        {
            context.Response.StatusCode = StatusCodes.Status200OK;
            context.Response.ContentType = "video/mp4";
            context.Response.Headers.ContentEncoding = "gzip";
            context.Response.ContentLength = 4;
            await context.Response.WriteAsync("body");
        });
        var audit = new CapturingAuditSink();
        var options = new RelayOptions(
            RelayCapabilityToken.Generate(),
            new RelayRoute(new Uri(upstream.BaseAddress, "video")),
            new RelayRoute(new Uri(upstream.BaseAddress, "audio")),
            RelayTargetPolicy.CreateLoopbackFixture([upstream.BaseAddress]),
            auditSink: audit);

        await using var relay = await RelayServer.StartAsync(options);
        using var client = CreateClient();
        using var response = await client.GetAsync(relay.GetClientUri(RelayRoles.Video));

        Assert.Equal(HttpStatusCode.BadGateway, response.StatusCode);
        Assert.Empty(response.Content.Headers.ContentEncoding);
        Assert.DoesNotContain("body", await response.Content.ReadAsStringAsync(), StringComparison.Ordinal);
        Assert.Contains("upstream_encoding_rejected", audit.Text, StringComparison.Ordinal);
    }

    [Fact]
    public async Task RejectionPaths_DoNotContactUpstreamOrLeakRequestValuesToAudit()
    {
        await using var upstream = await TestUpstreamServer.StartAsync(context =>
        {
            context.Response.StatusCode = StatusCodes.Status200OK;
            return Task.CompletedTask;
        });
        var audit = new CapturingAuditSink();
        var token = RelayCapabilityToken.Generate();
        var options = new RelayOptions(
            token,
            new RelayRoute(new Uri(upstream.BaseAddress, "video")),
            new RelayRoute(new Uri(upstream.BaseAddress, "audio")),
            RelayTargetPolicy.CreateLoopbackFixture([upstream.BaseAddress]),
            auditSink: audit);

        await using var relay = await RelayServer.StartAsync(options);
        using var client = CreateClient();

        using var wrongToken = await client.GetAsync(
            new Uri(relay.BaseAddress, $"{RelayCapabilityToken.Generate()}/video"));
        using var unknownRole = await client.GetAsync(
            new Uri(relay.BaseAddress, $"{token}/unknown"));

        using var queryRequest = new HttpRequestMessage(
            HttpMethod.Get,
            new Uri(relay.BaseAddress, $"{token}/video?secret=QUERY_SENTINEL"));
        queryRequest.Headers.TryAddWithoutValidation("X-Audit-Secret", "HEADER_SENTINEL");
        using var queryResponse = await client.SendAsync(queryRequest);

        using var hostRequest = new HttpRequestMessage(HttpMethod.Get, relay.GetClientUri(RelayRoles.Video));
        hostRequest.Headers.TryAddWithoutValidation("Host", $"localhost:{relay.BaseAddress.Port}");
        using var hostResponse = await client.SendAsync(hostRequest);

        using var postResponse = await client.PostAsync(
            relay.GetClientUri(RelayRoles.Video),
            new StringContent("ignored"));

        Assert.Equal(HttpStatusCode.NotFound, wrongToken.StatusCode);
        Assert.Equal(HttpStatusCode.NotFound, unknownRole.StatusCode);
        Assert.Equal(HttpStatusCode.BadRequest, queryResponse.StatusCode);
        Assert.Equal(HttpStatusCode.BadRequest, hostResponse.StatusCode);
        Assert.Equal(HttpStatusCode.MethodNotAllowed, postResponse.StatusCode);
        Assert.Equal("GET, HEAD", postResponse.Content.Headers.Allow.ToString());
        Assert.Empty(upstream.Requests);

        var auditText = audit.Text;
        Assert.DoesNotContain("QUERY_SENTINEL", auditText, StringComparison.Ordinal);
        Assert.DoesNotContain("HEADER_SENTINEL", auditText, StringComparison.Ordinal);
        Assert.DoesNotContain(token, auditText, StringComparison.Ordinal);
        Assert.Contains("host_rejected", auditText, StringComparison.Ordinal);
        Assert.Contains("query_rejected", auditText, StringComparison.Ordinal);
        Assert.Contains("route_not_found", auditText, StringComparison.Ordinal);
        Assert.Contains("method_rejected", auditText, StringComparison.Ordinal);
    }

    [Theory]
    [InlineData(301, "GET")]
    [InlineData(302, "GET")]
    [InlineData(303, "GET")]
    [InlineData(307, "GET")]
    [InlineData(308, "GET")]
    [InlineData(302, "HEAD")]
    public async Task CrossOriginRedirect_IsFailClosedAndNotFollowed(int upstreamStatus, string method)
    {
        await using var redirectTarget = await TestUpstreamServer.StartAsync(context =>
        {
            context.Response.StatusCode = StatusCodes.Status200OK;
            return Task.CompletedTask;
        });
        await using var upstream = await TestUpstreamServer.StartAsync(context =>
        {
            context.Response.StatusCode = upstreamStatus;
            context.Response.Headers.Location =
                new Uri(redirectTarget.BaseAddress, "steal?value=REDIRECT_SENTINEL").ToString();
            context.Response.Headers.Append("Set-Cookie", "fixture=REDIRECT_COOKIE");
            return Task.CompletedTask;
        });
        var audit = new CapturingAuditSink();
        var options = new RelayOptions(
            RelayCapabilityToken.Generate(),
            new RelayRoute(new Uri(upstream.BaseAddress, "video")),
            new RelayRoute(new Uri(upstream.BaseAddress, "audio")),
            RelayTargetPolicy.CreateLoopbackFixture([upstream.BaseAddress]),
            auditSink: audit);

        await using var relay = await RelayServer.StartAsync(options);
        using var client = CreateClient();
        using var request = new HttpRequestMessage(new HttpMethod(method), relay.GetClientUri(RelayRoles.Video));
        using var response = await client.SendAsync(request);

        Assert.Equal(HttpStatusCode.BadGateway, response.StatusCode);
        Assert.Null(response.Headers.Location);
        Assert.False(response.Headers.Contains("Set-Cookie"));
        Assert.Empty(redirectTarget.Requests);
        Assert.Contains("redirect_rejected", audit.Text, StringComparison.Ordinal);
        Assert.DoesNotContain("REDIRECT_SENTINEL", audit.Text, StringComparison.Ordinal);
        Assert.DoesNotContain("REDIRECT_COOKIE", audit.Text, StringComparison.Ordinal);
    }

    [Fact]
    public async Task SameOriginRedirect_IsAlsoFailClosed()
    {
        await using var upstream = await TestUpstreamServer.StartAsync(context =>
        {
            if (context.Request.Path == "/video")
            {
                context.Response.StatusCode = StatusCodes.Status302Found;
                context.Response.Headers.Location = "/video-followed";
            }
            else
            {
                context.Response.StatusCode = StatusCodes.Status200OK;
            }

            return Task.CompletedTask;
        });
        var options = new RelayOptions(
            RelayCapabilityToken.Generate(),
            new RelayRoute(new Uri(upstream.BaseAddress, "video")),
            new RelayRoute(new Uri(upstream.BaseAddress, "audio")),
            RelayTargetPolicy.CreateLoopbackFixture([upstream.BaseAddress]));

        await using var relay = await RelayServer.StartAsync(options);
        using var client = CreateClient();
        using var response = await client.GetAsync(relay.GetClientUri(RelayRoles.Video));

        Assert.Equal(HttpStatusCode.BadGateway, response.StatusCode);
        var request = Assert.Single(upstream.Requests);
        Assert.Equal("/video", request.Path);
    }

    [Fact]
    public void TargetPolicy_RejectsPrivateUnlistedAndCrossOriginTargets()
    {
        var loopbackOrigin = new Uri("http://127.0.0.1:32100/");
        var differentLoopbackOrigin = new Uri("http://127.0.0.1:32101/");
        var publicOrigin = new Uri("https://media.example.test/");

        Assert.Throws<ArgumentException>(() =>
            RelayTargetPolicy.CreateExactOrigins([loopbackOrigin]));
        Assert.Throws<ArgumentException>(() =>
            RelayTargetPolicy.CreateExactOrigins([new Uri("http://192.168.1.10:8080/")]));
        Assert.Throws<ArgumentException>(() =>
            RelayTargetPolicy.CreateExactOrigins([new Uri("http://[64:ff9b::a00:1]/")]));
        Assert.Throws<ArgumentException>(() =>
            RelayTargetPolicy.CreateExactOrigins([new Uri("http://[2002:a00:1::]/")]));
        Assert.Throws<ArgumentException>(() =>
            RelayTargetPolicy.CreateExactOrigins([new Uri("http://[::ffff:10.0.0.1]/")]));
        Assert.Throws<ArgumentException>(() =>
            RelayTargetPolicy.CreateExactOrigins([new Uri("http://media.example.test/")]));
        Assert.Throws<ArgumentException>(() =>
            RelayTargetPolicy.CreateExactOrigins([new Uri("https://192.0.2.1/")]));
        Assert.Throws<ArgumentException>(() =>
            RelayTargetPolicy.CreateExactOrigins([new Uri("https://198.51.100.1/")]));
        Assert.Throws<ArgumentException>(() =>
            RelayTargetPolicy.CreateExactOrigins([new Uri("https://203.0.113.1/")]));

        var fixturePolicy = RelayTargetPolicy.CreateLoopbackFixture([loopbackOrigin]);
        fixturePolicy.Validate(new Uri("http://127.0.0.1:32100/media.mp4?source=fixture"));
        Assert.True(fixturePolicy.IsConnectAddressAllowed("127.0.0.1", IPAddress.Loopback));
        Assert.False(fixturePolicy.IsConnectAddressAllowed("fixture.example.test", IPAddress.Loopback));
        Assert.False(fixturePolicy.IsConnectAddressAllowed("127.0.0.1", IPAddress.Parse("10.0.0.1")));
        Assert.Throws<ArgumentException>(() =>
            fixturePolicy.Validate(differentLoopbackOrigin));
        Assert.Throws<ArgumentException>(() =>
            fixturePolicy.Validate(new Uri("http://localhost:32100/media.mp4")));

        var publicPolicy = RelayTargetPolicy.CreateExactOrigins([publicOrigin]);
        publicPolicy.Validate(new Uri("https://media.example.test/video.m3u8"));
        Assert.False(publicPolicy.IsConnectAddressAllowed("media.example.test", IPAddress.Parse("192.168.1.20")));
        Assert.False(publicPolicy.IsConnectAddressAllowed("media.example.test", IPAddress.Parse("169.254.169.254")));
        Assert.False(publicPolicy.IsConnectAddressAllowed("media.example.test", IPAddress.Parse("64:ff9b::a00:1")));
        Assert.False(publicPolicy.IsConnectAddressAllowed("media.example.test", IPAddress.Parse("64:ff9b:1::a00:1")));
        Assert.False(publicPolicy.IsConnectAddressAllowed("media.example.test", IPAddress.Parse("2002:a00:1::")));
        Assert.False(publicPolicy.IsConnectAddressAllowed("media.example.test", IPAddress.Parse("::a00:1")));
        Assert.False(publicPolicy.IsConnectAddressAllowed("media.example.test", IPAddress.Parse("::ffff:10.0.0.1")));
        Assert.False(publicPolicy.IsConnectAddressAllowed("media.example.test", IPAddress.Parse("::ffff:203.0.113.10")));
        Assert.False(publicPolicy.IsConnectAddressAllowed("media.example.test", IPAddress.Parse("2001::1")));
        Assert.False(publicPolicy.IsConnectAddressAllowed("media.example.test", IPAddress.Parse("192.0.2.1")));
        Assert.False(publicPolicy.IsConnectAddressAllowed("media.example.test", IPAddress.Parse("198.51.100.1")));
        Assert.False(publicPolicy.IsConnectAddressAllowed("media.example.test", IPAddress.Parse("203.0.113.10")));
        Assert.False(publicPolicy.IsConnectAddressAllowed("media.example.test", IPAddress.Parse("2001:db8::1")));
        Assert.False(publicPolicy.IsConnectAddressAllowed("media.example.test", IPAddress.Parse("2d00::1")));
        Assert.False(publicPolicy.IsConnectAddressAllowed("media.example.test", IPAddress.Parse("3000::1")));
        Assert.False(publicPolicy.IsConnectAddressAllowed("media.example.test", IPAddress.Parse("3fff::1")));
        Assert.False(publicPolicy.IsConnectAddressAllowed("media.example.test", IPAddress.Parse("3fff:1000::1")));
        Assert.False(publicPolicy.IsConnectAddressAllowed("media.example.test", IPAddress.Parse("5f00::1")));
        Assert.True(publicPolicy.IsConnectAddressAllowed("media.example.test", IPAddress.Parse("8.8.8.8")));
        Assert.True(publicPolicy.IsConnectAddressAllowed("media.example.test", IPAddress.Parse("2001:4860:4860::8888")));
        Assert.True(publicPolicy.IsConnectAddressAllowed("media.example.test", IPAddress.Parse("2606:4700:4700::1111")));
        Assert.Throws<ArgumentException>(() =>
            publicPolicy.Validate(new Uri("https://other.example.test/video.m3u8")));
    }

    [Theory]
    [InlineData("2003::1", true)]
    [InlineData("2001:200::1", true)]
    [InlineData("2001:3ff::1", true)]
    [InlineData("2001:bff::1", true)]
    [InlineData("2001:3fff::1", true)]
    [InlineData("2001:4dff::1", true)]
    [InlineData("2001:5fff::1", true)]
    [InlineData("2001:8000::1", true)]
    [InlineData("2001:8800::1", true)]
    [InlineData("2001:9fff::1", true)]
    [InlineData("2001:bfff::1", true)]
    [InlineData("2003:3fff::1", true)]
    [InlineData("2606:4700:4700::1111", true)]
    [InlineData("2400::1", true)]
    [InlineData("240f:ffff::1", true)]
    [InlineData("2410::1", true)]
    [InlineData("241f:ffff::1", true)]
    [InlineData("260f:ffff::1", true)]
    [InlineData("2610:1ff::1", true)]
    [InlineData("2620:1ff::1", true)]
    [InlineData("263f:ffff::1", true)]
    [InlineData("280f:ffff::1", true)]
    [InlineData("2a0f:ffff::1", true)]
    [InlineData("2a1f:ffff::1", true)]
    [InlineData("2c0f:ffff::1", true)]
    [InlineData("26ff::1", false)]
    [InlineData("24ff::1", false)]
    [InlineData("2003:4000::1", false)]
    [InlineData("2001:1000::1", false)]
    [InlineData("2001:4e00::1", false)]
    [InlineData("2001:6000::1", false)]
    [InlineData("2001:c000::1", false)]
    [InlineData("2001:db8::1", false)]
    [InlineData("2002::1", false)]
    [InlineData("2612::1", false)]
    [InlineData("2622::1", false)]
    [InlineData("28ff::1", false)]
    [InlineData("2aff::1", false)]
    [InlineData("2cff::1", false)]
    public void TargetPolicy_UsesExactIanaIpv6AllocationPrefixes(string address, bool expected)
    {
        var policy = RelayTargetPolicy.CreateExactOrigins([new Uri("https://media.example.test/")]);

        Assert.Equal(
            expected,
            policy.IsConnectAddressAllowed("media.example.test", IPAddress.Parse(address)));
    }

    [Fact]
    public void CapabilityTokens_AreRandomAndWellFormed()
    {
        var first = RelayCapabilityToken.Generate();
        var second = RelayCapabilityToken.Generate();

        Assert.True(RelayCapabilityToken.IsWellFormed(first));
        Assert.True(RelayCapabilityToken.IsWellFormed(second));
        Assert.Equal(RelayCapabilityToken.EncodedLength, first.Length);
        Assert.NotEqual(first, second);
        Assert.False(RelayCapabilityToken.IsWellFormed(first + "!"));
    }

    private static HttpClient CreateClient()
    {
        return new HttpClient(new HttpClientHandler
        {
            AllowAutoRedirect = false,
            AutomaticDecompression = DecompressionMethods.None,
            UseCookies = false,
            UseProxy = false,
        });
    }

    private sealed class CapturingAuditSink : IRelayAuditSink
    {
        private readonly ConcurrentQueue<RelayAuditEvent> _events = new();

        public string Text => string.Join(
            '\n',
            _events.Select(static entry =>
                $"{entry.Method}|{entry.Role}|{entry.Outcome}|{entry.StatusCode}"));

        public void Record(RelayAuditEvent auditEvent)
        {
            _events.Enqueue(auditEvent);
        }
    }
}
