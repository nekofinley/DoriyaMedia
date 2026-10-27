using DoriyaMedia.WinUISpike.Core;
using Microsoft.UI.Xaml;

namespace DoriyaMedia.WinUISpike.App;

public partial class App : Application
{
    private Window? _window;
    private readonly RedactingDiagnosticWriter _diagnostics =
        new(new DebugDiagnosticSink());

    public App()
    {
        InitializeComponent();
        UnhandledException += OnUnhandledException;
    }

    protected override void OnLaunched(LaunchActivatedEventArgs args)
    {
        _window = new MainWindow();
        _window.Activate();
    }

    private void OnUnhandledException(object sender, Microsoft.UI.Xaml.UnhandledExceptionEventArgs args)
    {
        _diagnostics.WriteException("Unhandled WinUI exception", args.Exception);
    }
}
