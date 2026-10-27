using System.Diagnostics;
using DoriyaMedia.WinUISpike.Core;

namespace DoriyaMedia.WinUISpike.App;

internal sealed class DebugDiagnosticSink : IDiagnosticSink
{
    public void WriteLine(string line)
    {
        Debug.WriteLine(line);
    }
}
