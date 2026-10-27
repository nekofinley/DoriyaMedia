using DoriyaMedia.WinUISpike.Core;
using Xunit;

namespace DoriyaMedia.WinUISpike.Core.Tests;

public sealed class PlaybackSessionTests
{
    [Fact]
    public void Open_RejectsInvalidUriBeforeCallingTheBackend()
    {
        var backend = new FakeBackend();
        using var session = new PlaybackSession(backend, CreateWriter(out _));

        session.Open("file:///C:/private.mp4");

        Assert.Equal(0, backend.OpenCount);
        Assert.Equal(PlaybackState.Failed, session.LastStatus.State);
        Assert.DoesNotContain("private.mp4", session.LastStatus.SafeMessage, StringComparison.OrdinalIgnoreCase);
    }

    [Fact]
    public void Open_ForwardsAValidatedUriAndAllowsTheBackendToPublishPlaying()
    {
        var backend = new FakeBackend();
        backend.OnOpen = _ =>
        {
            backend.RaiseState(BackendPlaybackState.Playing);
            return BackendOperationResult.Success("accepted");
        };
        using var session = new PlaybackSession(backend, CreateWriter(out _));
        var statuses = new List<PlaybackState>();
        session.StatusChanged += (_, args) => statuses.Add(args.Status.State);

        session.Open("https://media.example.test/video.mp4?token=secret");

        Assert.Equal(1, backend.OpenCount);
        Assert.Equal(
            "https://media.example.test/video.mp4?token=secret",
            backend.LastOpenedPlan?.VideoUri);
        Assert.Equal(MediaSourceMode.SingleSource, backend.LastOpenedPlan?.Mode);
        Assert.Equal(PlaybackState.Playing, session.LastStatus.State);
        Assert.Equal([PlaybackState.Opening, PlaybackState.Playing], statuses);
    }

    [Fact]
    public void Open_ForwardsDualSourceAndHeadersToTheBackend()
    {
        var backend = new FakeBackend();
        using var session = new PlaybackSession(backend, CreateWriter(out _));

        session.Open(
            "https://media.example.test/video.mp4",
            "https://media.example.test/audio.mp4",
            "Referer: https://referrer.example.test/watch\nUser-Agent: DoriyaSpike/1.0");

        Assert.NotNull(backend.LastOpenedPlan);
        Assert.Equal(MediaSourceMode.VideoWithAudioSlave, backend.LastOpenedPlan.Mode);
        Assert.Equal(
            "https://media.example.test/audio.mp4",
            backend.LastOpenedPlan.AudioSlaveUri);
        Assert.Equal(
            [
                ":http-referrer=https://referrer.example.test/watch",
                ":http-user-agent=DoriyaSpike/1.0"
            ],
            backend.LastOpenedPlan.LibVlcMediaOptions);
        Assert.Equal(PlaybackState.Opening, session.LastStatus.State);
    }

    [Fact]
    public void Open_WhenAudioSlaveCannotBeAdded_PublishesMergeFailure()
    {
        var backend = new FakeBackend
        {
            OnOpen = _ => BackendOperationResult.Failure(
                "LibVLC could not add the audio source.",
                PlaybackErrorCode.MediaMergeUnavailable)
        };
        using var session = new PlaybackSession(backend, CreateWriter(out _));

        session.Open(
            "https://media.example.test/video.mp4",
            "https://media.example.test/audio.mp4");

        Assert.Equal(PlaybackState.Failed, session.LastStatus.State);
        Assert.Equal(PlaybackErrorCode.MediaMergeUnavailable, session.LastStatus.ErrorCode);
        Assert.DoesNotContain("audio.mp4", session.LastStatus.SafeMessage, StringComparison.Ordinal);
    }

    [Fact]
    public void Open_WhenLibVlcIsUnavailable_PublishesAFailureWithoutLeakingHeaderValues()
    {
        var diagnostics = CreateWriter(out var sink);
        var backend = new FakeBackend
        {
            OnOpen = _ => throw new DllNotFoundException(
                "libvlc.dll missing for DoriyaSecretAgent/1.0 https://private.example.test/video")
        };
        using var session = new PlaybackSession(backend, diagnostics);

        session.Open(
            "https://media.example.test/video.mp4",
            rawHeaders: "User-Agent: DoriyaSecretAgent/1.0");

        Assert.Equal(PlaybackState.Failed, session.LastStatus.State);
        Assert.Equal(PlaybackErrorCode.PlaybackFailed, session.LastStatus.ErrorCode);
        var line = Assert.Single(sink.Lines);
        Assert.Contains(nameof(DllNotFoundException), line, StringComparison.Ordinal);
        Assert.DoesNotContain("DoriyaSecretAgent", line, StringComparison.Ordinal);
        Assert.DoesNotContain("private.example.test", line, StringComparison.Ordinal);
    }

    [Fact]
    public void Stop_IsForwardedAndReportsStopped()
    {
        var backend = new FakeBackend();
        using var session = new PlaybackSession(backend, CreateWriter(out _));

        session.Stop();

        Assert.Equal(1, backend.StopCount);
        Assert.Equal(PlaybackState.Stopped, session.LastStatus.State);
    }

    [Theory]
    [InlineData(BackendPlaybackState.Opening, PlaybackState.Opening)]
    [InlineData(BackendPlaybackState.Playing, PlaybackState.Playing)]
    [InlineData(BackendPlaybackState.Stopped, PlaybackState.Stopped)]
    [InlineData(BackendPlaybackState.Ended, PlaybackState.Ended)]
    [InlineData(BackendPlaybackState.Failed, PlaybackState.Failed)]
    public void BackendState_IsMappedToASafePublicStatus(
        BackendPlaybackState backendState,
        PlaybackState expectedState)
    {
        var backend = new FakeBackend();
        using var session = new PlaybackSession(backend, CreateWriter(out _));

        backend.RaiseState(backendState);

        Assert.Equal(expectedState, session.LastStatus.State);
        Assert.DoesNotContain("http", session.LastStatus.SafeMessage, StringComparison.OrdinalIgnoreCase);
    }

    [Fact]
    public void BackendFailureState_PreservesItsPlaybackErrorCode()
    {
        var backend = new FakeBackend();
        using var session = new PlaybackSession(backend, CreateWriter(out _));

        backend.RaiseState(
            BackendPlaybackState.Failed,
            PlaybackErrorCode.MediaMergeFailed);

        Assert.Equal(PlaybackState.Failed, session.LastStatus.State);
        Assert.Equal(PlaybackErrorCode.MediaMergeFailed, session.LastStatus.ErrorCode);
    }

    [Fact]
    public void BackendException_IsRedactedBeforeReachingTheDiagnosticSink()
    {
        var diagnostics = CreateWriter(out var sink);
        var backend = new FakeBackend
        {
            OnOpen = _ => throw new InvalidOperationException(
                "https://cdn.example.test/media?token=secret " +
                "User-Agent: DoriyaSecretAgent/1.0 Authorization: Bearer secret")
        };
        using var session = new PlaybackSession(backend, diagnostics);

        session.Open(
            "https://media.example.test/video.mp4",
            rawHeaders: "User-Agent: DoriyaSecretAgent/1.0");

        Assert.Equal(PlaybackState.Failed, session.LastStatus.State);
        Assert.Single(sink.Lines);
        Assert.DoesNotContain("cdn.example.test", sink.Lines[0], StringComparison.Ordinal);
        Assert.DoesNotContain("DoriyaSecretAgent", sink.Lines[0], StringComparison.Ordinal);
        Assert.DoesNotContain("Bearer secret", sink.Lines[0], StringComparison.Ordinal);
    }

    private static RedactingDiagnosticWriter CreateWriter(out CapturingSink sink)
    {
        sink = new CapturingSink();
        return new RedactingDiagnosticWriter(sink);
    }

    private sealed class CapturingSink : IDiagnosticSink
    {
        public List<string> Lines { get; } = [];

        public void WriteLine(string line) => Lines.Add(line);
    }

    private sealed class FakeBackend : IMediaPlaybackBackend
    {
        public event EventHandler<BackendStateChangedEventArgs>? StateChanged;

        public int OpenCount { get; private set; }

        public int StopCount { get; private set; }

        public MediaLoadPlan? LastOpenedPlan { get; private set; }

        public Func<MediaLoadPlan, BackendOperationResult> OnOpen { get; set; } =
            _ => BackendOperationResult.Success("accepted");

        public BackendOperationResult Open(MediaLoadPlan plan)
        {
            OpenCount++;
            LastOpenedPlan = plan;
            return OnOpen(plan);
        }

        public BackendOperationResult Stop()
        {
            StopCount++;
            return BackendOperationResult.Success("stopped");
        }

        public void RaiseState(
            BackendPlaybackState state,
            PlaybackErrorCode errorCode = PlaybackErrorCode.None)
        {
            StateChanged?.Invoke(this, new BackendStateChangedEventArgs(state, errorCode));
        }

        public void Dispose()
        {
        }
    }
}
