using DoriyaMedia.WinUISpike.Core;
using Xunit;

namespace DoriyaMedia.WinUISpike.Core.Tests;

public sealed class LibVlcLocatorTests
{
    [Fact]
    public void Locate_UsesACompleteExplicitInstallation()
    {
        var fileSystem = FakeFileSystem.WithCompleteInstallation(@"C:\vlc-explicit");

        var result = LibVlcLocator.Locate(
            @"C:\vlc-explicit",
            @"C:\ignored-environment",
            [@"C:\ignored-default"],
            fileSystem);

        Assert.True(result.IsUsable);
        Assert.Equal(@"C:\vlc-explicit", result.RootDirectory);
        Assert.Equal(LibVlcProbeFailure.None, result.Failure);
    }

    [Fact]
    public void Locate_FailsClosedWhenAnExplicitPathIsIncomplete()
    {
        var fileSystem = FakeFileSystem.WithCompleteInstallation(@"C:\vlc-default");
        fileSystem.Directories.Add(@"C:\vlc-explicit");
        fileSystem.Files.Add(@"C:\vlc-explicit\libvlc.dll");

        var result = LibVlcLocator.Locate(
            @"C:\vlc-explicit",
            null,
            [@"C:\vlc-default"],
            fileSystem);

        Assert.False(result.IsUsable);
        Assert.Equal(LibVlcProbeFailure.Incomplete, result.Failure);
        Assert.DoesNotContain(@"C:\vlc-default", result.SafeMessage, StringComparison.OrdinalIgnoreCase);
    }

    [Fact]
    public void Locate_PrefersEnvironmentBeforePlatformDefaults()
    {
        var fileSystem = FakeFileSystem.WithCompleteInstallation(@"C:\vlc-environment");
        fileSystem.AddCompleteInstallation(@"C:\Program Files\VideoLAN\VLC");

        var result = LibVlcLocator.Locate(
            null,
            @"C:\vlc-environment",
            [@"C:\Program Files\VideoLAN\VLC"],
            fileSystem);

        Assert.True(result.IsUsable);
        Assert.Equal(@"C:\vlc-environment", result.RootDirectory);
    }

    [Fact]
    public void Locate_SkipsAnIncompleteDefaultAndUsesTheNextCompleteCandidate()
    {
        var fileSystem = new FakeFileSystem();
        fileSystem.Directories.Add(@"C:\first");
        fileSystem.Files.Add(@"C:\first\libvlc.dll");
        fileSystem.AddCompleteInstallation(@"C:\second");

        var result = LibVlcLocator.Locate(
            null,
            null,
            [@"C:\first", @"C:\second"],
            fileSystem);

        Assert.True(result.IsUsable);
        Assert.Equal(@"C:\second", result.RootDirectory);
    }

    [Fact]
    public void Locate_ReportsIncompleteWhenNoDefaultIsUsable()
    {
        var fileSystem = new FakeFileSystem();
        fileSystem.Directories.Add(@"C:\incomplete");
        fileSystem.Files.Add(@"C:\incomplete\libvlc.dll");

        var result = LibVlcLocator.Locate(
            null,
            null,
            [@"C:\missing", @"C:\incomplete"],
            fileSystem);

        Assert.False(result.IsUsable);
        Assert.Equal(LibVlcProbeFailure.Incomplete, result.Failure);
    }

    [Fact]
    public void Locate_ReturnsNotFoundWhenNoCandidateExists()
    {
        var result = LibVlcLocator.Locate(
            null,
            null,
            [@"C:\missing"],
            new FakeFileSystem());

        Assert.False(result.IsUsable);
        Assert.Equal(LibVlcProbeFailure.NotFound, result.Failure);
        Assert.Contains("DORIYA_VLC_HOME", result.SafeMessage, StringComparison.Ordinal);
    }

    private sealed class FakeFileSystem : IFileSystemProbe
    {
        public HashSet<string> Files { get; } = new(StringComparer.OrdinalIgnoreCase);

        public HashSet<string> Directories { get; } = new(StringComparer.OrdinalIgnoreCase);

        public static FakeFileSystem WithCompleteInstallation(string root)
        {
            var result = new FakeFileSystem();
            result.AddCompleteInstallation(root);
            return result;
        }

        public void AddCompleteInstallation(string root)
        {
            Directories.Add(root);
            Files.Add(Path.Combine(root, "libvlc.dll"));
            Files.Add(Path.Combine(root, "libvlccore.dll"));
            Directories.Add(Path.Combine(root, "plugins"));
        }

        public bool FileExists(string path) => Files.Contains(path);

        public bool DirectoryExists(string path) => Directories.Contains(path);
    }
}
