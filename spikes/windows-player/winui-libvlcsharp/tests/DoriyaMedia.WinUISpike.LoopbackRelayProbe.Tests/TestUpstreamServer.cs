using System.Collections.Concurrent;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Hosting;
using Microsoft.AspNetCore.Hosting.Server;
using Microsoft.AspNetCore.Hosting.Server.Features;
using Microsoft.AspNetCore.Http;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Logging;

namespace DoriyaMedia.WinUISpike.LoopbackRelayProbe.Tests;

internal sealed class TestUpstreamServer : IAsyncDisposable
{
    private readonly WebApplication _application;

    private TestUpstreamServer(WebApplication application, Uri baseAddress)
    {
        _application = application;
        BaseAddress = baseAddress;
    }

    public Uri BaseAddress { get; }

    public ConcurrentQueue<RecordedUpstreamRequest> Requests { get; } = new();

    public static async Task<TestUpstreamServer> StartAsync(
        Func<HttpContext, Task> handler,
        CancellationToken cancellationToken = default)
    {
        ArgumentNullException.ThrowIfNull(handler);

        var builder = WebApplication.CreateSlimBuilder();
        builder.Logging.ClearProviders();
        builder.WebHost.UseUrls("http://127.0.0.1:0");
        var application = builder.Build();
        TestUpstreamServer? server = null;

        application.Run(async context =>
        {
            server!.Record(context.Request);
            await handler(context);
        });

        try
        {
            await application.StartAsync(cancellationToken);
            var addresses = application.Services
                .GetRequiredService<IServer>()
                .Features
                .Get<IServerAddressesFeature>()?
                .Addresses;
            var address = addresses?.SingleOrDefault();

            if (address is null)
            {
                throw new InvalidOperationException("The test upstream did not expose an address.");
            }

            var baseAddress = new Uri(
                address.EndsWith("/", StringComparison.Ordinal) ? address : $"{address}/",
                UriKind.Absolute);
            server = new TestUpstreamServer(application, baseAddress);
            return server;
        }
        catch
        {
            await application.DisposeAsync();
            throw;
        }
    }

    public async ValueTask DisposeAsync()
    {
        try
        {
            using var stopTimeout = new CancellationTokenSource(TimeSpan.FromSeconds(5));
            try
            {
                await _application.StopAsync(stopTimeout.Token);
            }
            catch (OperationCanceledException) when (stopTimeout.IsCancellationRequested)
            {
                // Disposal below still releases the ephemeral port.
            }
        }
        finally
        {
            await _application.DisposeAsync();
        }
    }

    private void Record(HttpRequest request)
    {
        var headers = request.Headers.ToDictionary(
            static header => header.Key,
            static header => header.Value
                .Select(static value => value ?? string.Empty)
                .ToArray(),
            StringComparer.OrdinalIgnoreCase);
        Requests.Enqueue(new RecordedUpstreamRequest(
            request.Method,
            request.Path.Value ?? string.Empty,
            request.QueryString.Value ?? string.Empty,
            headers));
    }
}

internal sealed record RecordedUpstreamRequest(
    string Method,
    string Path,
    string QueryString,
    IReadOnlyDictionary<string, string[]> Headers);
