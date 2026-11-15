using System.Linq;
using System.Reflection;

namespace Cribbage;

// BuildInfo is what a release build was made with (export.sh, from
// CRIBBAGE_SERVER and CRIBBAGE_VERSION): the server it connects to until
// the player picks another, and its version. Other builds use a local
// server and say "dev".
public static class BuildInfo
{
    public const string ReleasesUrl = "https://github.com/Optimoos/claude-cribbage/releases/latest";

    public static string Server { get; } = Get("CribbageServer") ?? "ws://127.0.0.1:8080/rtc";
    public static string Version { get; } = Get("CribbageVersion") ?? "dev";

    static string? Get(string key) =>
        typeof(BuildInfo).Assembly.GetCustomAttributes<AssemblyMetadataAttribute>()
            .FirstOrDefault(a => a.Key == key && !string.IsNullOrEmpty(a.Value))?.Value;
}
