namespace DoriyaMedia.WinUISpike.Core;

public interface IDiagnosticSink
{
    void WriteLine(string line);
}

public sealed class RedactingDiagnosticWriter
{
    private readonly IDiagnosticSink _sink;

    public RedactingDiagnosticWriter(IDiagnosticSink sink)
    {
        _sink = sink ?? throw new ArgumentNullException(nameof(sink));
    }

    public void Write(string message)
    {
        _sink.WriteLine(SensitiveDataRedactor.Redact(message));
    }

    public void WriteException(string safeOperation, Exception exception)
    {
        ArgumentNullException.ThrowIfNull(exception);
        Write($"{safeOperation}: {exception.GetType().Name}: {exception.Message}");
    }

    public void WriteExceptionType(string safeOperation, Exception exception)
    {
        ArgumentNullException.ThrowIfNull(exception);
        Write($"{safeOperation}: {exception.GetType().Name}.");
    }
}
