using System.Diagnostics;
using LibVLCSharp.Shared;

namespace DoriyaMedia.WinUISpike.Core;

public sealed class LibVlcPlaybackBackend : IMediaPlaybackBackend
{
    private static readonly TimeSpan DualSourceStartTimeout = TimeSpan.FromSeconds(10);
    private static readonly TimeSpan TrackPollInterval = TimeSpan.FromMilliseconds(50);
    private readonly RedactingDiagnosticWriter _diagnostics;
    private readonly LibVLC _libVlc;
    private Media? _media;
    private int _loadGeneration;
    private int _requiresAudioSlave;
    private int _terminalStatePublished;
    private int _mergeMonitorRunning;
    private int _playbackActive;
    private int _disposed;

    public LibVlcPlaybackBackend(
        string vlcDirectory,
        IEnumerable<string> swapChainOptions,
        RedactingDiagnosticWriter diagnostics)
    {
        ArgumentException.ThrowIfNullOrWhiteSpace(vlcDirectory);
        ArgumentNullException.ThrowIfNull(swapChainOptions);
        _diagnostics = diagnostics ?? throw new ArgumentNullException(nameof(diagnostics));

        LibVLCSharp.Shared.Core.Initialize(vlcDirectory);

        var options = swapChainOptions
            .Append("--quiet")
            .Append("--no-video-title-show")
            .Distinct(StringComparer.Ordinal)
            .ToArray();

        _libVlc = new LibVLC(enableDebugLogs: false, options);
        MediaPlayer = new MediaPlayer(_libVlc);
        MediaPlayer.Opening += OnOpening;
        MediaPlayer.Playing += OnPlaying;
        MediaPlayer.EndReached += OnEndReached;
        MediaPlayer.EncounteredError += OnEncounteredError;
    }

    public event EventHandler<BackendStateChangedEventArgs>? StateChanged;

    public MediaPlayer MediaPlayer { get; }

    public string LibVlcVersion => _libVlc.Version;

    public BackendOperationResult Open(MediaLoadPlan plan)
    {
        ArgumentNullException.ThrowIfNull(plan);
        ObjectDisposedException.ThrowIf(Volatile.Read(ref _disposed) != 0, this);

        Media? nextMedia = null;
        try
        {
            Interlocked.Increment(ref _loadGeneration);
            Interlocked.Exchange(ref _mergeMonitorRunning, 0);
            nextMedia = new Media(_libVlc, new Uri(plan.VideoUri, UriKind.Absolute));
            foreach (var option in plan.LibVlcMediaOptions)
            {
                nextMedia.AddOption(option);
            }

            if (plan.AudioSlaveUri is not null &&
                !nextMedia.AddSlave(
                    MediaSlaveType.Audio,
                    4,
                    new Uri(plan.AudioSlaveUri, UriKind.Absolute)))
            {
                nextMedia.Dispose();
                return BackendOperationResult.Failure(
                    "LibVLC could not add the audio source.",
                    PlaybackErrorCode.MediaMergeUnavailable);
            }

            Interlocked.Exchange(ref _requiresAudioSlave, plan.AudioSlaveUri is null ? 0 : 1);
            Interlocked.Exchange(ref _terminalStatePublished, 0);

            var previousMedia = Interlocked.Exchange(ref _media, nextMedia);
            previousMedia?.Dispose();

            if (MediaPlayer.Play(nextMedia))
            {
                Interlocked.Exchange(ref _playbackActive, 1);
                return BackendOperationResult.Success("Media open accepted.");
            }

            Interlocked.Exchange(ref _requiresAudioSlave, 0);
            Interlocked.Exchange(ref _playbackActive, 0);
            return BackendOperationResult.Failure(
                "LibVLC rejected the media.",
                PlaybackErrorCode.LibVlcRejectedMedia);
        }
        catch (Exception exception)
        {
            _diagnostics.WriteExceptionType("LibVLC media open failed", exception);
            Interlocked.Exchange(ref _requiresAudioSlave, 0);
            Interlocked.Exchange(ref _playbackActive, 0);
            if (ReferenceEquals(Interlocked.CompareExchange(ref _media, null, nextMedia), nextMedia))
            {
                nextMedia?.Dispose();
            }

            return BackendOperationResult.Failure(
                "The media could not be opened.",
                PlaybackErrorCode.PlaybackFailed);
        }
    }

    public BackendOperationResult Stop()
    {
        ObjectDisposedException.ThrowIf(Volatile.Read(ref _disposed) != 0, this);

        if (Interlocked.Exchange(ref _playbackActive, 0) == 0)
        {
            Interlocked.Exchange(ref _requiresAudioSlave, 0);
            return BackendOperationResult.Success("Playback is already stopped.");
        }

        try
        {
            Interlocked.Increment(ref _loadGeneration);
            Interlocked.Exchange(ref _terminalStatePublished, 0);
            Interlocked.Exchange(ref _mergeMonitorRunning, 0);
            MediaPlayer.Stop();
            Interlocked.Exchange(ref _requiresAudioSlave, 0);
            Interlocked.Exchange(ref _terminalStatePublished, 0);
            return BackendOperationResult.Success("Playback stopped.");
        }
        catch (Exception exception)
        {
            _diagnostics.WriteExceptionType("LibVLC stop failed", exception);
            return BackendOperationResult.Failure(
                "Playback could not be stopped.",
                PlaybackErrorCode.PlaybackFailed);
        }
    }

    private void OnOpening(object? sender, EventArgs args) =>
        Publish(BackendPlaybackState.Opening);

    private void OnPlaying(object? sender, EventArgs args)
    {
        if (Volatile.Read(ref _requiresAudioSlave) == 0)
        {
            Publish(BackendPlaybackState.Playing);
            return;
        }

        if (Interlocked.CompareExchange(ref _mergeMonitorRunning, 1, 0) == 0)
        {
            var generation = Volatile.Read(ref _loadGeneration);
            _ = Task.Run(() => MonitorDualSourceStart(generation));
        }
    }

    private void MonitorDualSourceStart(int generation)
    {
        try
        {
            var stopwatch = Stopwatch.StartNew();
            while (stopwatch.Elapsed < DualSourceStartTimeout)
            {
                if (Volatile.Read(ref _disposed) != 0 ||
                    generation != Volatile.Read(ref _loadGeneration) ||
                    Volatile.Read(ref _terminalStatePublished) != 0)
                {
                    return;
                }

                if (HasPlayableVideoAndAudio())
                {
                    if (generation == Volatile.Read(ref _loadGeneration) &&
                        Volatile.Read(ref _terminalStatePublished) == 0)
                    {
                        Interlocked.Exchange(ref _requiresAudioSlave, 0);
                        Publish(BackendPlaybackState.Playing);
                    }

                    return;
                }

                if (MediaPlayer.State is VLCState.Error or VLCState.Ended or VLCState.Stopped)
                {
                    return;
                }

                Thread.Sleep(TrackPollInterval);
            }

            if (generation != Volatile.Read(ref _loadGeneration) ||
                Volatile.Read(ref _terminalStatePublished) != 0)
            {
                return;
            }

            Interlocked.Exchange(ref _requiresAudioSlave, 0);
            Interlocked.Exchange(ref _terminalStatePublished, 1);
            _diagnostics.Write(
                "LibVLC dual-source playback did not expose selected video and audio tracks.");
            Publish(
                BackendPlaybackState.Failed,
                PlaybackErrorCode.MediaMergeFailed);
            ThreadPool.QueueUserWorkItem(_ => StopAfterMergeFailure());
        }
        catch (Exception exception)
        {
            _diagnostics.WriteExceptionType("LibVLC dual-source track verification failed", exception);
            if (generation == Volatile.Read(ref _loadGeneration))
            {
                Interlocked.Exchange(ref _requiresAudioSlave, 0);
                Interlocked.Exchange(ref _terminalStatePublished, 1);
                Publish(BackendPlaybackState.Failed, PlaybackErrorCode.PlaybackFailed);
            }
        }
        finally
        {
            Interlocked.Exchange(ref _mergeMonitorRunning, 0);
        }
    }

    private bool HasPlayableVideoAndAudio()
    {
        var videoTrackIds = MediaPlayer.VideoTrackDescription
            .Where(track => track.Id >= 0)
            .Select(track => track.Id)
            .ToHashSet();
        var audioTrackIds = MediaPlayer.AudioTrackDescription
            .Where(track => track.Id >= 0)
            .Select(track => track.Id)
            .ToHashSet();
        return videoTrackIds.Count != 0 &&
               audioTrackIds.Count != 0 &&
               videoTrackIds.Contains(MediaPlayer.VideoTrack) &&
               audioTrackIds.Contains(MediaPlayer.AudioTrack);
    }

    private void StopAfterMergeFailure()
    {
        if (Volatile.Read(ref _disposed) != 0 ||
            Interlocked.Exchange(ref _playbackActive, 0) == 0)
        {
            return;
        }

        try
        {
            MediaPlayer.Stop();
        }
        catch (ObjectDisposedException)
        {
        }
        catch (Exception exception)
        {
            _diagnostics.WriteExceptionType("LibVLC merge failure cleanup failed", exception);
        }
    }

    private void OnEndReached(object? sender, EventArgs args)
    {
        Interlocked.Exchange(ref _playbackActive, 0);
        Interlocked.Exchange(ref _requiresAudioSlave, 0);
        if (Interlocked.CompareExchange(ref _terminalStatePublished, 1, 0) == 0)
        {
            Publish(BackendPlaybackState.Ended);
        }
    }

    private void OnEncounteredError(object? sender, EventArgs args)
    {
        Interlocked.Exchange(ref _playbackActive, 0);
        Interlocked.Exchange(ref _requiresAudioSlave, 0);
        Interlocked.Exchange(ref _terminalStatePublished, 1);
        Publish(BackendPlaybackState.Failed, PlaybackErrorCode.PlaybackFailed);
    }

    private void Publish(
        BackendPlaybackState state,
        PlaybackErrorCode errorCode = PlaybackErrorCode.None)
    {
        if (Volatile.Read(ref _disposed) != 0)
        {
            return;
        }

        StateChanged?.Invoke(this, new BackendStateChangedEventArgs(state, errorCode));
    }

    public void Dispose()
    {
        if (Interlocked.Exchange(ref _disposed, 1) != 0)
        {
            return;
        }

        MediaPlayer.Opening -= OnOpening;
        MediaPlayer.Playing -= OnPlaying;
        MediaPlayer.EndReached -= OnEndReached;
        MediaPlayer.EncounteredError -= OnEncounteredError;

        try
        {
            if (Interlocked.Exchange(ref _playbackActive, 0) != 0)
            {
                MediaPlayer.Stop();
            }
        }
        catch (Exception exception)
        {
            _diagnostics.WriteExceptionType("LibVLC shutdown stop failed", exception);
        }

        MediaPlayer.Dispose();
        Interlocked.Exchange(ref _media, null)?.Dispose();
        _libVlc.Dispose();
    }
}
