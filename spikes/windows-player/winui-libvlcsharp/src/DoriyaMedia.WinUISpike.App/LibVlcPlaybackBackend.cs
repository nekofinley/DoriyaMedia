using DoriyaMedia.WinUISpike.Core;
using LibVLCSharp.Shared;

namespace DoriyaMedia.WinUISpike.App;

internal sealed class LibVlcPlaybackBackend : IMediaPlaybackBackend
{
    private readonly RedactingDiagnosticWriter _diagnostics;
    private readonly LibVLC _libVlc;
    private Media? _media;
    private int _requiresAudioSlave;
    private int _terminalFailurePublished;
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
        MediaPlayer.Stopped += OnStopped;
        MediaPlayer.EndReached += OnEndReached;
        MediaPlayer.EncounteredError += OnEncounteredError;
    }

    public event EventHandler<BackendStateChangedEventArgs>? StateChanged;

    public MediaPlayer MediaPlayer { get; }

    public BackendOperationResult Open(MediaLoadPlan plan)
    {
        ArgumentNullException.ThrowIfNull(plan);
        ObjectDisposedException.ThrowIf(Volatile.Read(ref _disposed) != 0, this);

        Media? nextMedia = null;
        try
        {
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
            Interlocked.Exchange(ref _terminalFailurePublished, 0);

            var previousMedia = Interlocked.Exchange(ref _media, nextMedia);
            previousMedia?.Dispose();

            if (MediaPlayer.Play(nextMedia))
            {
                return BackendOperationResult.Success("Media open accepted.");
            }

            Interlocked.Exchange(ref _requiresAudioSlave, 0);
            return BackendOperationResult.Failure(
                "LibVLC rejected the media.",
                PlaybackErrorCode.LibVlcRejectedMedia);
        }
        catch (Exception exception)
        {
            _diagnostics.WriteExceptionType("LibVLC media open failed", exception);
            Interlocked.Exchange(ref _requiresAudioSlave, 0);
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

        try
        {
            MediaPlayer.Stop();
            Interlocked.Exchange(ref _requiresAudioSlave, 0);
            Interlocked.Exchange(ref _terminalFailurePublished, 0);
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
        if (Volatile.Read(ref _requiresAudioSlave) != 0 &&
            MediaPlayer.AudioTrackCount <= 0)
        {
            Interlocked.Exchange(ref _requiresAudioSlave, 0);
            Interlocked.Exchange(ref _terminalFailurePublished, 1);
            _diagnostics.Write("LibVLC dual-source playback did not expose an audio track.");
            Publish(
                BackendPlaybackState.Failed,
                PlaybackErrorCode.MediaMergeFailed);
            ThreadPool.QueueUserWorkItem(_ => StopAfterMergeFailure());

            return;
        }

        Publish(BackendPlaybackState.Playing);
    }

    private void OnStopped(object? sender, EventArgs args)
    {
        if (Volatile.Read(ref _terminalFailurePublished) == 0)
        {
            Publish(BackendPlaybackState.Stopped);
        }
    }

    private void StopAfterMergeFailure()
    {
        if (Volatile.Read(ref _disposed) != 0)
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
        Interlocked.Exchange(ref _requiresAudioSlave, 0);
        Publish(BackendPlaybackState.Ended);
    }

    private void OnEncounteredError(object? sender, EventArgs args)
    {
        Interlocked.Exchange(ref _requiresAudioSlave, 0);
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
        MediaPlayer.Stopped -= OnStopped;
        MediaPlayer.EndReached -= OnEndReached;
        MediaPlayer.EncounteredError -= OnEncounteredError;

        try
        {
            MediaPlayer.Stop();
        }
        catch (Exception exception)
        {
            _diagnostics.WriteExceptionType("LibVLC shutdown stop failed", exception);
        }

        Interlocked.Exchange(ref _media, null)?.Dispose();
        MediaPlayer.Dispose();
        _libVlc.Dispose();
    }
}
