using System.Collections.Generic;
using System.Linq;
using Cribbage.Protocol;
using Cribbage.Ui;
using Godot;

namespace Cribbage;

// Account settings, account deletion and game history.
public partial class Main
{
    // Handlers for replies to particular requests, by request id. A handler
    // returns true if it dealt with the reply completely.
    readonly Dictionary<uint, System.Func<ServerMessage, bool>> replies = new();
    Account account = new();

    Window? accountWindow, historyWindow;
    Label? emailStatus, deleteStatus, passkeyStatus;
    Control? deleteUser, passkeyBox;
    // A DeleteAccount that deletes at once is waiting for its reply. The
    // server closes the connection right after AccountDeleted, which can
    // then be lost; a session that no longer resumes confirms the deletion.
    bool deleting;

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
        foreach (var w in new[] { accountWindow, historyWindow })
        {
            w?.QueueFree();
        }
        accountWindow = historyWindow = null;
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

    static LineEdit Field(VBoxContainer box, string label, string text = "")
    {
        box.AddChild(Style.Label(label, 14, Style.P.Muted));
        var e = new LineEdit { Text = text, MaxLength = 254 };
        box.AddChild(e);
        return e;
    }

    // ---------------------------------------------------------------
    // Account: email, passkeys, other devices, deletion

    void ShowAccount()
    {
        accountWindow?.QueueFree();
        var box = Window("Account", 460, 700, out var w);
        accountWindow = w;
        w.TreeExiting += () => { if (accountWindow == w) accountWindow = null; };

        box.AddChild(Heading("Email"));
        emailStatus = Wrapped("", 14);
        box.AddChild(emailStatus);
        var email = Field(box, "New email address");
        var eMsg = Wrapped("", 14, Style.P.Theirs);
        void SetEmail()
        {
            var addr = email.Text.Trim();
            if (addr == "")
            {
                eMsg.AddThemeColorOverride("font_color", Style.P.Theirs);
                eMsg.Text = "Enter an email address.";
                return;
            }
            eMsg.AddThemeColorOverride("font_color", Style.P.Accent);
            eMsg.Text = "Sending…";
            var sent = Request(new ClientMessage { SetEmail = new SetEmail { Email = addr } }, m =>
            {
                if (m.Error != null)
                {
                    eMsg.AddThemeColorOverride("font_color", Style.P.Theirs);
                    eMsg.Text = m.Error.Message;
                    return true;
                }
                account = m.Account;
                email.Text = "";
                eMsg.Text = "";
                RefreshAccount();
                return true;
            });
            if (sent == 0)
            {
                eMsg.AddThemeColorOverride("font_color", Style.P.Theirs);
                eMsg.Text = "Not connected.";
            }
        }
        email.TextSubmitted += _ => SetEmail();
        var row = new HBoxContainer();
        row.AddThemeConstantOverride("separation", 10);
        row.AddChild(Style.Primary("Change email", SetEmail));
        box.AddChild(row);
        box.AddChild(eMsg);

        passkeyStatus = Wrapped("", 14);
        passkeyBox = new VBoxContainer();
        passkeyBox.AddThemeConstantOverride("separation", 8);
        passkeyBox.AddChild(Heading("Passkeys"));
        passkeyBox.AddChild(passkeyStatus);
        box.AddChild(passkeyBox);

        box.AddChild(Heading("Other devices"));
        box.AddChild(Wrapped("Sign out everywhere else you're signed in, such as other computers, phones and browsers.", 14, Style.P.Muted));
        var oMsg = Wrapped("", 14, Style.P.Theirs);
        void SignOutOthers()
        {
            var sent = Request(new ClientMessage { SignOutOthers = new SignOutOthers() }, m =>
            {
                oMsg.AddThemeColorOverride("font_color", m.Error != null ? Style.P.Theirs : Style.P.Accent);
                oMsg.Text = m.Error?.Message ?? "Your other devices are signed out.";
                return true;
            });
            if (sent == 0)
            {
                oMsg.AddThemeColorOverride("font_color", Style.P.Theirs);
                oMsg.Text = "Not connected.";
            }
        }
        var orow = new HBoxContainer();
        orow.AddChild(Style.Button("Sign out other devices", SignOutOthers));
        box.AddChild(orow);
        box.AddChild(oMsg);

        AddDeleteAccount(box);
        RefreshAccount();
        w.PopupCentered();
    }

    // DeleteByEmail reports whether deleting the account takes a confirmation
    // link mailed to its address, rather than typing the username (a bot,
    // which has no address).
    bool DeleteByEmail => account.Email != "";

    void AddDeleteAccount(VBoxContainer box)
    {
        box.AddChild(Heading("Delete account"));
        deleteStatus = Wrapped("", 14, Style.P.Muted);
        box.AddChild(deleteStatus);
        // The username field, with its label, shows only without email.
        var userBox = new VBoxContainer();
        userBox.AddThemeConstantOverride("separation", 8);
        var name = Field(userBox, "Type your username to confirm");
        box.AddChild(userBox);
        deleteUser = userBox;
        var dMsg = Wrapped("", 14, Style.P.Theirs);
        void Delete()
        {
            dMsg.AddThemeColorOverride("font_color", Style.P.Theirs);
            dMsg.Text = "";
            var byEmail = DeleteByEmail;
            var sent = Request(new ClientMessage
            {
                DeleteAccount = new DeleteAccount { Username = byEmail ? "" : name.Text.Trim() },
            }, m =>
            {
                deleting = false;
                if (m.Error != null)
                {
                    dMsg.AddThemeColorOverride("font_color", Style.P.Theirs);
                    dMsg.Text = m.Error.Message;
                    return true;
                }
                name.Text = "";
                if (m.Ack != null)
                {
                    dMsg.AddThemeColorOverride("font_color", Style.P.Accent);
                    dMsg.Text = $"We've emailed you a link to confirm, at {account.Email}. It works for an hour.";
                    return true;
                }
                if (m.AccountDeleted != null)
                {
                    AccountDeleted();
                    return true;
                }
                return false;
            });
            if (sent == 0)
            {
                dMsg.Text = "Not connected.";
            }
            else
            {
                deleting = !byEmail;
            }
        }
        name.TextSubmitted += _ => Delete();
        var row = new HBoxContainer();
        row.AddChild(Style.Danger("Delete account", Delete));
        box.AddChild(row);
        box.AddChild(dMsg);
    }

    // AccountDeleted forgets the session of a deleted account and returns
    // to the sign-in screen. The server closes the connection; the
    // reconnect comes up unauthenticated, since the token is gone.
    void AccountDeleted()
    {
        deleting = false;
        settings.Token = "";
        me = null;
        CloseAccountWindows();
        games.Clear();
        invites.Clear();
        selected = null;
        LoginMessage("Your account was deleted.", ok: true);
        ShowLogin(true);
        RenderAll();
    }

    // RefreshAccount shows the latest account settings in the open window.
    void RefreshAccount()
    {
        if (accountWindow == null || emailStatus == null || !IsInstanceValid(emailStatus))
        {
            return;
        }
        var a = account;
        var status = a.Email != ""
            ? $"Sign-in codes go to {a.Email}."
            : "This account has no email address.";
        if (a.PendingEmail != "")
        {
            status += a.PendingNeedsCurrent && a.Email != ""
                ? $"\n\nTo change it to {a.PendingEmail}, first open the link we sent to {a.Email}, your current address. " +
                  $"Then we'll send a link to {a.PendingEmail}; open that one too to finish."
                : $"\n\nAlmost done: open the link we sent to {a.PendingEmail} to start using it.";
        }
        emailStatus.Text = status;
        if (passkeyBox != null && IsInstanceValid(passkeyBox) && passkeyStatus != null)
        {
            passkeyBox.Visible = a.PasskeysEnabled;
            var n = a.Passkeys.Count;
            passkeyStatus.Text = $"Passkeys: {n} (manage them in the web version)";
        }
        if (deleteStatus != null && IsInstanceValid(deleteStatus) && deleteUser != null && IsInstanceValid(deleteUser))
        {
            deleteStatus.Text = DeleteByEmail
                ? $"We'll email a link to {a.Email}; your account is deleted when you open it and confirm. " +
                  "Your unfinished games end, and this cannot be undone."
                : "Your account is deleted at once. This cannot be undone.";
            deleteUser.Visible = !DeleteByEmail;
        }
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
