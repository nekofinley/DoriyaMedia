using System.Collections.Concurrent;
using System.Diagnostics;
using System.Text.Json;
using System.Text.Json.Serialization;
using DoriyaMedia.WinUISpike.Core;
using LibVLCSharp.Shared;
using Xunit;
using Xunit.Abstractions;

namespace DoriyaMedia.WinUISpike.PlaybackProbe.Tests;

[CollectionDefinition(Name, DisableParallelization = true)]
public sealed class LibVlcPlaybackCollection
{
    public const string Name = "LibVLC playback";
}

[Collection(LibVlcPlaybackCollection.Name)]
public sealed class SyntheticDualSourcePlaybackTests
{
    private static readonly TimeSpan FixtureStartTimeout = TimeSpan.FromSeconds(10);
    private static readonly TimeSpan PlaybackStartTimeout = TimeSpan.FromSeconds(20);
    private static readonly TimeSpan SeekTimeout = TimeSpan.FromSeconds(5);
    private static readonly TimeSpan ResumeSettleDelay = TimeSpan.FromMilliseconds(100);
    private static readonly TimeSpan TrackStabilityTimeout = TimeSpan.FromSeconds(5);
    private static readonly TimeSpan StopTimeout = TimeSpan.FromSeconds(10);
    private static readonly TimeSpan FixtureStopTimeout = TimeSpan.FromSeconds(5);
    private const int FixtureLifetimeSeconds = 90;
    private static readonly long[] SeekTargets =
    [
        2600,
        500,
        2400,
        800,
        2200,
        1100,
        2700,
        300,
        2000,
        600,
        2300,
        900,
        2500,
        400,
        2100,
        1000,
        2800,
        700,
        1900,
        1200
    ];

    private readonly ITestOutputHelper _output;

    public SyntheticDualSourcePlaybackTests(ITestOutputHelper output)
    {
        _output = output;
    }

    [Fact]
    public async Task SyntheticVideoAndAudioSlavePlayAndSeekDeterministicallyAsync()
    {
        Assert.True(
            OperatingSystem.IsWindows(),
            "The LibVLCSharp WinUI playback spike is Windows-only.");

        var paths = SpikePaths.Discover();
        using var videoFixture = await LoopbackFixtureServer.StartAsync(
            paths.FixtureServer,
            paths.VideoFixture);
        using var audioFixture = await LoopbackFixtureServer.StartAsync(
            paths.FixtureServer,
            paths.AudioFixture);

        Assert.False(videoFixture.HasExited, "The video fixture server exited.");
        Assert.False(audioFixture.HasExited, "The audio fixture server exited.");
        Assert.NotEqual(
            new Uri(videoFixture.Url).Authority,
            new Uri(audioFixture.Url).Authority);
        Assert.Equal(videoFixture.Referer, audioFixture.Referer, StringComparer.Ordinal);
        Assert.Equal(videoFixture.UserAgent, audioFixture.UserAgent, StringComparer.Ordinal);

        var headers =
            $"Referer: {videoFixture.Referer}\n" +
            $"User-Agent: {videoFixture.UserAgent}";
        var planResult = MediaLoadPlanBuilder.Build(
            videoFixture.Url,
            audioFixture.Url,
            headers,
            allowLoopbackHttp: true);
        Assert.True(planResult.IsValid, "The synthetic dual-source plan was rejected.");
        Assert.Equal(MediaSourceMode.VideoWithAudioSlave, planResult.Plan!.Mode);

        var installation = LocateVlc();
        Assert.True(installation.IsUsable, installation.SafeMessage);

        var diagnostics = new RedactingDiagnosticWriter(new TestDiagnosticSink(_output));
        using var backend = new LibVlcPlaybackBackend(
            installation.RootDirectory!,
            [
                "--vout=dummy",
                "--aout=dummy",
                "--avcodec-hw=none",
                "--no-embedded-video"
            ],
            diagnostics);
        Assert.StartsWith("3.0.19", backend.LibVlcVersion, StringComparison.Ordinal);

        using var session = new PlaybackSession(backend, diagnostics);
        var statuses = new ConcurrentQueue<PlaybackStatus>();
        session.StatusChanged += (_, args) => statuses.Enqueue(args.Status);
        session.Open(
            videoFixture.Url,
            audioFixture.Url,
            headers,
            allowLoopbackHttp: true);

        WaitForPlaybackState(session, PlaybackState.Playing, PlaybackStartTimeout);
        var observedStatuses = statuses.ToArray();
        var openingIndex = Array.FindIndex(
            observedStatuses,
            status => status.State == PlaybackState.Opening);
        var playingIndex = Array.FindIndex(
            observedStatuses,
            status => status.State == PlaybackState.Playing);
        Assert.True(openingIndex >= 0, "The production session never reported Opening.");
        Assert.True(
            playingIndex > openingIndex,
            "The production session reported Playing before Opening.");
        Assert.DoesNotContain(
            observedStatuses,
            status => status.State == PlaybackState.Failed);

        var player = backend.MediaPlayer;
        Assert.Equal(VLCState.Playing, player.State);
        Assert.True(player.IsSeekable, "The synthetic dual-source media is not seekable.");
        var initialTracks = WaitForStableDualTracks(player);

        var initialPosition = WaitForTimeAtLeast(player, 250, PlaybackStartTimeout);
        player.SetPause(true);
        WaitUntil(
            () => player.State == VLCState.Paused,
            SeekTimeout,
            () => $"state={player.State}, time={player.Time}");
        WaitForStableDualTracks(player);

        var observedPositions = new HashSet<long>();
        var previousPosition = player.Time;
        foreach (var targetMilliseconds in SeekTargets)
        {
            Assert.True(
                Math.Abs(targetMilliseconds - previousPosition) >= 700,
                $"Seek target {targetMilliseconds} ms is too close to " +
                $"the current position {previousPosition} ms.");
            player.Time = targetMilliseconds;
            var actualMilliseconds = WaitForSeek(player, targetMilliseconds);
            Assert.True(
                Math.Abs(actualMilliseconds - previousPosition) > 200,
                $"Seek {targetMilliseconds} did not move far enough from " +
                $"{previousPosition}; settled at {actualMilliseconds}.");
            Assert.True(
                observedPositions.Add(actualMilliseconds),
                $"Seek target {targetMilliseconds} collapsed onto {actualMilliseconds}.");
            Assert.Equal(VLCState.Paused, player.State);
            WaitForStableDualTracks(player);
            previousPosition = actualMilliseconds;
        }

        Assert.Equal(SeekTargets.Length, observedPositions.Count);

        long resumeStart;
        long resumeEnd;
        DualTrackSnapshot finalTracks;
        player.SetPause(false);
        // LibVLC 3.0.19 can unload its D3D11 output while the resume transition
        // is still completing. Let that native transition settle before polling.
        Thread.Sleep(ResumeSettleDelay);
        WaitUntil(
            () => player.State == VLCState.Playing,
            SeekTimeout,
            () => $"state={player.State}, time={player.Time}");
        resumeStart = player.Time;
        resumeEnd = WaitForTimeAtLeast(
            player,
            resumeStart + 250,
            PlaybackStartTimeout);
        finalTracks = WaitForStableDualTracks(player);

        Assert.False(videoFixture.HasExited, "The video fixture server exited during playback.");
        Assert.False(audioFixture.HasExited, "The audio fixture server exited during playback.");

        var stopMilliseconds = StopWithinTimeout(session, player);

        _output.WriteLine(
            $"PLAYBACK_ENV|vlcRoot={installation.RootDirectory}" +
            $"|vlcVersion={backend.LibVlcVersion}" +
            $"|python={GetPythonVersion()}");
        _output.WriteLine(
            "PLAYBACK_PROBE|status=PASS" +
            $"|initialVideoTracks={initialTracks.VideoTrackIds.Length}" +
            $"|initialAudioTracks={initialTracks.AudioTrackIds.Length}" +
            $"|playableVideoTrackIds={string.Join(',', finalTracks.VideoTrackIds)}" +
            $"|playableAudioTrackIds={string.Join(',', finalTracks.AudioTrackIds)}" +
            $"|selectedVideoTrack={finalTracks.SelectedVideoTrack}" +
            $"|selectedAudioTrack={finalTracks.SelectedAudioTrack}" +
            $"|initialPositionMs={initialPosition}" +
            $"|seekRequests={SeekTargets.Length}" +
            $"|verifiedSeeks={observedPositions.Count}" +
            $"|distinctPositions={observedPositions.Count}" +
            $"|resumeAdvanceMs={resumeEnd - resumeStart}" +
            $"|stopMs={stopMilliseconds}");
    }

    private static PlaybackStatus WaitForPlaybackState(
        PlaybackSession session,
        PlaybackState expected,
        TimeSpan timeout)
    {
        var stopwatch = Stopwatch.StartNew();
        while (stopwatch.Elapsed < timeout)
        {
            var status = session.LastStatus;
            if (status.State == PlaybackState.Failed && expected != PlaybackState.Failed)
            {
                throw new Xunit.Sdk.XunitException(
                    $"Playback failed while waiting for {expected}: " +
                    $"{status.ErrorCode} / {status.SafeMessage}");
            }
            if (status.State == expected)
            {
                return status;
            }

            Thread.Sleep(20);
        }

        var last = session.LastStatus;
        throw new Xunit.Sdk.XunitException(
            $"Timed out after {timeout.TotalSeconds:0.#} seconds waiting for " +
            $"{expected}; last state={last.State}, error={last.ErrorCode}, " +
            $"message={last.SafeMessage}");
    }

    private static DualTrackSnapshot WaitForStableDualTracks(MediaPlayer player)
    {
        DualTrackSnapshot? previous = null;
        var stableSamples = 0;
        var stopwatch = Stopwatch.StartNew();
        while (stopwatch.Elapsed < TrackStabilityTimeout)
        {
            var current = CaptureDualTracks(player);
            if (current.HasExactlyOneVideoAndAudio &&
                previous is not null &&
                current.SameAs(previous.Value))
            {
                stableSamples++;
                if (stableSamples >= 2)
                {
                    return current;
                }
            }
            else
            {
                stableSamples = 0;
            }

            previous = current;
            Thread.Sleep(50);
        }

        throw new Xunit.Sdk.XunitException(
            "Video/audio track state did not stabilize with exactly one playable " +
            $"track of each type; video={string.Join(',', previous?.VideoTrackIds ?? [])}, " +
            $"audio={string.Join(',', previous?.AudioTrackIds ?? [])}, " +
            $"selectedVideo={previous?.SelectedVideoTrack}, " +
            $"selectedAudio={previous?.SelectedAudioTrack}");
    }

    private static DualTrackSnapshot CaptureDualTracks(MediaPlayer player)
    {
        var videoTrackIds = player.VideoTrackDescription
            .Where(track => track.Id >= 0)
            .Select(track => track.Id)
            .Order()
            .ToArray();
        var audioTrackIds = player.AudioTrackDescription
            .Where(track => track.Id >= 0)
            .Select(track => track.Id)
            .Order()
            .ToArray();
        return new DualTrackSnapshot(
            videoTrackIds,
            audioTrackIds,
            player.VideoTrack,
            player.AudioTrack);
    }

    private static long WaitForSeek(MediaPlayer player, long targetMilliseconds) =>
        WaitUntilValue(
            () => player.Time,
            time =>
                time >= 0 &&
                Math.Abs(time - targetMilliseconds) <= 300,
            SeekTimeout,
            () =>
                $"seek did not settle near {targetMilliseconds} ms; " +
                $"state={player.State}, time={player.Time}");

    private static long WaitForTimeAtLeast(
        MediaPlayer player,
        long targetMilliseconds,
        TimeSpan timeout) =>
        WaitUntilValue(
            () => player.Time,
            time => time >= targetMilliseconds,
            timeout,
            () => $"time did not advance to {targetMilliseconds} ms; time={player.Time}");

    private static long StopWithinTimeout(
        PlaybackSession session,
        MediaPlayer player)
    {
        var stopwatch = Stopwatch.StartNew();
        session.Stop();
        stopwatch.Stop();
        Assert.True(
            stopwatch.Elapsed < StopTimeout,
            $"LibVLC stop exceeded {StopTimeout.TotalSeconds:0} seconds.");
        WaitUntil(
            () => player.State is VLCState.Stopped or VLCState.NothingSpecial,
            StopTimeout,
            () => $"state={player.State}");
        return stopwatch.ElapsedMilliseconds;
    }

    private static void WaitUntil(
        Func<bool> condition,
        TimeSpan timeout,
        Func<string> failureDetail)
    {
        var stopwatch = Stopwatch.StartNew();
        while (stopwatch.Elapsed < timeout)
        {
            if (condition())
            {
                return;
            }

            Thread.Sleep(20);
        }

        throw new Xunit.Sdk.XunitException(
            $"Timed out after {timeout.TotalSeconds:0.#} seconds: {failureDetail()}");
    }

    private static long WaitUntilValue(
        Func<long> value,
        Func<long, bool> condition,
        TimeSpan timeout,
        Func<string> failureDetail)
    {
        var stopwatch = Stopwatch.StartNew();
        while (stopwatch.Elapsed < timeout)
        {
            var current = value();
            if (condition(current))
            {
                return current;
            }

            Thread.Sleep(20);
        }

        throw new Xunit.Sdk.XunitException(
            $"Timed out after {timeout.TotalSeconds:0.#} seconds: {failureDetail()}");
    }

    private static LibVlcInstallationProbeResult LocateVlc()
    {
        var programFiles = Environment.GetFolderPath(Environment.SpecialFolder.ProgramFiles);
        var programFilesX86 = Environment.GetFolderPath(Environment.SpecialFolder.ProgramFilesX86);
        return LibVlcLocator.Locate(
            explicitPath: null,
            environmentPath: Environment.GetEnvironmentVariable("DORIYA_VLC_HOME"),
            defaultDirectories:
            [
                Path.Combine(programFiles, "VideoLAN", "VLC"),
                Path.Combine(programFilesX86, "VideoLAN", "VLC")
            ],
            new PhysicalFileSystemProbe());
    }

    private static string GetPythonVersion()
    {
        try
        {
            var startInfo = new ProcessStartInfo
            {
                FileName = "python",
                UseShellExecute = false,
                CreateNoWindow = true,
                RedirectStandardOutput = true,
                RedirectStandardError = true
            };
            startInfo.ArgumentList.Add("--version");
            using var process = Process.Start(startInfo);
            if (process is null)
            {
                return "unavailable";
            }

            var output = process.StandardOutput.ReadToEnd().Trim();
            var error = process.StandardError.ReadToEnd().Trim();
            if (!process.WaitForExit(5000))
            {
                return "timeout";
            }

            return string.IsNullOrWhiteSpace(output) ? error : output;
        }
        catch (Exception exception) when (
            exception is InvalidOperationException or
            System.ComponentModel.Win32Exception)
        {
            return $"unavailable:{exception.GetType().Name}";
        }
    }

    private readonly record struct DualTrackSnapshot(
        int[] VideoTrackIds,
        int[] AudioTrackIds,
        int SelectedVideoTrack,
        int SelectedAudioTrack)
    {
        public bool HasExactlyOneVideoAndAudio =>
            VideoTrackIds.Length == 1 &&
            AudioTrackIds.Length == 1 &&
            VideoTrackIds[0] == SelectedVideoTrack &&
            AudioTrackIds[0] == SelectedAudioTrack;

        public bool SameAs(DualTrackSnapshot other) =>
            VideoTrackIds.SequenceEqual(other.VideoTrackIds) &&
            AudioTrackIds.SequenceEqual(other.AudioTrackIds) &&
            SelectedVideoTrack == other.SelectedVideoTrack &&
            SelectedAudioTrack == other.SelectedAudioTrack;
    }

    private sealed class TestDiagnosticSink : IDiagnosticSink
    {
        private readonly ITestOutputHelper _output;
        private readonly object _gate = new();

        public TestDiagnosticSink(ITestOutputHelper output)
        {
            _output = output;
        }

        public void WriteLine(string line)
        {
            lock (_gate)
            {
                _output.WriteLine($"DIAG|{line}");
            }
        }
    }

    private sealed class SpikePaths
    {
        private SpikePaths(string fixtureServer, string videoFixture, string audioFixture)
        {
            FixtureServer = fixtureServer;
            VideoFixture = videoFixture;
            AudioFixture = audioFixture;
        }

        public string FixtureServer { get; }

        public string VideoFixture { get; }

        public string AudioFixture { get; }

        public static SpikePaths Discover()
        {
            foreach (var startDirectory in new[]
                     {
                         AppContext.BaseDirectory,
                         Directory.GetCurrentDirectory()
                     })
            {
                for (var directory = new DirectoryInfo(startDirectory);
                     directory is not null;
                     directory = directory.Parent)
                {
                    if (!File.Exists(Path.Combine(
                        directory.FullName,
                        "DoriyaMedia.WinUISpike.sln")))
                    {
                        continue;
                    }

                    var fixtureDirectory = Path.GetFullPath(
                        Path.Combine(directory.FullName, "..", "fixtures"));
                    var paths = new SpikePaths(
                        Path.Combine(fixtureDirectory, "header-range-server.py"),
                        Path.Combine(fixtureDirectory, "synthetic", "s1-video-only.mp4"),
                        Path.Combine(fixtureDirectory, "synthetic", "s1-audio-only.mp4"));
                    paths.Validate();
                    return paths;
                }
            }

            throw new InvalidOperationException(
                "Could not locate the WinUI spike fixture directory.");
        }

        private void Validate()
        {
            foreach (var path in new[] { FixtureServer, VideoFixture, AudioFixture })
            {
                if (!File.Exists(path))
                {
                    throw new FileNotFoundException(
                        "A required playback fixture is missing.",
                        path);
                }
            }
        }
    }

    private sealed class LoopbackFixtureServer : IDisposable
    {
        private readonly Process _process;
        private bool _disposed;

        private LoopbackFixtureServer(Process process, FixtureInfo info)
        {
            _process = process;
            Url = info.Url;
            Referer = info.Referer;
            UserAgent = info.UserAgent;
        }

        public string Url { get; }

        public string Referer { get; }

        public string UserAgent { get; }

        public bool HasExited => _process.HasExited;

        public static async Task<LoopbackFixtureServer> StartAsync(
            string fixtureServer,
            string mediaFile)
        {
            var startInfo = new ProcessStartInfo
            {
                FileName = "python",
                UseShellExecute = false,
                CreateNoWindow = true,
                RedirectStandardOutput = true
            };
            startInfo.ArgumentList.Add("-u");
            startInfo.ArgumentList.Add(fixtureServer);
            startInfo.ArgumentList.Add("--file");
            startInfo.ArgumentList.Add(mediaFile);
            startInfo.ArgumentList.Add("--max-seconds");
            startInfo.ArgumentList.Add(FixtureLifetimeSeconds.ToString());

            var process = new Process { StartInfo = startInfo };
            try
            {
                if (!process.Start())
                {
                    throw new InvalidOperationException(
                        "The fixture server process did not start.");
                }

                var line = await process.StandardOutput
                    .ReadLineAsync()
                    .WaitAsync(FixtureStartTimeout);
                if (string.IsNullOrWhiteSpace(line))
                {
                    throw new InvalidOperationException(
                        "The fixture server did not publish its metadata.");
                }

                var info = JsonSerializer.Deserialize<FixtureInfo>(line);
                if (info is null ||
                    !Uri.TryCreate(info.Url, UriKind.Absolute, out var uri) ||
                    uri.Scheme != Uri.UriSchemeHttp ||
                    uri.Host != "127.0.0.1")
                {
                    throw new InvalidOperationException(
                        "The fixture server published invalid metadata.");
                }

                return new LoopbackFixtureServer(process, info);
            }
            catch
            {
                KillAndWait(process);
                process.Dispose();
                throw;
            }
        }

        public void Dispose()
        {
            if (_disposed)
            {
                return;
            }

            _disposed = true;
            KillAndWait(_process);
            _process.Dispose();
        }

        private static void KillAndWait(Process process)
        {
            try
            {
                if (!process.HasExited)
                {
                    process.Kill(entireProcessTree: true);
                }
            }
            catch (InvalidOperationException)
            {
            }
            catch (System.ComponentModel.Win32Exception)
            {
            }

            if (!process.WaitForExit((int)FixtureStopTimeout.TotalMilliseconds))
            {
                throw new TimeoutException(
                    $"A fixture server exceeded " +
                    $"{FixtureStopTimeout.TotalSeconds:0} seconds to stop.");
            }
        }

        private sealed class FixtureInfo
        {
            [JsonPropertyName("url")]
            public string Url { get; init; } = string.Empty;

            [JsonPropertyName("referer")]
            public string Referer { get; init; } = string.Empty;

            [JsonPropertyName("user_agent")]
            public string UserAgent { get; init; } = string.Empty;
        }
    }
}
