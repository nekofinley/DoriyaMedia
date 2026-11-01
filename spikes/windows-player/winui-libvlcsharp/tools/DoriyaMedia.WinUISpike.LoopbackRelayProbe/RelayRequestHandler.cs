using System.Buffers;
using System.Net;
using Microsoft.AspNetCore.Http;
using Microsoft.Extensions.Primitives;

namespace DoriyaMedia.WinUISpike.LoopbackRelayProbe;

internal sealed class RelayRequestHandler
{
    private const int MaximumForwardedConditionalHeaderLength = 8192;

    private static readonly HashSet<string> AllowedResponseHeaders = new(
        [
            "Accept-Ranges",
            "Cache-Control",
            "Content-Length",
            "Content-Range",
            "Content-Type",
            "ETag",
            "Expires",
            "Last-Modified",
            "Vary",
        ],
        StringComparer.OrdinalIgnoreCase);

    private readonly RelayOptions _options;
    private readonly HttpClient _client;

    public RelayRequestHandler(RelayOptions options, HttpClient client)
    {
        _options = options;
        _client = client;
    }

    public async Task HandleAsync(HttpContext context)
    {
        var method = AuditMethod(context.Request.Method);

        if (!HasExactLoopbackHost(context))
        {
            await RejectAsync(context, method, "unknown", StatusCodes.Status400BadRequest, "host_rejected", "Bad request.").ConfigureAwait(false);
            return;
        }

        if (context.Request.QueryString.HasValue)
        {
            await RejectAsync(context, method, "unknown", StatusCodes.Status400BadRequest, "query_rejected", "Bad request.").ConfigureAwait(false);
            return;
        }

        if (!TryParsePath(context.Request.Path, out var suppliedToken, out var suppliedRole) ||
            !RelayCapabilityToken.FixedTimeEquals(_options.CapabilityToken, suppliedToken))
        {
            await RejectAsync(context, method, "unknown", StatusCodes.Status404NotFound, "route_not_found", "Not found.").ConfigureAwait(false);
            return;
        }

        if (!_options.TryGetRoute(suppliedRole, out var route) || route is null)
        {
            await RejectAsync(context, method, "unknown", StatusCodes.Status404NotFound, "route_not_found", "Not found.").ConfigureAwait(false);
            return;
        }

        if (!HttpMethods.IsGet(context.Request.Method) && !HttpMethods.IsHead(context.Request.Method))
        {
            context.Response.Headers.Allow = "GET, HEAD";
            await RejectAsync(context, method, suppliedRole, StatusCodes.Status405MethodNotAllowed, "method_rejected", "Method not allowed.").ConfigureAwait(false);
            return;
        }

        await ProxyAsync(context, method, suppliedRole, route).ConfigureAwait(false);
    }

    private async Task ProxyAsync(HttpContext context, string method, string role, RelayRoute route)
    {
        using var upstreamRequest = new HttpRequestMessage(
            new HttpMethod(context.Request.Method),
            route.UpstreamUri);

        foreach (var (name, value) in route.InjectedHeaders)
        {
            upstreamRequest.Headers.TryAddWithoutValidation(name, value);
        }

        if (!TryCopyConditionalHeader(context, "Range", upstreamRequest) ||
            !TryCopyConditionalHeader(context, "If-Range", upstreamRequest))
        {
            await RejectAsync(context, method, role, StatusCodes.Status400BadRequest, "range_rejected", "Bad request.").ConfigureAwait(false);
            return;
        }

        HttpResponseMessage upstreamResponse;
        using (var headerTimeout = new CancellationTokenSource(_options.UpstreamTimeout))
        using (var headerCancellation = CancellationTokenSource.CreateLinkedTokenSource(
            context.RequestAborted,
            headerTimeout.Token))
        {
            try
            {
                upstreamResponse = await _client.SendAsync(
                    upstreamRequest,
                    HttpCompletionOption.ResponseHeadersRead,
                    headerCancellation.Token).ConfigureAwait(false);
            }
            catch (OperationCanceledException) when (context.RequestAborted.IsCancellationRequested)
            {
                Audit(method, role, "client_aborted", 499);
                return;
            }
            catch (OperationCanceledException)
            {
                await RejectAsync(context, method, role, StatusCodes.Status504GatewayTimeout, "upstream_header_timeout", "Upstream timed out.").ConfigureAwait(false);
                return;
            }
            catch (TimeoutException)
            {
                await RejectAsync(context, method, role, StatusCodes.Status504GatewayTimeout, "upstream_header_timeout", "Upstream timed out.").ConfigureAwait(false);
                return;
            }
            catch (HttpRequestException)
            {
                await RejectAsync(context, method, role, StatusCodes.Status502BadGateway, "upstream_failed", "Upstream request failed.").ConfigureAwait(false);
                return;
            }
            catch (IOException)
            {
                await RejectAsync(context, method, role, StatusCodes.Status502BadGateway, "upstream_failed", "Upstream request failed.").ConfigureAwait(false);
                return;
            }
        }

        using (upstreamResponse)
        {
            var upstreamStatus = (int)upstreamResponse.StatusCode;
            if (upstreamStatus is >= 300 and < 400)
            {
                await RejectAsync(context, method, role, StatusCodes.Status502BadGateway, "redirect_rejected", "Upstream redirect rejected.").ConfigureAwait(false);
                return;
            }

            if (upstreamStatus is not StatusCodes.Status200OK and
                not StatusCodes.Status206PartialContent and
                not StatusCodes.Status416RangeNotSatisfiable)
            {
                await RejectAsync(context, method, role, StatusCodes.Status502BadGateway, "upstream_status_rejected", "Upstream response rejected.").ConfigureAwait(false);
                return;
            }

            if (upstreamResponse.Content.Headers.ContentEncoding.Any(
                    static encoding => !string.Equals(encoding, "identity", StringComparison.OrdinalIgnoreCase)))
            {
                await RejectAsync(context, method, role, StatusCodes.Status502BadGateway, "upstream_encoding_rejected", "Upstream response encoding rejected.").ConfigureAwait(false);
                return;
            }

            context.Response.StatusCode = upstreamStatus;
            CopyAllowedResponseHeaders(upstreamResponse, context.Response);
            context.Response.Headers["X-Content-Type-Options"] = "nosniff";

            if (!HttpMethods.IsHead(context.Request.Method))
            {
                try
                {
                    await using var upstreamBody = await upstreamResponse.Content
                        .ReadAsStreamAsync(context.RequestAborted)
                        .ConfigureAwait(false);
                    await CopyResponseBodyWithIdleTimeoutAsync(
                        upstreamBody,
                        context.Response.Body,
                        _options.UpstreamTimeout,
                        context.RequestAborted).ConfigureAwait(false);
                }
                catch (OperationCanceledException) when (context.RequestAborted.IsCancellationRequested)
                {
                    Audit(method, role, "client_aborted", 499);
                    return;
                }
                catch (TimeoutException)
                {
                    if (!context.Response.HasStarted)
                    {
                        await RejectAsync(context, method, role, StatusCodes.Status504GatewayTimeout, "upstream_body_idle_timeout", "Upstream response body timed out.").ConfigureAwait(false);
                    }
                    else
                    {
                        Audit(method, role, "upstream_body_idle_timeout", StatusCodes.Status504GatewayTimeout);
                        context.Abort();
                    }

                    return;
                }
                catch (HttpRequestException) when (!context.Response.HasStarted)
                {
                    await RejectAsync(context, method, role, StatusCodes.Status502BadGateway, "upstream_failed", "Upstream request failed.").ConfigureAwait(false);
                    return;
                }
                catch (HttpRequestException)
                {
                    Audit(method, role, "stream_failed", StatusCodes.Status502BadGateway);
                    context.Abort();
                    return;
                }
                catch (IOException) when (!context.Response.HasStarted)
                {
                    await RejectAsync(context, method, role, StatusCodes.Status502BadGateway, "upstream_failed", "Upstream request failed.").ConfigureAwait(false);
                    return;
                }
                catch (IOException)
                {
                    Audit(method, role, "stream_failed", StatusCodes.Status502BadGateway);
                    context.Abort();
                    return;
                }
            }

            Audit(method, role, "proxied", upstreamStatus);
        }
    }

    private static async Task CopyResponseBodyWithIdleTimeoutAsync(
        Stream source,
        Stream destination,
        TimeSpan idleTimeout,
        CancellationToken clientCancellation)
    {
        var buffer = ArrayPool<byte>.Shared.Rent(64 * 1024);
        using var idleCancellation = CancellationTokenSource.CreateLinkedTokenSource(clientCancellation);
        using var idleTimer = new Timer(
            static state =>
            {
                try
                {
                    ((CancellationTokenSource)state!).Cancel();
                }
                catch (ObjectDisposedException)
                {
                    // A callback racing disposal no longer needs to cancel anything.
                }
            },
            idleCancellation,
            Timeout.InfiniteTimeSpan,
            Timeout.InfiniteTimeSpan);

        try
        {
            while (true)
            {
                idleTimer.Change(idleTimeout, Timeout.InfiniteTimeSpan);
                var bytesRead = await source.ReadAsync(buffer, idleCancellation.Token).ConfigureAwait(false);
                if (bytesRead == 0)
                {
                    return;
                }

                idleTimer.Change(idleTimeout, Timeout.InfiniteTimeSpan);
                await destination.WriteAsync(buffer.AsMemory(0, bytesRead), idleCancellation.Token).ConfigureAwait(false);
            }
        }
        catch (OperationCanceledException) when (!clientCancellation.IsCancellationRequested)
        {
            throw new TimeoutException("The upstream response body exceeded the idle timeout.");
        }
        finally
        {
            ArrayPool<byte>.Shared.Return(buffer);
        }
    }

    private static bool HasExactLoopbackHost(HttpContext context)
    {
        var host = context.Request.Host;
        return host.HasValue &&
               host.Port == context.Connection.LocalPort &&
               string.Equals(host.Host, IPAddress.Loopback.ToString(), StringComparison.Ordinal);
    }

    private static bool TryParsePath(PathString path, out string token, out string role)
    {
        token = string.Empty;
        role = string.Empty;

        var value = path.Value;
        if (string.IsNullOrEmpty(value) ||
            value.Length < 3 ||
            value[0] != '/' ||
            value[^1] == '/')
        {
            return false;
        }

        var segments = value[1..].Split('/');
        if (segments.Length != 2 ||
            string.IsNullOrEmpty(segments[0]) ||
            string.IsNullOrEmpty(segments[1]))
        {
            return false;
        }

        token = segments[0];
        role = segments[1];
        return true;
    }

    private static bool TryCopyConditionalHeader(
        HttpContext context,
        string headerName,
        HttpRequestMessage upstreamRequest)
    {
        if (!context.Request.Headers.TryGetValue(headerName, out StringValues values))
        {
            return true;
        }

        if (values.Count != 1)
        {
            return false;
        }

        var value = values[0];
        if (string.IsNullOrWhiteSpace(value) ||
            value.Length > MaximumForwardedConditionalHeaderLength ||
            value.Any(static character =>
                character is '\r' or '\n' or '\0' ||
                character > '\x7E' ||
                char.IsControl(character)))
        {
            return false;
        }

        return upstreamRequest.Headers.TryAddWithoutValidation(headerName, value);
    }

    private static void CopyAllowedResponseHeaders(
        HttpResponseMessage upstreamResponse,
        HttpResponse relayResponse)
    {
        foreach (var header in upstreamResponse.Headers)
        {
            if (AllowedResponseHeaders.Contains(header.Key))
            {
                relayResponse.Headers[header.Key] = new StringValues(header.Value.ToArray());
            }
        }

        foreach (var header in upstreamResponse.Content.Headers)
        {
            if (AllowedResponseHeaders.Contains(header.Key))
            {
                relayResponse.Headers[header.Key] = new StringValues(header.Value.ToArray());
            }
        }

        relayResponse.ContentLength = upstreamResponse.Content.Headers.ContentLength;
    }

    private async Task RejectAsync(
        HttpContext context,
        string method,
        string role,
        int statusCode,
        string outcome,
        string safeMessage)
    {
        Audit(method, role, outcome, statusCode);

        if (context.Response.HasStarted)
        {
            return;
        }

        context.Response.Clear();
        context.Response.StatusCode = statusCode;
        context.Response.ContentType = "text/plain; charset=utf-8";
        context.Response.Headers.CacheControl = "no-store";
        context.Response.Headers["X-Content-Type-Options"] = "nosniff";

        if (statusCode == StatusCodes.Status405MethodNotAllowed)
        {
            context.Response.Headers.Allow = "GET, HEAD";
        }

        await context.Response.WriteAsync(safeMessage, context.RequestAborted).ConfigureAwait(false);
    }

    private void Audit(string method, string role, string outcome, int statusCode)
    {
        try
        {
            _options.AuditSink.Record(new RelayAuditEvent(method, role, outcome, statusCode));
        }
        catch
        {
            // Audit failures must not alter media response behavior.
        }
    }

    private static string AuditMethod(string method)
    {
        return method switch
        {
            "GET" => "GET",
            "HEAD" => "HEAD",
            _ => "OTHER",
        };
    }
}
