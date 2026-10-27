namespace DoriyaMedia.WinUISpike.Core;

public enum PlaybackState
{
    Ready,
    Opening,
    Playing,
    Stopped,
    Ended,
    Failed
}

public sealed class PlaybackStatus
{
    public PlaybackStatus(
        PlaybackState state,
        string safeMessage,
        PlaybackErrorCode errorCode = PlaybackErrorCode.None)
    {
        State = state;
        SafeMessage = safeMessage;
        ErrorCode = errorCode;
    }

    public PlaybackState State { get; }

    public string SafeMessage { get; }

    public PlaybackErrorCode ErrorCode { get; }
}

public sealed class PlaybackStatusChangedEventArgs : EventArgs
{
    public PlaybackStatusChangedEventArgs(PlaybackStatus status)
    {
        Status = status;
    }

    public PlaybackStatus Status { get; }
}

public enum BackendPlaybackState
{
    Opening,
    Playing,
    Stopped,
    Ended,
    Failed
}

public sealed class BackendStateChangedEventArgs : EventArgs
{
    public BackendStateChangedEventArgs(
        BackendPlaybackState state,
        PlaybackErrorCode errorCode = PlaybackErrorCode.None)
    {
        State = state;
        ErrorCode = errorCode;
    }

    public BackendPlaybackState State { get; }

    public PlaybackErrorCode ErrorCode { get; }
}

public sealed class BackendOperationResult
{
    private BackendOperationResult(
        bool isSuccess,
        string safeMessage,
        PlaybackErrorCode errorCode)
    {
        IsSuccess = isSuccess;
        SafeMessage = safeMessage;
        ErrorCode = errorCode;
    }

    public bool IsSuccess { get; }

    public string SafeMessage { get; }

    public PlaybackErrorCode ErrorCode { get; }

    public static BackendOperationResult Success(string safeMessage) =>
        new(true, safeMessage, PlaybackErrorCode.None);

    public static BackendOperationResult Failure(
        string safeMessage,
        PlaybackErrorCode errorCode = PlaybackErrorCode.PlaybackFailed) =>
        new(false, safeMessage, errorCode);
}

public interface IMediaPlaybackBackend : IDisposable
{
    event EventHandler<BackendStateChangedEventArgs>? StateChanged;

    BackendOperationResult Open(MediaLoadPlan plan);

    BackendOperationResult Stop();
}
