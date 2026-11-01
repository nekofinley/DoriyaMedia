using System.Net;
using System.Net.Sockets;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Hosting;
using Microsoft.AspNetCore.Hosting.Server;
using Microsoft.AspNetCore.Hosting.Server.Features;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Logging;

namespace DoriyaMedia.WinUISpike.LoopbackRelayProbe;

public sealed class RelayServer : IAsyncDisposable
{
    private readonly WebApplication _application;
    private readonly HttpClient _client;
    private readonly string _capabilityToken;
    private int _disposed;

    private RelayServer(
        WebApplication application,
        HttpClient client,
        Uri baseAddress,
        string capabilityToken)
    {
        _application = application;
        _client = client;
        BaseAddress = baseAddress;
        _capabilityToken = capabilityToken;
    }

    public Uri BaseAddress { get; }

    public static async Task<RelayServer> StartAsync(
        RelayOptions options,
        CancellationToken cancellationToken = default)
    {
        ArgumentNullException.ThrowIfNull(options);

        var builder = WebApplication.CreateSlimBuilder();
        builder.Logging.ClearProviders();
        builder.WebHost.UseUrls("http://127.0.0.1:0");
        builder.WebHost.ConfigureKestrel(serverOptions =>
        {
            serverOptions.AddServerHeader = false;
            serverOptions.Limits.MaxRequestBodySize = 0;
            serverOptions.Limits.RequestHeadersTimeout = TimeSpan.FromSeconds(10);
        });

        var application = builder.Build();
        var client = new HttpClient(CreateHandler(options))
        {
            Timeout = Timeout.InfiniteTimeSpan,
        };
        var requestHandler = new RelayRequestHandler(options, client);
        application.Run(requestHandler.HandleAsync);

        try
        {
            await application.StartAsync(cancellationToken).ConfigureAwait(false);

            var server = application.Services.GetRequiredService<IServer>();
            var address = server.Features
                .Get<IServerAddressesFeature>()?
                .Addresses
                .SingleOrDefault();

            if (address is null)
            {
                throw new InvalidOperationException("The relay did not expose a listening address.");
            }

            var baseAddress = new Uri(
                address.EndsWith("/", StringComparison.Ordinal) ? address : $"{address}/",
                UriKind.Absolute);

            if (!string.Equals(baseAddress.Host, "127.0.0.1", StringComparison.Ordinal) ||
                baseAddress.Port <= 0)
            {
                throw new InvalidOperationException("The relay did not bind to an ephemeral IPv4 loopback port.");
            }

            return new RelayServer(application, client, baseAddress, options.CapabilityToken);
        }
        catch
        {
            client.Dispose();
            await application.DisposeAsync().ConfigureAwait(false);
            throw;
        }
    }

    public Uri GetClientUri(string role)
    {
        return role switch
        {
            RelayRoles.Video => new Uri(BaseAddress, $"{_capabilityToken}/{RelayRoles.Video}"),
            RelayRoles.Audio => new Uri(BaseAddress, $"{_capabilityToken}/{RelayRoles.Audio}"),
            _ => throw new ArgumentException("The stream role is not recognized.", nameof(role)),
        };
    }

    public async ValueTask DisposeAsync()
    {
        if (Interlocked.Exchange(ref _disposed, 1) != 0)
        {
            return;
        }

        try
        {
            using var stopTimeout = new CancellationTokenSource(TimeSpan.FromSeconds(5));
            try
            {
                await _application.StopAsync(stopTimeout.Token).ConfigureAwait(false);
            }
            catch (OperationCanceledException) when (stopTimeout.IsCancellationRequested)
            {
                // Dispose still tears down Kestrel if a request does not stop promptly.
            }
        }
        finally
        {
            _client.Dispose();
            await _application.DisposeAsync().ConfigureAwait(false);
        }
    }

    private static SocketsHttpHandler CreateHandler(RelayOptions options)
    {
        var handler = new SocketsHttpHandler
        {
            AllowAutoRedirect = false,
            AutomaticDecompression = DecompressionMethods.None,
            ConnectTimeout = options.UpstreamTimeout,
            PooledConnectionLifetime = TimeSpan.FromMinutes(2),
            UseCookies = false,
            UseProxy = false,
        };

        handler.ConnectCallback = (context, cancellationToken) =>
            ConnectAllowedAddressAsync(options.TargetPolicy, context, cancellationToken);
        return handler;
    }

    private static async ValueTask<Stream> ConnectAllowedAddressAsync(
        RelayTargetPolicy targetPolicy,
        SocketsHttpConnectionContext connectionContext,
        CancellationToken cancellationToken)
    {
        var host = connectionContext.DnsEndPoint.Host;
        IPAddress[] addresses;

        if (IPAddress.TryParse(host, out var literalAddress))
        {
            addresses = [literalAddress];
        }
        else
        {
            addresses = await Dns.GetHostAddressesAsync(host, cancellationToken).ConfigureAwait(false);
        }

        SocketException? lastSocketError = null;
        foreach (var address in addresses)
        {
            if (!targetPolicy.IsConnectAddressAllowed(host, address))
            {
                continue;
            }

            var socket = new Socket(address.AddressFamily, SocketType.Stream, ProtocolType.Tcp)
            {
                NoDelay = true,
            };

            try
            {
                await socket.ConnectAsync(
                    new IPEndPoint(address, connectionContext.DnsEndPoint.Port),
                    cancellationToken).ConfigureAwait(false);
                return new NetworkStream(socket, ownsSocket: true);
            }
            catch (SocketException exception)
            {
                lastSocketError = exception;
                socket.Dispose();
            }
            catch
            {
                socket.Dispose();
                throw;
            }
        }

        throw new HttpRequestException(
            "No permitted upstream address was reachable.",
            lastSocketError);
    }
}
