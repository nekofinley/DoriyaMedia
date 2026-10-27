using System.Diagnostics;
using DoriyaMedia.WinUISpike.Core;
using LibVLCSharp.Platforms.Windows;
using Microsoft.UI.Xaml;

namespace DoriyaMedia.WinUISpike.App;

public sealed partial class MainWindow : Window
{
    private readonly RedactingDiagnosticWriter _diagnostics;
    private readonly LibVlcInstallationProbeResult _vlcInstallation;
    private LibVlcPlaybackBackend? _backend;
    private PlaybackSession? _session;
    private bool _disposed;

    public MainWindow()
    {
        InitializeComponent();
        Title = "DoriyaMedia WinUI + LibVLCSharp spike";
        AppWindow.Resize(new Windows.Graphics.SizeInt32(1000, 760));

        _diagnostics = new RedactingDiagnosticWriter(new DebugDiagnosticSink());
        _vlcInstallation = LocateVlc();

        if (_vlcInstallation.IsUsable)
        {
            RuntimeText.Text =
                $"VLC {GetVlcVersion(_vlcInstallation.RootDirectory!)} found. Initializing WinUI video surface.";
            StatusText.Text = "Initializing";
            StatusDetailText.Text = "Waiting for LibVLCSharp to provide the WinUI swap-chain options.";
        }
        else
        {
            RuntimeText.Text = "System VLC installation unavailable.";
            StatusText.Text = "Initialization failed";
            StatusDetailText.Text = _vlcInstallation.SafeMessage;
        }

        VideoView.Initialized += OnVideoViewInitialized;
        Closed += OnClosed;
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

    private static string GetVlcVersion(string vlcDirectory)
    {
        try
        {
            var version = FileVersionInfo.GetVersionInfo(Path.Combine(vlcDirectory, "libvlc.dll"));
            return version.FileVersion ?? "3.x";
        }
        catch (Exception exception) when (
            exception is IOException or UnauthorizedAccessException or ArgumentException)
        {
            return "3.x";
        }
    }

    private void OnVideoViewInitialized(object? sender, InitializedEventArgs args)
    {
        if (_session is not null || !_vlcInstallation.IsUsable)
        {
            return;
        }

        try
        {
            _backend = new LibVlcPlaybackBackend(
                _vlcInstallation.RootDirectory!,
                args.SwapChainOptions,
                _diagnostics);
            VideoView.MediaPlayer = _backend.MediaPlayer;

            _session = new PlaybackSession(_backend, _diagnostics);
            _session.StatusChanged += OnPlaybackStatusChanged;

            VideoUriBox.IsEnabled = true;
            AudioUriBox.IsEnabled = true;
            HeadersBox.IsEnabled = true;
            LoopbackHttpCheckBox.IsEnabled = true;
            OpenButton.IsEnabled = true;
            RuntimeText.Text =
                $"VLC {GetVlcVersion(_vlcInstallation.RootDirectory!)} initialized with LibVLCSharp.WinUI.";
            ApplyStatus(new PlaybackStatus(PlaybackState.Ready, "Player is ready."));
        }
        catch (Exception exception)
        {
            _diagnostics.WriteException("LibVLCSharp initialization failed", exception);
            StatusText.Text = "Initialization failed";
            StatusDetailText.Text = "LibVLC could not be initialized. Check the VLC installation and architecture.";
            VideoUriBox.IsEnabled = false;
            AudioUriBox.IsEnabled = false;
            HeadersBox.IsEnabled = false;
            LoopbackHttpCheckBox.IsEnabled = false;
            OpenButton.IsEnabled = false;
            StopButton.IsEnabled = false;
        }
    }

    private void OnOpenClicked(object sender, RoutedEventArgs args)
    {
        _session?.Open(
            VideoUriBox.Text,
            AudioUriBox.Text,
            HeadersBox.Text,
            LoopbackHttpCheckBox.IsChecked == true);
    }

    private void OnStopClicked(object sender, RoutedEventArgs args)
    {
        _session?.Stop();
    }

    private void OnPlaybackStatusChanged(object? sender, PlaybackStatusChangedEventArgs args)
    {
        if (DispatcherQueue.HasThreadAccess)
        {
            ApplyStatus(args.Status);
            return;
        }

        DispatcherQueue.TryEnqueue(() => ApplyStatus(args.Status));
    }

    private void ApplyStatus(PlaybackStatus status)
    {
        StatusText.Text = status.State.ToString();
        StatusDetailText.Text = status.SafeMessage;

        var canOpen =
            status.State is PlaybackState.Ready or PlaybackState.Stopped or PlaybackState.Ended or PlaybackState.Failed;
        var canStop = status.State is PlaybackState.Opening or PlaybackState.Playing;

        VideoUriBox.IsEnabled = _session is not null;
        AudioUriBox.IsEnabled = _session is not null;
        HeadersBox.IsEnabled = _session is not null;
        LoopbackHttpCheckBox.IsEnabled = _session is not null;
        OpenButton.IsEnabled = _session is not null && canOpen;
        StopButton.IsEnabled = _session is not null && canStop;
    }

    private void OnClosed(object sender, WindowEventArgs args)
    {
        if (_disposed)
        {
            return;
        }

        _disposed = true;
        VideoView.Initialized -= OnVideoViewInitialized;

        if (_session is not null)
        {
            _session.StatusChanged -= OnPlaybackStatusChanged;
        }

        VideoView.MediaPlayer = null;
        _session?.Dispose();
        _session = null;
        _backend = null;
    }
}
