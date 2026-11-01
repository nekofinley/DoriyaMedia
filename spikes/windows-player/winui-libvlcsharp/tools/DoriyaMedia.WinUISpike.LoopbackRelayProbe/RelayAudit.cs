namespace DoriyaMedia.WinUISpike.LoopbackRelayProbe;

public sealed record RelayAuditEvent(
    string Method,
    string Role,
    string Outcome,
    int StatusCode);

public interface IRelayAuditSink
{
    void Record(RelayAuditEvent auditEvent);
}

public sealed class NullRelayAuditSink : IRelayAuditSink
{
    public static NullRelayAuditSink Instance { get; } = new();

    private NullRelayAuditSink()
    {
    }

    public void Record(RelayAuditEvent auditEvent)
    {
        ArgumentNullException.ThrowIfNull(auditEvent);
    }
}
