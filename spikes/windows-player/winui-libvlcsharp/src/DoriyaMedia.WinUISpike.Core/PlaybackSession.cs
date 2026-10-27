namespace DoriyaMedia.WinUISpike.Core;

public sealed class PlaybackSession : IDisposable
{
    private readonly object _gate = new();
    private readonly IMediaPlaybackBackend _backend;
    private readonly RedactingDiagnosticWriter _diagnostics;
    private PlaybackStatus _lastStatus = new(PlaybackState.Ready, "Player is ready.");
    private bool _disposed;

    public PlaybackSession(
        IMediaPlaybackBackend backend,
        RedactingDiagnosticWriter diagnostics)
    {
        _backend = backend ?? throw new ArgumentNullException(nameof(backend));
        _diagnostics = diagnostics ?? throw new ArgumentNullException(nameof(diagnostics));
        _backend.StateChanged += OnBackendStateChanged;
    }

    public event EventHandler<PlaybackStatusChangedEventArgs>? StatusChanged;

    public PlaybackStatus LastStatus
    {
        get
        {
            lock (_gate)
            {
                return _lastStatus;
            }
        }
    }

    public void Open(
        string? rawVideoUri,
        string? rawAudioUri = null,
        string? rawHeaders = null,
        bool allowLoopbackHttp = false)
    {
        ThrowIfDisposed();

        var planResult = MediaLoadPlanBuilder.Build(
            rawVideoUri,
            rawAudioUri,
            rawHeaders,
            allowLoopbackHttp);
        if (!planResult.IsValid)
        {
            Publish(
                PlaybackState.Failed,
                planResult.SafeMessage,
                planResult.ErrorCode);
            return;
        }

        Publish(PlaybackState.Opening, "Opening media.");

        try
        {
            var result = _backend.Open(planResult.Plan!);
            if (!result.IsSuccess)
            {
                Publish(PlaybackState.Failed, result.SafeMessage, result.ErrorCode);
            }
        }
        catch (Exception exception)
        {
            _diagnostics.WriteExceptionType("Playback backend open failed", exception);
            Publish(
                PlaybackState.Failed,
                "The media could not be opened.",
                PlaybackErrorCode.PlaybackFailed);
        }
    }

    public void Stop()
    {
        ThrowIfDisposed();

        try
        {
            var result = _backend.Stop();
            Publish(
                result.IsSuccess ? PlaybackState.Stopped : PlaybackState.Failed,
                result.IsSuccess ? "Playback stopped." : result.SafeMessage,
                result.IsSuccess ? PlaybackErrorCode.None : result.ErrorCode);
        }
        catch (Exception exception)
        {
            _diagnostics.WriteExceptionType("Playback backend stop failed", exception);
            Publish(
                PlaybackState.Failed,
                "Playback could not be stopped.",
                PlaybackErrorCode.PlaybackFailed);
        }
    }

    private void OnBackendStateChanged(object? sender, BackendStateChangedEventArgs args)
    {
        switch (args.State)
        {
            case BackendPlaybackState.Opening:
                Publish(PlaybackState.Opening, "Opening media.");
                break;
            case BackendPlaybackState.Playing:
                Publish(PlaybackState.Playing, "Playing.");
                break;
            case BackendPlaybackState.Stopped:
                Publish(PlaybackState.Stopped, "Playback stopped.");
                break;
            case BackendPlaybackState.Ended:
                Publish(PlaybackState.Ended, "Playback ended.");
                break;
            case BackendPlaybackState.Failed:
                Publish(
                    PlaybackState.Failed,
                    "Playback failed.",
                    args.ErrorCode == PlaybackErrorCode.None
                        ? PlaybackErrorCode.PlaybackFailed
                        : args.ErrorCode);
                break;
            default:
                throw new ArgumentOutOfRangeException(nameof(args), args.State, "Unknown backend state.");
        }
    }

    private void Publish(
        PlaybackState state,
        string safeMessage,
        PlaybackErrorCode errorCode = PlaybackErrorCode.None)
    {
        EventHandler<PlaybackStatusChangedEventArgs>? handler;
        PlaybackStatus status;

        lock (_gate)
        {
            if (_disposed)
            {
                return;
            }

            _lastStatus = new PlaybackStatus(state, safeMessage, errorCode);
            status = _lastStatus;
            handler = StatusChanged;
        }

        handler?.Invoke(this, new PlaybackStatusChangedEventArgs(status));
    }

    private void ThrowIfDisposed()
    {
        lock (_gate)
        {
            ObjectDisposedException.ThrowIf(_disposed, this);
        }
    }

    public void Dispose()
    {
        lock (_gate)
        {
            if (_disposed)
            {
                return;
            }

            _disposed = true;
        }

        _backend.StateChanged -= OnBackendStateChanged;

        try
        {
            _backend.Dispose();
        }
        catch (Exception exception)
        {
            _diagnostics.WriteException("Playback backend disposal failed", exception);
        }
    }
}
