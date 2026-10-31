using System.Collections.Generic;
using System.Globalization;
using Godot;

namespace Cribbage;

// Options come from the command line, after "--":
//
//	--server ws://host:8080/rtc   signaling URL (remembered)
//	--profile NAME                keep the session in a separate file, so
//	                              several clients can run on one machine
//	--autoplay                    play by itself, for testing (see Autoplay)
//	--user NAME --password PW     the account autoplay uses
//	--vs BOT | --open | --join    what game autoplay starts
//	--quit-after N                stop abruptly after N moves
//	--pace S                      wait S seconds between moves
//	--screenshot FILE --after S   save the window after S seconds and quit
public sealed class Options
{
    public string? Server;
    public string Profile = "default";
    public bool Autoplay;
    public string User = "", Password = "";
    public string Vs = "", Mode = "";
    public int QuitAfter;
    public double Pace = 0.02;
    public string Screenshot = "";
    public double After = 3;

    public static Options Parse(IEnumerable<string> args)
    {
        var o = new Options();
        var a = new List<string>(args);
        for (var i = 0; i < a.Count; i++)
        {
            string Next() => i + 1 < a.Count ? a[++i] : "";
            switch (a[i])
            {
                case "--server": o.Server = Next(); break;
                case "--profile": o.Profile = Next(); break;
                case "--autoplay": o.Autoplay = true; break;
                case "--user": o.User = Next(); break;
                case "--password": o.Password = Next(); break;
                case "--vs": o.Vs = Next(); o.Mode = "vs"; break;
                case "--open": o.Mode = "open"; break;
                case "--join": o.Mode = "join"; break;
                case "--quit-after": o.QuitAfter = int.Parse(Next(), CultureInfo.InvariantCulture); break;
                case "--pace": o.Pace = double.Parse(Next(), CultureInfo.InvariantCulture); break;
                case "--screenshot": o.Screenshot = Next(); break;
                case "--after": o.After = double.Parse(Next(), CultureInfo.InvariantCulture); break;
                default: GD.PushWarning($"unknown option {a[i]}"); break;
            }
        }
        return o;
    }
}

// Settings persist the server URL and session token between runs.
public sealed class Settings
{
    const string DefaultServer = "ws://127.0.0.1:8080/rtc";
    readonly ConfigFile file = new();
    readonly string path;

    public Settings(string profile)
    {
        path = $"user://{profile}.cfg";
        file.Load(path); // a missing file leaves the defaults
    }

    public string Server
    {
        get => (string)file.GetValue("connection", "server", DefaultServer);
        set { file.SetValue("connection", "server", value); Save(); }
    }

    public string Token
    {
        get => (string)file.GetValue("session", "token", "");
        set { file.SetValue("session", "token", value); Save(); }
    }

    public string LastUser
    {
        get => (string)file.GetValue("session", "username", "");
        set { file.SetValue("session", "username", value); Save(); }
    }

    void Save() => file.Save(path);
}
