using System.Collections.Generic;
using System.Linq;
using Cribbage.Protocol;
using Cribbage.Ui;
using Godot;

namespace Cribbage;

// Account settings, password resets and game history.
public partial class Main
{
    // Handlers for replies to particular requests, by request id. A handler
    // returns true if it dealt with the reply completely.
    readonly Dictionary<uint, System.Func<ServerMessage, bool>> replies = new();
    Account account = new();

    Window? accountWindow, forgotWindow, historyWindow;
    Label? emailStatus;
    Button? emailRemove;

    // History, as fetched so far.
    readonly List<GameSummary> historyGames = new();
    List<Record> historyRecords = new();
    Record? historyTotal;
    string historyNext = "";
    bool historyLoaded;
    VBoxContainer? historyBox;

    uint Request(ClientMessage m, System.Func<ServerMessage, bool> onReply)
    {
        var id = Send(m);
        if (id != 0)
        {
            replies[id] = onReply;
        }
        return id;
    }

    // TakeReply passes m to the handler waiting for it, if any, and reports
    // whether that dealt with it.
    bool TakeReply(ServerMessage m) =>
        m.RequestId != 0 && replies.Remove(m.RequestId, out var h) && h(m);

    void CloseAccountWindows()
    {
        foreach (var w in new[] { accountWindow, forgotWindow, historyWindow })
        {
            w?.QueueFree();
        }
        accountWindow = forgotWindow = historyWindow = null;
        replies.Clear();
    }

    // Window makes a dialog window of a fixed size holding a padded,
    // scrolling column, returned. (Sizing the window to its content does not
    // work: wrapped labels report their height at zero width.)
    VBoxContainer Window(string title, int width, int height, out Window window, bool closable = true)
    {
        var w = new Window
        {
            Title = title, Transient = true, Exclusive = true, Size = new Vector2I(width, height),
            // A window that cannot be closed has no title bar, so no close
            // button that does nothing.
            Borderless = !closable,
        };
        var panel = new PanelContainer();
        panel.AddThemeStyleboxOverride("panel", Style.Box(Style.P.Page, 0));
        w.AddChild(panel);
        panel.SetAnchorsAndOffsetsPreset(LayoutPreset.FullRect);
        var margin = new MarginContainer();
        foreach (var side in new[] { "left", "right", "top", "bottom" })
        {
            margin.AddThemeConstantOverride("margin_" + side, 18);
        }
        panel.AddChild(margin);
        var scroll = new ScrollContainer { HorizontalScrollMode = ScrollContainer.ScrollMode.Disabled };
        margin.AddChild(scroll);
        var box = new VBoxContainer { SizeFlagsHorizontal = SizeFlags.ExpandFill };
        box.AddThemeConstantOverride("separation", 8);
        scroll.AddChild(box);
        w.CloseRequested += () =>
        {
            if (closable)
            {
                w.QueueFree();
            }
        };
        ui.AddChild(w);
        window = w;
        return box;
    }

    static Label Wrapped(string text, int size = 14, Color? color = null)
    {
        var l = Style.Label(text, size, color);
        l.AutowrapMode = TextServer.AutowrapMode.WordSmart;
        l.SizeFlagsHorizontal = SizeFlags.ExpandFill;
        return l;
    }

    static LineEdit Field(VBoxContainer box, string label, bool secret = false, string text = "")
    {
        box.AddChild(Style.Label(label, 14, Style.P.Muted));
        var e = new LineEdit { Secret = secret, Text = text, MaxLength = 254 };
        box.AddChild(e);
        return e;
    }

    // ---------------------------------------------------------------
    // Account: password and email

    void ShowAccount()
    {
        accountWindow?.QueueFree();
        var must = account.MustChangePassword;
        var box = Window("Account", 440, must ? 470 : 620, out var w, closable: !must);
        accountWindow = w;
        w.TreeExiting += () => { if (accountWindow == w) accountWindow = null; };
        if (must)
        {
            box.AddChild(Style.Label("Account", 22, Style.P.Accent));
            box.AddChild(Wrapped("Your password was reset. Choose a new password to carry on.", 15, Style.P.Accent));
        }

        box.AddChild(Heading("Change password"));
        var old = Field(box, must ? "Temporary password" : "Current password", true);
        var pw = Field(box, "New password", true);
        var again = Field(box, "New password again", true);
        var pwMsg = Wrapped("", 14, Style.P.Theirs);
        void ChangePassword()
        {
            if (pw.Text != again.Text)
            {
                pwMsg.Text = "The new passwords don't match.";
                return;
            }
            pwMsg.Text = "";
            var sent = Request(new ClientMessage { ChangePassword = new ChangePassword { OldPassword = old.Text, NewPassword = pw.Text } }, m =>
            {
                if (m.Error != null)
                {
                    pwMsg.AddThemeColorOverride("font_color", Style.P.Theirs);
                    pwMsg.Text = m.Error.Message;
                    return true;
                }
                account = m.Account;
                old.Text = pw.Text = again.Text = "";
                if (must)
                {
                    w.QueueFree();
                    Notice("Password changed.");
                    Send(new ClientMessage { ListGames = new ListGames() });
                }
                else
                {
                    pwMsg.AddThemeColorOverride("font_color", Style.P.Accent);
                    pwMsg.Text = "Password changed. Your other devices have been logged out.";
                }
                return true;
            });
            if (sent == 0)
            {
                pwMsg.Text = "Not connected.";
            }
        }
        again.TextSubmitted += _ => ChangePassword();
        box.AddChild(Style.Primary("Change password", ChangePassword));
        box.AddChild(pwMsg);

        if (!must)
        {
            box.AddChild(Heading("Email for password resets"));
            emailStatus = Wrapped("", 13, Style.P.Muted);
            box.AddChild(emailStatus);
            var email = Field(box, "Email address");
            var epw = Field(box, "Current password", true);
            var eMsg = Wrapped("", 14, Style.P.Theirs);
            void SetEmail(bool remove)
            {
                var addr = remove ? "" : email.Text.Trim();
                if (!remove && addr == "")
                {
                    eMsg.Text = "Enter an email address.";
                    return;
                }
                eMsg.AddThemeColorOverride("font_color", Style.P.Accent);
                eMsg.Text = remove ? "" : "Sending…";
                var sent = Request(new ClientMessage { SetEmail = new SetEmail { Email = addr, Password = epw.Text } }, m =>
                {
                    if (m.Error != null)
                    {
                        eMsg.AddThemeColorOverride("font_color", Style.P.Theirs);
                        eMsg.Text = m.Error.Message;
                        return true;
                    }
                    account = m.Account;
                    email.Text = epw.Text = "";
                    RefreshAccount();
                    eMsg.Text = remove ? "Address removed." :
                        account.PendingEmail != "" ? $"Check {account.PendingEmail} for a link to confirm it." : "Saved.";
                    return true;
                });
                if (sent == 0)
                {
                    eMsg.Text = "Not connected.";
                }
            }
            epw.TextSubmitted += _ => SetEmail(false);
            var row = new HBoxContainer();
            row.AddThemeConstantOverride("separation", 10);
            row.AddChild(Style.Primary("Save", () => SetEmail(false)));
            emailRemove = Style.Button("Remove address", () => SetEmail(true));
            row.AddChild(emailRemove);
            box.AddChild(row);
            box.AddChild(eMsg);
            foreach (var c in new Control[] { email, epw, row })
            {
                c.Visible = account.EmailEnabled;
            }
        }
        RefreshAccount();
        w.PopupCentered();
        old.GrabFocus();
    }

    // RefreshAccount shows the latest account settings in the open window.
    void RefreshAccount()
    {
        if (accountWindow == null || emailStatus == null || !IsInstanceValid(emailStatus))
        {
            return;
        }
        var a = account;
        string status;
        if (!a.EmailEnabled)
        {
            status = "This server can't send email, so whoever runs it resets forgotten passwords.";
        }
        else if (a.Email != "")
        {
            status = $"Reset links go to {a.Email}.";
        }
        else
        {
            status = "No address yet. Without one, only whoever runs the server can reset your password.";
        }
        if (a.EmailEnabled && a.PendingEmail != "")
        {
            status += $" We sent a link to {a.PendingEmail}: open it within 48 hours to start using that address.";
        }
        emailStatus.Text = status;
        if (emailRemove != null && IsInstanceValid(emailRemove))
        {
            emailRemove.Visible = a.Email != "" || a.PendingEmail != "";
        }
    }

    // ShowForgot asks for a reset link, which opens in a web browser.
    void ShowForgot()
    {
        ChangeServer();
        forgotWindow?.QueueFree();
        var box = Window("Forgot your password?", 420, 330, out var w);
        forgotWindow = w;
        box.AddChild(Wrapped("Enter your username or email address. If the account has a confirmed email address, " +
            "we'll send it a link to choose a new password. The link opens in your web browser; then log in here with the new password."));
        var name = Field(box, "Username or email", text: userEdit.Text);
        var msg = Wrapped("", 14, Style.P.Theirs);
        void Submit()
        {
            var sent = Request(new ClientMessage { RequestPasswordReset = new RequestPasswordReset { Username = name.Text.Trim() } }, m =>
            {
                if (m.Error != null)
                {
                    msg.AddThemeColorOverride("font_color", Style.P.Theirs);
                    msg.Text = m.Error.Message;
                }
                else
                {
                    msg.AddThemeColorOverride("font_color", Style.P.Accent);
                    msg.Text = "If that account has a confirmed email address, a link is on its way. It works for an hour.";
                }
                return true;
            });
            if (sent == 0)
            {
                msg.Text = "Not connected yet…";
            }
        }
        name.TextSubmitted += _ => Submit();
        var row = new HBoxContainer();
        row.AddThemeConstantOverride("separation", 10);
        row.AddChild(Style.Primary("Send link", Submit));
        row.AddChild(Style.Button("Close", () => w.QueueFree()));
        box.AddChild(row);
        box.AddChild(msg);
        w.PopupCentered();
        name.GrabFocus();
    }

    // ---------------------------------------------------------------
    // History

    void ShowHistory()
    {
        historyWindow?.QueueFree();
        historyBox = Window("History", 620, 560, out var w);
        historyWindow = w;
        historyGames.Clear();
        historyRecords = new();
        historyTotal = null;
        historyNext = "";
        historyLoaded = false;
        RenderHistory();
        Send(new ClientMessage { GetHistory = new GetHistory() });
        w.PopupCentered();
    }

    void OnHistory(History h)
    {
        if (!historyLoaded)
        {
            historyRecords = h.Records.ToList();
            historyTotal = h.Total;
            historyLoaded = true;
        }
        historyGames.AddRange(h.Games);
        historyNext = h.Next;
        RenderHistory();
    }

    static string WinRate(Record r)
    {
        var n = r.Wins + r.Losses;
        return n == 0 ? "–" : $"{System.Math.Round(100.0 * r.Wins / n)}%";
    }

    static string PlayerLabel(PlayerInfo? p) =>
        p == null ? "Unknown" : p.Bot != "" ? Text.BotLabel(p.Bot) : p.Nickname != "" ? p.Nickname : "Unknown";

    static string ShortDate(long ms)
    {
        var d = System.DateTimeOffset.FromUnixTimeMilliseconds(ms).LocalDateTime;
        return d.ToString(d.Year == System.DateTime.Now.Year ? "MMM d" : "MMM d, yyyy");
    }

    void RenderHistory()
    {
        if (historyBox == null || !IsInstanceValid(historyBox))
        {
            return;
        }
        Clear(historyBox);
        if (!historyLoaded)
        {
            historyBox.AddChild(Style.Label("Loading…", 15, Style.P.Muted));
            return;
        }
        var t = historyTotal ?? new Record();
        if (t.Games == 0)
        {
            historyBox.AddChild(Style.Label("No finished games yet.", 16));
            return;
        }
        var total = $"{t.Games} {(t.Games == 1 ? "game" : "games")} · {t.Wins} won · {t.Losses} lost";
        if (t.Unfinished > 0)
        {
            total += $" · {t.Unfinished} unfinished";
        }
        historyBox.AddChild(Style.Label(total + $" · win rate {WinRate(t)}", 17));

        historyBox.AddChild(Heading("Record by opponent"));
        var grid = new GridContainer { Columns = 5 };
        grid.AddThemeConstantOverride("h_separation", 18);
        foreach (var h in new[] { "Opponent", "Played", "Won", "Lost", "Win rate" })
        {
            grid.AddChild(Style.Label(h, 13, Style.P.Muted));
        }
        foreach (var r in historyRecords)
        {
            var name = Style.Label(PlayerLabel(r.Opponent), 15);
            name.SizeFlagsHorizontal = SizeFlags.ExpandFill;
            grid.AddChild(name);
            foreach (var v in new[] { r.Games.ToString(), r.Wins.ToString(), r.Losses.ToString(), WinRate(r) })
            {
                grid.AddChild(new Label { Text = v, HorizontalAlignment = HorizontalAlignment.Right });
            }
        }
        historyBox.AddChild(grid);

        historyBox.AddChild(Heading("Finished games"));
        foreach (var g in historyGames)
        {
            var row = new HBoxContainer();
            row.AddThemeConstantOverride("separation", 12);
            var when = Style.Label(ShortDate(g.Updated), 14, Style.P.Muted);
            when.CustomMinimumSize = new Vector2(90, 0);
            row.AddChild(when);
            var opp = g.Seat is 0 or 1 && g.Players.Count == 2 ? g.Players[1 - g.Seat] : null;
            var name = Style.Label("vs " + PlayerLabel(opp), 15);
            name.SizeFlagsHorizontal = SizeFlags.ExpandFill;
            name.ClipText = true;
            row.AddChild(name);
            if (g.Seat is 0 or 1 && g.Scores.Count == 2)
            {
                row.AddChild(Style.Label($"{g.Scores[g.Seat]}–{g.Scores[1 - g.Seat]}", 14, Style.P.Muted));
            }
            var res = g.Winner < 0 ? "unfinished" : g.Winner == g.Seat ? "won" : g.Resigned ? "resigned" : "lost";
            var tag = Style.Label(res.ToUpperInvariant(), 12, res == "won" ? Style.P.Accent : res == "unfinished" ? Style.P.Muted : Style.P.Theirs);
            tag.CustomMinimumSize = new Vector2(84, 0);
            tag.HorizontalAlignment = HorizontalAlignment.Right;
            row.AddChild(tag);
            historyBox.AddChild(row);
        }
        if (historyNext != "")
        {
            var next = historyNext;
            Button? more = null;
            more = Style.Button("Load more", () =>
            {
                more!.Disabled = true;
                Send(new ClientMessage { GetHistory = new GetHistory { After = next } });
            });
            historyBox.AddChild(more);
        }
    }
}
