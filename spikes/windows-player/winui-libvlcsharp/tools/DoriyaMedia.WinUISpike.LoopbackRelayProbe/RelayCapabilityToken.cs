using System.Diagnostics.CodeAnalysis;
using System.Security.Cryptography;
using System.Text;

namespace DoriyaMedia.WinUISpike.LoopbackRelayProbe;

public static class RelayCapabilityToken
{
    public const int EntropyBytes = 32;
    public const int EncodedLength = 43;

    public static string Generate()
    {
        Span<byte> entropy = stackalloc byte[EntropyBytes];
        RandomNumberGenerator.Fill(entropy);
        return Convert.ToBase64String(entropy)
            .TrimEnd('=')
            .Replace('+', '-')
            .Replace('/', '_');
    }

    public static bool IsWellFormed([NotNullWhen(true)] string? token)
    {
        if (token is null || token.Length != EncodedLength)
        {
            return false;
        }

        foreach (var character in token)
        {
            if (!char.IsAsciiLetterOrDigit(character) && character is not '-' and not '_')
            {
                return false;
            }
        }

        return true;
    }

    internal static bool FixedTimeEquals(string expected, string actual)
    {
        if (!IsWellFormed(expected) || !IsWellFormed(actual))
        {
            return false;
        }

        var expectedBytes = Encoding.ASCII.GetBytes(expected);
        var actualBytes = Encoding.ASCII.GetBytes(actual);
        return CryptographicOperations.FixedTimeEquals(expectedBytes, actualBytes);
    }
}
