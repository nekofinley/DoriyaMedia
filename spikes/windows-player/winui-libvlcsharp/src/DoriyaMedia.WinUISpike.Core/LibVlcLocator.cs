namespace DoriyaMedia.WinUISpike.Core;

public interface IFileSystemProbe
{
    bool FileExists(string path);

    bool DirectoryExists(string path);
}

public sealed class PhysicalFileSystemProbe : IFileSystemProbe
{
    public bool FileExists(string path) => File.Exists(path);

    public bool DirectoryExists(string path) => Directory.Exists(path);
}

public enum LibVlcProbeFailure
{
    None,
    NotFound,
    Incomplete,
    InvalidPath
}

public sealed class LibVlcInstallationProbeResult
{
    private LibVlcInstallationProbeResult(
        bool isUsable,
        string? rootDirectory,
        LibVlcProbeFailure failure,
        string safeMessage)
    {
        IsUsable = isUsable;
        RootDirectory = rootDirectory;
        Failure = failure;
        SafeMessage = safeMessage;
    }

    public bool IsUsable { get; }

    public string? RootDirectory { get; }

    public LibVlcProbeFailure Failure { get; }

    public string SafeMessage { get; }

    public static LibVlcInstallationProbeResult Usable(string rootDirectory) =>
        new(true, rootDirectory, LibVlcProbeFailure.None, "VLC installation found.");

    public static LibVlcInstallationProbeResult Failed(LibVlcProbeFailure failure, string safeMessage) =>
        new(false, null, failure, safeMessage);
}

public static class LibVlcLocator
{
    public static LibVlcInstallationProbeResult Locate(
        string? explicitPath,
        string? environmentPath,
        IEnumerable<string?> defaultDirectories,
        IFileSystemProbe fileSystem)
    {
        ArgumentNullException.ThrowIfNull(defaultDirectories);
        ArgumentNullException.ThrowIfNull(fileSystem);

        if (!string.IsNullOrWhiteSpace(explicitPath))
        {
            return ProbeConfiguredPath(explicitPath, "The explicit VLC path", fileSystem);
        }

        if (!string.IsNullOrWhiteSpace(environmentPath))
        {
            return ProbeConfiguredPath(environmentPath, "The DORIYA_VLC_HOME VLC path", fileSystem);
        }

        var incompleteCandidateFound = false;
        var seen = new HashSet<string>(StringComparer.OrdinalIgnoreCase);

        foreach (var candidate in defaultDirectories)
        {
            if (string.IsNullOrWhiteSpace(candidate) || !seen.Add(candidate.Trim()))
            {
                continue;
            }

            var result = ProbeCandidate(candidate, fileSystem);
            if (result.IsUsable)
            {
                return result;
            }

            incompleteCandidateFound |= result.Failure == LibVlcProbeFailure.Incomplete;
        }

        return incompleteCandidateFound
            ? LibVlcInstallationProbeResult.Failed(
                LibVlcProbeFailure.Incomplete,
                "A VLC directory was found, but libvlc.dll, libvlccore.dll, or the plugins directory is missing.")
            : LibVlcInstallationProbeResult.Failed(
                LibVlcProbeFailure.NotFound,
                "VLC was not found. Install VLC 3.x or set DORIYA_VLC_HOME.");
    }

    private static LibVlcInstallationProbeResult ProbeConfiguredPath(
        string path,
        string sourceDescription,
        IFileSystemProbe fileSystem)
    {
        var result = ProbeCandidate(path, fileSystem);
        if (result.IsUsable)
        {
            return result;
        }

        return result.Failure switch
        {
            LibVlcProbeFailure.InvalidPath => LibVlcInstallationProbeResult.Failed(
                LibVlcProbeFailure.InvalidPath,
                $"{sourceDescription} is not a valid directory path."),
            LibVlcProbeFailure.Incomplete => LibVlcInstallationProbeResult.Failed(
                LibVlcProbeFailure.Incomplete,
                $"{sourceDescription} is incomplete; libvlc.dll, libvlccore.dll, and plugins are required."),
            _ => LibVlcInstallationProbeResult.Failed(
                LibVlcProbeFailure.NotFound,
                $"{sourceDescription} does not exist.")
        };
    }

    private static LibVlcInstallationProbeResult ProbeCandidate(
        string candidate,
        IFileSystemProbe fileSystem)
    {
        string fullPath;
        try
        {
            fullPath = Path.TrimEndingDirectorySeparator(Path.GetFullPath(candidate.Trim()));
        }
        catch (Exception exception) when (
            exception is ArgumentException or NotSupportedException or PathTooLongException)
        {
            return LibVlcInstallationProbeResult.Failed(
                LibVlcProbeFailure.InvalidPath,
                "The configured VLC path is invalid.");
        }

        if (!fileSystem.DirectoryExists(fullPath))
        {
            return LibVlcInstallationProbeResult.Failed(
                LibVlcProbeFailure.NotFound,
                "The configured VLC directory does not exist.");
        }

        var hasLibVlc = fileSystem.FileExists(Path.Combine(fullPath, "libvlc.dll"));
        var hasLibVlcCore = fileSystem.FileExists(Path.Combine(fullPath, "libvlccore.dll"));
        var hasPlugins = fileSystem.DirectoryExists(Path.Combine(fullPath, "plugins"));

        return hasLibVlc && hasLibVlcCore && hasPlugins
            ? LibVlcInstallationProbeResult.Usable(fullPath)
            : LibVlcInstallationProbeResult.Failed(
                LibVlcProbeFailure.Incomplete,
                "The VLC directory does not contain the required runtime files.");
    }
}
