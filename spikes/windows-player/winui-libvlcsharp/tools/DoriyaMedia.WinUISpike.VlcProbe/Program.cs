using DoriyaMedia.WinUISpike.Core;

var programFiles = Environment.GetFolderPath(Environment.SpecialFolder.ProgramFiles);
var programFilesX86 = Environment.GetFolderPath(Environment.SpecialFolder.ProgramFilesX86);
var installation = LibVlcLocator.Locate(
    explicitPath: null,
    environmentPath: Environment.GetEnvironmentVariable("DORIYA_VLC_HOME"),
    defaultDirectories:
    [
        Path.Combine(programFiles, "VideoLAN", "VLC"),
        Path.Combine(programFilesX86, "VideoLAN", "VLC")
    ],
    new PhysicalFileSystemProbe());

if (!installation.IsUsable)
{
    Console.Error.WriteLine(installation.SafeMessage);
    return 2;
}

try
{
    LibVLCSharp.Shared.Core.Initialize(installation.RootDirectory!);
    using var libVlc = new LibVLCSharp.Shared.LibVLC(enableDebugLogs: false, "--quiet");
    Console.WriteLine("LibVLC initialization probe: PASS");
    Console.WriteLine($"LibVLC runtime version: {libVlc.Version}");

    using var media = new LibVLCSharp.Shared.Media(
        libVlc,
        new Uri("https://media.invalid/video.mp4"));
    media.AddOption(":http-referrer=https://referrer.invalid/watch");
    media.AddOption(":http-user-agent=DoriyaMediaVlcProbe/1.0");
    if (!media.AddSlave(
            LibVLCSharp.Shared.MediaSlaveType.Audio,
            4,
            new Uri("https://media.invalid/audio.mp4")))
    {
        Console.Error.WriteLine("LibVLC dual-source media construction: FAIL");
        return 3;
    }

    if (media.Slaves.Length != 1 ||
        !string.Equals(
            media.Slaves[0].Uri,
            "https://media.invalid/audio.mp4",
            StringComparison.Ordinal))
    {
        Console.Error.WriteLine("LibVLC audio slave list probe: FAIL");
        return 4;
    }

    Console.WriteLine("LibVLC dual-source media construction: PASS");
    return 0;
}
catch (Exception exception)
{
    var writer = new RedactingDiagnosticWriter(new ConsoleDiagnosticSink());
    writer.WriteException("LibVLC initialization probe failed", exception);
    return 1;
}

internal sealed class ConsoleDiagnosticSink : IDiagnosticSink
{
    public void WriteLine(string line)
    {
        Console.Error.WriteLine(line);
    }
}
