namespace DoriyaMedia.WinUISpike.LoopbackRelayProbe;

internal static class RelayProbeCommand
{
    public static async Task<int> RunAsync(string[] args)
    {
        if (args.Length == 0 || args.Contains("--help", StringComparer.Ordinal))
        {
            PrintUsage();
            return args.Length == 0 ? 2 : 0;
        }

        try
        {
            var parsed = ParseArguments(args);
            var video = new RelayRoute(parsed.VideoUpstream, parsed.VideoHeaders);
            var audio = new RelayRoute(parsed.AudioUpstream, parsed.AudioHeaders);
            var origins = new[] { GetOrigin(video.UpstreamUri), GetOrigin(audio.UpstreamUri) }
                .Distinct()
                .ToArray();
            var targetPolicy = parsed.AllowLoopbackUpstream
                ? RelayTargetPolicy.CreateLoopbackFixture(origins)
                : RelayTargetPolicy.CreateExactOrigins(origins);
            var options = new RelayOptions(
                RelayCapabilityToken.Generate(),
                video,
                audio,
                targetPolicy,
                TimeSpan.FromSeconds(parsed.TimeoutSeconds));

            await using var relay = await RelayServer.StartAsync(options).ConfigureAwait(false);
            var shutdown = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);

            ConsoleCancelEventHandler cancelHandler = (_, eventArgs) =>
            {
                eventArgs.Cancel = true;
                shutdown.TrySetResult();
            };

            Console.CancelKeyPress += cancelHandler;
            try
            {
                Console.WriteLine("MECHANISM PROBE ONLY - not a product local-relay.");
                Console.WriteLine($"Listening: {relay.BaseAddress}");
                Console.WriteLine($"Video client path: {relay.GetClientUri(RelayRoles.Video).PathAndQuery}");
                Console.WriteLine($"Audio client path: {relay.GetClientUri(RelayRoles.Audio).PathAndQuery}");
                Console.WriteLine("Press Ctrl+C to stop.");
                await shutdown.Task.ConfigureAwait(false);
            }
            finally
            {
                Console.CancelKeyPress -= cancelHandler;
            }

            return 0;
        }
        catch (ArgumentException)
        {
            Console.Error.WriteLine("Invalid or unsafe relay configuration. Use --help for the supported arguments.");
            return 2;
        }
        catch (Exception exception)
        {
            Console.Error.WriteLine($"Relay probe failed ({exception.GetType().Name}).");
            return 1;
        }
    }

    private static ParsedArguments ParseArguments(string[] args)
    {
        Uri? videoUpstream = null;
        Uri? audioUpstream = null;
        var videoHeaders = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
        var audioHeaders = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
        var allowLoopbackUpstream = false;
        var timeoutSeconds = (int)RelayOptions.DefaultUpstreamTimeout.TotalSeconds;

        for (var index = 0; index < args.Length; index++)
        {
            switch (args[index])
            {
                case "--video-upstream":
                    videoUpstream = ParseAbsoluteUri(ReadValue(args, ref index));
                    break;
                case "--audio-upstream":
                    audioUpstream = ParseAbsoluteUri(ReadValue(args, ref index));
                    break;
                case "--video-header":
                    AddHeader(videoHeaders, ReadValue(args, ref index));
                    break;
                case "--audio-header":
                    AddHeader(audioHeaders, ReadValue(args, ref index));
                    break;
                case "--allow-loopback-upstream":
                    allowLoopbackUpstream = true;
                    break;
                case "--timeout-seconds":
                    if (!int.TryParse(ReadValue(args, ref index), out timeoutSeconds) ||
                        timeoutSeconds is < 1 or > 300)
                    {
                        throw new ArgumentException("The timeout is invalid.", nameof(args));
                    }

                    break;
                default:
                    throw new ArgumentException("An argument is not recognized.", nameof(args));
            }
        }

        if (videoUpstream is null || audioUpstream is null)
        {
            throw new ArgumentException("Both upstream arguments are required.", nameof(args));
        }

        return new ParsedArguments(
            videoUpstream,
            audioUpstream,
            videoHeaders,
            audioHeaders,
            allowLoopbackUpstream,
            timeoutSeconds);
    }

    private static string ReadValue(string[] args, ref int index)
    {
        index++;
        if (index >= args.Length)
        {
            throw new ArgumentException("An argument value is missing.", nameof(args));
        }

        return args[index];
    }

    private static Uri ParseAbsoluteUri(string value)
    {
        if (!Uri.TryCreate(value, UriKind.Absolute, out var uri))
        {
            throw new ArgumentException("An upstream URI is invalid.", nameof(value));
        }

        return uri;
    }

    private static void AddHeader(Dictionary<string, string> headers, string argument)
    {
        var separator = argument.IndexOf('=');
        if (separator <= 0)
        {
            throw new ArgumentException("A header argument is invalid.", nameof(argument));
        }

        var name = argument[..separator];
        var value = argument[(separator + 1)..];
        if (!headers.TryAdd(name, value))
        {
            throw new ArgumentException("A header argument is duplicated.", nameof(argument));
        }
    }

    private static Uri GetOrigin(Uri uri)
    {
        return new Uri($"{uri.GetLeftPart(UriPartial.Authority)}/", UriKind.Absolute);
    }

    private static void PrintUsage()
    {
        Console.WriteLine("MECHANISM PROBE ONLY - not a product local-relay.");
        Console.WriteLine("Usage:");
        Console.WriteLine("  LoopbackRelayProbe --video-upstream <absolute-uri> --audio-upstream <absolute-uri>");
        Console.WriteLine("      [--video-header Name=Value]... [--audio-header Name=Value]...");
        Console.WriteLine("      [--allow-loopback-upstream] [--timeout-seconds 1..300]");
        Console.WriteLine();
        Console.WriteLine("Injected headers are limited to Origin, Referer, and User-Agent.");
        Console.WriteLine("Loopback upstream fixtures require --allow-loopback-upstream.");
    }

    private sealed record ParsedArguments(
        Uri VideoUpstream,
        Uri AudioUpstream,
        IReadOnlyDictionary<string, string> VideoHeaders,
        IReadOnlyDictionary<string, string> AudioHeaders,
        bool AllowLoopbackUpstream,
        int TimeoutSeconds);
}
