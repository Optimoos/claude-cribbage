using System.Collections.Generic;
using System.Globalization;
using Godot;

namespace Cribbage;

// Options come from the command line, after "--":
//
//	--server ws://host:8080/rtc   signaling URL (remembered)
//	--theme felt|parchment|midnight  colour theme (remembered)
//	--profile NAME                keep the session in a separate file, so
//	                              several clients can run on one machine
//	--autoplay                    play by itself, for testing (see Autoplay)
//	--user NAME --password PW     the account autoplay uses
//	--vs BOT | --open | --join    what game autoplay starts
//	--invite USER | --accept      or invite USER, or accept the first invite
//	--quit-after N                stop abruptly after N moves
//	--pace S                      wait S seconds between moves
//	--animate                     animate while autoplaying (rapid otherwise)
//	--screenshot FILE --after S   save the window after S seconds and quit
public sealed class Options
{
    public string? Server, Theme;
    public string Profile = "default";
    public bool Autoplay;
    public string User = "", Password = "";
    public string Vs = "", Mode = "", Invite = "";
    public int QuitAfter;
    public double Pace = 0.02;
    public bool Animate;
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
                case "--theme": o.Theme = Next(); break;
                case "--profile": o.Profile = Next(); break;
                case "--autoplay": o.Autoplay = true; break;
                case "--user": o.User = Next(); break;
                case "--password": o.Password = Next(); break;
                case "--vs": o.Vs = Next(); o.Mode = "vs"; break;
                case "--open": o.Mode = "open"; break;
                case "--join": o.Mode = "join"; break;
                case "--invite": o.Invite = Next(); o.Mode = "invite"; break;
                case "--accept": o.Mode = "accept"; break;
                case "--quit-after": o.QuitAfter = int.Parse(Next(), CultureInfo.InvariantCulture); break;
                case "--animate": o.Animate = true; break;
                case "--pace": o.Pace = double.Parse(Next(), CultureInfo.InvariantCulture); break;
                case "--screenshot": o.Screenshot = Next(); break;
                case "--after": o.After = double.Parse(Next(), CultureInfo.InvariantCulture); break;
                default: GD.PushWarning($"unknown option {a[i]}"); break;
            }
        }
        return o;
    }
}

// Settings persist the server URL, session token and preferences between
// runs.
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

    public string Theme
    {
        get => (string)file.GetValue("prefs", "theme", "felt");
        set { file.SetValue("prefs", "theme", value); Save(); }
    }

    // Rapid applies updates at once, with no animation or sound.
    public bool Rapid
    {
        get => (bool)file.GetValue("prefs", "rapid", false);
        set { file.SetValue("prefs", "rapid", value); Save(); }
    }

    public bool Muted
    {
        get => (bool)file.GetValue("prefs", "muted", false);
        set { file.SetValue("prefs", "muted", value); Save(); }
    }

    void Save() => file.Save(path);
}
