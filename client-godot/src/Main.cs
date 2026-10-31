using System.Collections.Generic;
using System.Linq;
using System.Text.Json;
using Cribbage.Net;
using Cribbage.Protocol;
using Cribbage.Ui;
using Godot;

namespace Cribbage;

// Main is the whole client: it owns the connection and the games, and shows
// the login form, or the lobby beside the selected game's table.
public partial class Main : Control
{
    Options opts = new();
    Settings settings = null!;
    Connection? conn;
    AuthOk? me;

    readonly Dictionary<string, GameState> games = new();
    List<GameSummary> openGames = new(), recentGames = new();
    string? selected;
    readonly HashSet<uint> picked = new();
    bool awaiting; // an action was sent and its update has not come back
    List<string> bots = new() { "heuristic", "random" };
    string statusText = "";
    bool loginShown;

    // Everything below is rebuilt when the theme changes.
    Control ui = null!;

    // Login
    Control loginView = null!;
    LineEdit serverEdit = null!, userEdit = null!, passEdit = null!, nickEdit = null!;
    Label loginError = null!;

    // Lobby
    Control lobbyView = null!;
    Label status = null!, who = null!, notice = null!;
    VBoxContainer botPicks = null!, myList = null!, openList = null!, recentList = null!, inviteList = null!;
    Label invitesHeading = null!;
    LineEdit inviteUser = null!, codeEdit = null!;
    List<Invite> invites = new();
    Button muteButton = null!;

    // Table
    Control tableView = null!, emptyView = null!, fxLayer = null!;
    Label oppName = null!, oppScore = null!, meName = null!, meScore = null!, prompt = null!, cribLabel = null!,
        countLabel = null!, phaseLabel = null!;
    HBoxContainer oppHand = null!, cribCards = null!, sequence = null!, earlier = null!, myHand = null!, starterSlot = null!;
    VBoxContainer shows = null!;
    Board board = null!;
    Button discardButton = null!, resignButton = null!;
    RichTextLabel log = null!;
    string? logGame;
    int logShown;
    Control bannerLayer = null!;
    string? bannerFor;

    public override void _Ready()
    {
        opts = Options.Parse(OS.GetCmdlineUserArgs());
        settings = new Settings(opts.Profile);
        if (opts.Server != null)
        {
            settings.Server = opts.Server;
        }
        if (opts.Theme != null)
        {
            settings.Theme = opts.Theme;
        }
        Style.Use(settings.Theme);
        Sfx.Init(this);
        UpdateSound();
        GetWindow().Title = "Cribbage";
        BuildUi();
        ShowLogin(false);
        Connect();
        FetchBots();
        if (opts.Screenshot != "")
        {
            GetTree().CreateTimer(opts.After).Timeout += SaveScreenshot;
        }
    }

    public override void _Process(double delta)
    {
        conn?.Poll();
        PollWaiters();
        if (opts.Autoplay)
        {
            AutoplayTick();
        }
    }

    // Rapid applies updates at once; autoplay plays rapidly unless asked
    // to animate.
    bool Rapid => settings.Rapid || opts.Autoplay && !opts.Animate;

    void UpdateSound() => Sfx.Enabled = !settings.Muted && !Rapid;

    // ---------------------------------------------------------------
    // Connection

    void Connect()
    {
        conn?.Close();
        SetStatus("connecting…");
        var url = settings.Server;
        Fetch(HttpBase(url) + "/api/config", body =>
        {
            var ice = new List<string>();
            try
            {
                using var doc = JsonDocument.Parse(body);
                foreach (var s in doc.RootElement.GetProperty("iceServers").EnumerateArray())
                {
                    foreach (var u in s.GetProperty("urls").EnumerateArray())
                    {
                        ice.Add(u.GetString() ?? "");
                    }
                }
            }
            catch (JsonException) { }
            catch (KeyNotFoundException) { }
            catch (System.InvalidOperationException) { }
            if (url != settings.Server)
            {
                return; // changed while fetching
            }
            conn = new Connection(url, ice);
            conn.Opened += OnOpen;
            conn.Closed += () => SetStatus("reconnecting…");
            conn.Received += OnMessage;
            conn.Connect();
        });
    }

    static string HttpBase(string ws)
    {
        var u = ws.StartsWith("wss:") ? "https:" + ws[4..] : ws.StartsWith("ws:") ? "http:" + ws[3..] : ws;
        var slash = u.LastIndexOf('/');
        return slash > "https://".Length ? u[..slash] : u;
    }

    // Fetch GETs url and passes the body (empty on failure) to done.
    void Fetch(string url, System.Action<string> done)
    {
        var req = new HttpRequest { Timeout = 4 };
        AddChild(req);
        req.RequestCompleted += (result, code, headers, body) =>
        {
            req.QueueFree();
            done(result == (long)HttpRequest.Result.Success && code == 200 ? System.Text.Encoding.UTF8.GetString(body) : "");
        };
        if (req.Request(url) != Godot.Error.Ok)
        {
            req.QueueFree();
            done("");
        }
    }

    // FetchBots shows only the bots this server offers (nn and expert
    // need files on the server).
    void FetchBots()
    {
        Fetch(HttpBase(settings.Server) + "/api/bots", body =>
        {
            try
            {
                var names = JsonSerializer.Deserialize<List<string>>(body);
                if (names != null)
                {
                    bots = names;
                }
            }
            catch (JsonException) { }
            RenderBotPicks();
        });
    }

    void OnOpen()
    {
        SetStatus("online");
        me = null; // each connection authenticates afresh
        var token = settings.Token;
        if (token != "")
        {
            Send(new ClientMessage { Resume = new Resume { Token = token } });
        }
        else
        {
            ShowLogin(true);
        }
    }

    uint Send(ClientMessage m) => conn?.Send(m) ?? 0;

    void OnMessage(ServerMessage m)
    {
        switch (m.BodyCase)
        {
            case ServerMessage.BodyOneofCase.AuthOk:
                me = m.AuthOk;
                settings.Token = me.Token;
                settings.LastUser = me.Username;
                loginError.Text = "";
                ShowLogin(false);
                Send(new ClientMessage { ListGames = new ListGames() });
                RenderAll();
                break;
            case ServerMessage.BodyOneofCase.Error:
                OnError(m.Error);
                break;
            case ServerMessage.BodyOneofCase.GameUpdate:
                OnUpdate(m.GameUpdate);
                break;
            case ServerMessage.BodyOneofCase.Invites:
                SetInvites(m.Invites.Invites_, true);
                break;
            case ServerMessage.BodyOneofCase.GameList:
                SetInvites(m.GameList.Invites, false);
                foreach (var g in m.GameList.Mine)
                {
                    if (!games.TryGetValue(g.GameId, out var gs))
                    {
                        games[g.GameId] = new GameState(g);
                    }
                    else if (gs.Update == null)
                    {
                        gs.Game = g;
                    }
                }
                openGames = m.GameList.Open.ToList();
                recentGames = m.GameList.Recent.ToList();
                RenderLobby();
                break;
            case ServerMessage.BodyOneofCase.MatchQueued:
                Notice("Waiting for an opponent…");
                break;
        }
        AutoplayMessage(m);
    }

    void OnError(Protocol.Error e)
    {
        if (me == null)
        {
            if (e.Code is ErrorCode.InvalidCredentials or ErrorCode.Unauthenticated && settings.Token != "")
            {
                settings.Token = ""; // stale session; ask for a password
                ShowLogin(true);
                return;
            }
            loginError.Text = e.Message;
            ShowLogin(true);
            return;
        }
        awaiting = false;
        Notice(e.Message);
        if (selected != null && games.TryGetValue(selected, out var gs) && gs.Update != null && !gs.V.Busy)
        {
            gs.V.Sync(gs.Update); // undo the optimistic lock on the hand
        }
        RenderTable();
    }

    void OnUpdate(GameUpdate u)
    {
        var id = u.Game.GameId;
        var known = games.TryGetValue(id, out var gs) && gs.Update != null;
        if (gs == null)
        {
            gs = games[id] = new GameState(u.Game);
        }
        gs.Received(u);
        if (id == selected)
        {
            awaiting = false;
        }
        if (gs.Finished)
        {
            if (id != selected)
            {
                Notice(Text.GameOver(u.Game));
            }
            Send(new ClientMessage { ListGames = new ListGames() });
        }
        if (selected == null || (!known && !gs.Finished && !u.Full))
        {
            Select(id);
        }
        if (u.Game.Status == GameStatus.Active && notice.Text.StartsWith("Waiting for an opponent"))
        {
            notice.Text = "";
        }
        UpdateTitle();
        RenderLobby();
        Enqueue(u);
    }

    void Select(string id)
    {
        if (selected != id)
        {
            SkipAnimation();
            picked.Clear();
            awaiting = false;
        }
        selected = id;
        if (games.TryGetValue(id, out var gs) && gs.Update == null)
        {
            Send(new ClientMessage { SyncGame = new SyncGame { GameId = id } });
        }
        RenderLobby();
        RenderTable();
    }

    void Act(Action a)
    {
        if (selected == null)
        {
            return;
        }
        Send(new ClientMessage { Act = new Act { GameId = selected, Action = a } });
        EndTails();
        picked.Clear();
        awaiting = true;
        var v = games[selected].V;
        v.Legal.Clear();
        v.MustAct = false;
        RenderTable();
    }

    void CreateGame(string bot) => Send(new ClientMessage { CreateGame = new CreateGame { Bot = bot } });

    // ---------------------------------------------------------------
    // Layout

    void BuildUi()
    {
        Theme = Style.Build();
        ui = new Control();
        AddChild(ui);
        ui.SetAnchorsAndOffsetsPreset(LayoutPreset.FullRect);
        var bg = new ColorRect { Color = Style.P.Felt };
        ui.AddChild(bg);
        bg.SetAnchorsAndOffsetsPreset(LayoutPreset.FullRect);

        var root = new HBoxContainer();
        root.AddThemeConstantOverride("separation", 0);
        ui.AddChild(root);
        root.SetAnchorsAndOffsetsPreset(LayoutPreset.FullRect);
        lobbyView = BuildLobby();
        root.AddChild(lobbyView);

        var main = new MarginContainer { SizeFlagsHorizontal = SizeFlags.ExpandFill };
        foreach (var side in new[] { "left", "right", "top", "bottom" })
        {
            main.AddThemeConstantOverride("margin_" + side, 16);
        }
        root.AddChild(main);
        var split = new HBoxContainer();
        split.AddThemeConstantOverride("separation", 16);
        main.AddChild(split);

        // The table, with the game-over banner over it.
        var stack = new Control { SizeFlagsHorizontal = SizeFlags.ExpandFill };
        split.AddChild(stack);
        tableView = BuildTable();
        stack.AddChild(tableView);
        tableView.SetAnchorsAndOffsetsPreset(LayoutPreset.FullRect);
        stack.GuiInput += e =>
        {
            if (e is InputEventMouseButton { ButtonIndex: MouseButton.Left, Pressed: true })
            {
                SkipAnimation();
            }
        };
        emptyView = new CenterContainer();
        emptyView.AddChild(Style.Label("Pick an opponent to start a game.", 20, Style.P.Muted));
        stack.AddChild(emptyView);
        emptyView.SetAnchorsAndOffsetsPreset(LayoutPreset.FullRect);
        bannerLayer = new CenterContainer { MouseFilter = MouseFilterEnum.Ignore };
        stack.AddChild(bannerLayer);
        bannerLayer.SetAnchorsAndOffsetsPreset(LayoutPreset.FullRect);
        split.AddChild(BuildLog());

        fxLayer = new Control { MouseFilter = MouseFilterEnum.Ignore };
        ui.AddChild(fxLayer);
        fxLayer.SetAnchorsAndOffsetsPreset(LayoutPreset.FullRect);

        loginView = BuildLogin();
        ui.AddChild(loginView);
        loginView.SetAnchorsAndOffsetsPreset(LayoutPreset.FullRect);
        loginView.Visible = loginShown;
        SetStatus(statusText);
        RenderBotPicks();
    }

    // Restyle rebuilds the interface in the current palette.
    void Restyle()
    {
        RemoveChild(ui);
        ui.QueueFree();
        bannerFor = null;
        logGame = null;
        BuildUi();
        RenderAll();
    }

    Control BuildLogin()
    {
        var shade = new ColorRect { Color = Style.P.Page with { A = 0.94f } };
        var center = new CenterContainer();
        shade.AddChild(center);
        center.SetAnchorsAndOffsetsPreset(LayoutPreset.FullRect);
        var panel = new PanelContainer { CustomMinimumSize = new Vector2(380, 0) };
        center.AddChild(panel);
        var box = new VBoxContainer();
        box.AddThemeConstantOverride("separation", 10);
        panel.AddChild(box);
        box.AddChild(Style.Label("♠ Cribbage", 32, Style.P.Accent));
        box.AddChild(Style.Label("Log in or create an account.", 15, Style.P.Muted));

        LineEdit Field(string label, string placeholder)
        {
            box.AddChild(Style.Label(label, 14, Style.P.Muted));
            var e = new LineEdit { PlaceholderText = placeholder };
            box.AddChild(e);
            return e;
        }
        serverEdit = Field("Server", "ws://host:8080/rtc");
        serverEdit.Text = settings.Server;
        serverEdit.TextSubmitted += _ => ChangeServer();
        serverEdit.FocusExited += ChangeServer;
        userEdit = Field("Username", "");
        userEdit.Text = settings.LastUser;
        passEdit = Field("Password", "");
        passEdit.Secret = true;
        passEdit.TextSubmitted += _ => Login(false);
        nickEdit = Field("Nickname", "for new accounts (optional)");

        var row = new HBoxContainer();
        row.AddThemeConstantOverride("separation", 10);
        box.AddChild(row);
        row.AddChild(Style.Primary("Log in", () => Login(false)));
        row.AddChild(Style.Button("Register", () => Login(true)));
        loginError = Style.Label("", 14, Style.P.Theirs);
        loginError.AutowrapMode = TextServer.AutowrapMode.WordSmart;
        box.AddChild(loginError);
        return shade;
    }

    void ChangeServer()
    {
        var url = serverEdit.Text.Trim();
        if (url == "" || url == settings.Server)
        {
            return;
        }
        settings.Server = url;
        Connect();
        FetchBots();
    }

    void Login(bool register)
    {
        ChangeServer();
        if (conn is not { IsOpen: true })
        {
            loginError.Text = "Not connected yet…";
            return;
        }
        loginError.Text = "";
        if (register)
        {
            Send(new ClientMessage { Register = new Register { Username = userEdit.Text, Password = passEdit.Text, Nickname = nickEdit.Text } });
        }
        else
        {
            Send(new ClientMessage { Login = new Login { Username = userEdit.Text, Password = passEdit.Text } });
        }
    }

    void ShowLogin(bool show)
    {
        loginShown = show && !opts.Autoplay;
        loginView.Visible = loginShown;
        if (loginShown)
        {
            (userEdit.Text == "" ? userEdit : passEdit).GrabFocus();
        }
    }

    Control BuildLobby()
    {
        var panel = new PanelContainer { CustomMinimumSize = new Vector2(250, 0) };
        panel.AddThemeStyleboxOverride("panel", Style.Box(Style.P.Page, 0));
        var scroll = new ScrollContainer { HorizontalScrollMode = ScrollContainer.ScrollMode.Disabled };
        panel.AddChild(scroll);
        var box = new VBoxContainer { SizeFlagsHorizontal = SizeFlags.ExpandFill };
        box.AddThemeConstantOverride("separation", 8);
        scroll.AddChild(box);

        var title = new HBoxContainer();
        title.AddChild(Style.Label("♠ Cribbage", 24, Style.P.Accent));
        title.AddChild(Style.Expand());
        muteButton = Style.Button("", ToggleMute);
        title.AddChild(muteButton);
        UpdateMuteButton();
        var gear = Style.Button("⚙", ShowSettings);
        gear.TooltipText = "Settings";
        title.AddChild(gear);
        box.AddChild(title);
        var acct = new HBoxContainer();
        who = Style.Label(me != null ? $"{me.Nickname} ({me.Username})" : "", 13, Style.P.Muted);
        who.SizeFlagsHorizontal = SizeFlags.ExpandFill;
        who.ClipText = true;
        acct.AddChild(who);
        status = Style.Label("", 13, Style.P.Muted);
        acct.AddChild(status);
        acct.AddChild(Style.Button("Log out", Logout));
        box.AddChild(acct);

        box.AddChild(Heading("New game"));
        botPicks = new VBoxContainer();
        box.AddChild(botPicks);
        var people = new HBoxContainer();
        people.AddChild(Style.Button("Find a match", () => Send(new ClientMessage { FindMatch = new FindMatch() })));
        people.AddChild(Style.Button("Open game", () => CreateGame("")));
        people.AddChild(Style.Button("↻", () => Send(new ClientMessage { ListGames = new ListGames() })));
        box.AddChild(people);
        notice = Style.Label("", 14, Style.P.Accent);
        notice.AutowrapMode = TextServer.AutowrapMode.WordSmart;
        box.AddChild(notice);

        box.AddChild(Heading("Play someone you know"));
        var inviteRow = new HBoxContainer();
        inviteUser = new LineEdit { PlaceholderText = "Their username", MaxLength = 32, SizeFlagsHorizontal = SizeFlags.ExpandFill };
        inviteUser.TextSubmitted += _ => SendInvite();
        inviteRow.AddChild(inviteUser);
        inviteRow.AddChild(Style.Button("Invite", SendInvite));
        box.AddChild(inviteRow);
        var codeButton = Style.Button("Get an invite code", () => Send(new ClientMessage { SendInvite = new SendInvite() }));
        codeButton.TooltipText = "A code to text or read to someone; it lasts 48 hours";
        box.AddChild(codeButton);
        var codeRow = new HBoxContainer();
        codeEdit = new LineEdit { PlaceholderText = "Have a code? ABC-DEF", MaxLength = 12, SizeFlagsHorizontal = SizeFlags.ExpandFill };
        codeEdit.TextSubmitted += _ => RedeemCode();
        codeRow.AddChild(codeEdit);
        codeRow.AddChild(Style.Button("Join", RedeemCode));
        box.AddChild(codeRow);
        invitesHeading = Heading("Invites");
        box.AddChild(invitesHeading);
        inviteList = new VBoxContainer();
        inviteList.AddThemeConstantOverride("separation", 6);
        box.AddChild(inviteList);

        box.AddChild(Heading("Your games"));
        myList = new VBoxContainer();
        box.AddChild(myList);
        box.AddChild(Heading("Open games"));
        openList = new VBoxContainer();
        box.AddChild(openList);
        box.AddChild(Heading("Recently finished"));
        recentList = new VBoxContainer();
        box.AddChild(recentList);
        return panel;
    }

    static Label Heading(string text) => Style.Label(text.ToUpperInvariant(), 12, Style.P.Muted);

    // ---------------------------------------------------------------
    // Invites

    void SendInvite()
    {
        var name = inviteUser.Text.Trim();
        if (name == "")
        {
            return;
        }
        Send(new ClientMessage { SendInvite = new SendInvite { Username = name } });
        inviteUser.Text = "";
    }

    void RedeemCode()
    {
        var code = codeEdit.Text.Trim();
        if (code == "")
        {
            return;
        }
        Send(new ClientMessage { RedeemCode = new RedeemCode { Code = code } });
        codeEdit.Text = "";
    }

    void Respond(Invite inv, bool accept) =>
        Send(new ClientMessage { RespondInvite = new RespondInvite { InviteId = inv.InviteId, Accept = accept } });

    void SetInvites(IEnumerable<Invite> list, bool pushed)
    {
        var known = invites.Select(i => i.InviteId).ToHashSet();
        var next = list.ToList();
        if (pushed && next.Any(i => i.Incoming && !known.Contains(i.InviteId)))
        {
            var from = next.First(i => i.Incoming && !known.Contains(i.InviteId)).From.Nickname;
            Notice($"{from} invited you to a game");
            Sfx.Play("turn");
        }
        invites = next;
        RenderInvites();
    }

    static string TimeLeft(long expires)
    {
        var left = System.DateTimeOffset.FromUnixTimeMilliseconds(expires) - System.DateTimeOffset.UtcNow;
        return left.TotalHours >= 1 ? $"{(int)left.TotalHours} h left" : $"{System.Math.Max(1, (int)System.Math.Ceiling(left.TotalMinutes))} min left";
    }

    void RenderInvites()
    {
        Clear(inviteList);
        invitesHeading.Visible = invites.Count > 0;
        foreach (var inv in invites)
        {
            var panel = new PanelContainer();
            panel.AddThemeStyleboxOverride("panel", Style.Box(Style.P.Button, 6, inv.Incoming ? Style.P.Accent : Style.P.Line));
            var col = new VBoxContainer();
            panel.AddChild(col);
            var top = new HBoxContainer();
            col.AddChild(top);
            Label text;
            if (inv.Incoming)
            {
                text = Style.Label($"{inv.From.Nickname} invited you", 14);
            }
            else if (inv.Code != "")
            {
                text = Style.Label($"Code  {inv.Code}", 16, Style.P.Accent);
                text.TooltipText = "Give this to the person you want to play";
                text.MouseFilter = MouseFilterEnum.Pass;
            }
            else
            {
                text = Style.Label($"Waiting for {inv.To.Nickname}", 14);
            }
            text.SizeFlagsHorizontal = SizeFlags.ExpandFill;
            text.ClipText = true;
            top.AddChild(text);
            top.AddChild(Style.Label(TimeLeft(inv.Expires), 12, Style.P.Muted));
            var acts = new HBoxContainer { Alignment = BoxContainer.AlignmentMode.End };
            col.AddChild(acts);
            var i = inv;
            if (inv.Incoming)
            {
                acts.AddChild(Style.Primary("Play", () => Respond(i, true)));
                acts.AddChild(Style.Button("No thanks", () => Respond(i, false)));
            }
            else
            {
                if (inv.Code != "")
                {
                    acts.AddChild(Style.Button("Copy", () => DisplayServer.ClipboardSet(i.Code)));
                }
                acts.AddChild(Style.Button("Cancel", () => Respond(i, false)));
            }
            inviteList.AddChild(panel);
        }
    }

    void ToggleMute()
    {
        settings.Muted = !settings.Muted;
        UpdateSound();
        UpdateMuteButton();
    }

    void UpdateMuteButton()
    {
        muteButton.Text = settings.Muted ? "🔇" : "🔊";
        muteButton.TooltipText = settings.Muted ? "Unmute sounds" : "Mute sounds";
    }

    void ShowSettings()
    {
        var pop = new PopupPanel();
        var box = new VBoxContainer();
        box.AddThemeConstantOverride("separation", 10);
        pop.AddChild(box);
        box.AddChild(Heading("Settings"));
        var row = new HBoxContainer();
        row.AddThemeConstantOverride("separation", 10);
        row.AddChild(Style.Label("Theme"));
        var themes = new OptionButton();
        for (var i = 0; i < Style.Palettes.Length; i++)
        {
            themes.AddItem(Style.Palettes[i].Label, i);
            if (Style.Palettes[i] == Style.P)
            {
                themes.Selected = i;
            }
        }
        themes.ItemSelected += i =>
        {
            settings.Theme = Style.Palettes[i].Name;
            Style.Use(settings.Theme);
            pop.Hide();
            Restyle();
        };
        row.AddChild(themes);
        box.AddChild(row);
        var rapid = new CheckBox { Text = "Rapid play (no animation or sound)", ButtonPressed = settings.Rapid };
        rapid.Toggled += on =>
        {
            settings.Rapid = on;
            UpdateSound();
            if (on)
            {
                SkipAnimation();
            }
        };
        box.AddChild(rapid);
        var mute = new CheckBox { Text = "Mute sounds", ButtonPressed = settings.Muted };
        mute.Toggled += on =>
        {
            settings.Muted = on;
            UpdateSound();
            UpdateMuteButton();
        };
        box.AddChild(mute);
        ui.AddChild(pop);
        pop.PopupHide += pop.QueueFree;
        pop.Popup(new Rect2I((Vector2I)(GetGlobalMousePosition() + new Vector2(-40, 24)), Vector2I.Zero));
    }

    void Logout()
    {
        settings.Token = "";
        me = null;
        games.Clear();
        invites.Clear();
        selected = null;
        Connect(); // a fresh, unauthenticated connection
        ShowLogin(true);
        RenderAll();
    }

    Control BuildTable()
    {
        var box = new VBoxContainer();
        box.AddThemeConstantOverride("separation", 12);

        var top = new HBoxContainer();
        top.AddThemeConstantOverride("separation", 16);
        box.AddChild(top);
        top.AddChild(Plate(out oppName, out oppScore, Style.P.Theirs));
        oppHand = Fan(-30);
        top.AddChild(oppHand);
        top.AddChild(Style.Expand());
        phaseLabel = Style.Label("", 14, Style.P.Muted);
        top.AddChild(phaseLabel);

        var boardRow = new CenterContainer();
        board = new Board();
        boardRow.AddChild(board);
        box.AddChild(boardRow);

        var mid = new HBoxContainer { SizeFlagsVertical = SizeFlags.ExpandFill };
        mid.AddThemeConstantOverride("separation", 20);
        box.AddChild(mid);
        var crib = new VBoxContainer { CustomMinimumSize = new Vector2(130, 0) };
        cribLabel = Style.Label("Crib", 14, Style.P.Muted);
        crib.AddChild(cribLabel);
        cribCards = Fan(-30);
        crib.AddChild(cribCards);
        mid.AddChild(crib);

        var center = new VBoxContainer { SizeFlagsHorizontal = SizeFlags.ExpandFill };
        center.AddThemeConstantOverride("separation", 6);
        countLabel = Style.Label("", 22, Style.P.Accent);
        center.AddChild(countLabel);
        sequence = Fan(-28);
        center.AddChild(sequence);
        earlier = Fan(-18);
        center.AddChild(earlier);
        shows = new VBoxContainer();
        shows.AddThemeConstantOverride("separation", 6);
        center.AddChild(shows);
        mid.AddChild(center);

        var deck = new VBoxContainer();
        deck.AddChild(Style.Label("Starter", 14, Style.P.Muted));
        starterSlot = Fan(-74);
        deck.AddChild(starterSlot);
        mid.AddChild(deck);

        var act = new HBoxContainer();
        act.AddThemeConstantOverride("separation", 12);
        prompt = Style.Label("", 18);
        prompt.SizeFlagsHorizontal = SizeFlags.ExpandFill;
        act.AddChild(prompt);
        discardButton = Style.Primary("Discard", Discard);
        act.AddChild(discardButton);
        box.AddChild(act);

        myHand = Fan(10);
        myHand.CustomMinimumSize = new Vector2(0, CardFace.Normal.Y + 20);
        myHand.Alignment = BoxContainer.AlignmentMode.Center;
        box.AddChild(myHand);

        var bottom = new HBoxContainer();
        bottom.AddChild(Plate(out meName, out meScore, Style.P.Mine));
        bottom.AddChild(Style.Expand());
        resignButton = Style.Button("Resign", ConfirmResign);
        bottom.AddChild(resignButton);
        box.AddChild(bottom);
        return box;
    }

    static HBoxContainer Fan(int separation)
    {
        var h = new HBoxContainer();
        h.AddThemeConstantOverride("separation", separation);
        return h;
    }

    static Control Plate(out Label name, out Label score, Color color)
    {
        var p = new PanelContainer();
        p.AddThemeStyleboxOverride("panel", Style.Box(Style.P.Panel with { A = 0.6f }, 8, color));
        var h = new HBoxContainer();
        h.AddThemeConstantOverride("separation", 14);
        p.AddChild(h);
        name = Style.Label("", 16);
        h.AddChild(name);
        score = Style.Label("0", 22, color);
        h.AddChild(score);
        return p;
    }

    Control BuildLog()
    {
        var panel = new PanelContainer { CustomMinimumSize = new Vector2(240, 0) };
        var box = new VBoxContainer();
        panel.AddChild(box);
        box.AddChild(Heading("Game log"));
        log = new RichTextLabel
        {
            BbcodeEnabled = true,
            ScrollFollowing = true,
            SizeFlagsVertical = SizeFlags.ExpandFill,
            SelectionEnabled = true,
        };
        log.AddThemeFontSizeOverride("normal_font_size", 14);
        box.AddChild(log);
        return panel;
    }

    // ---------------------------------------------------------------
    // Rendering

    void RenderAll()
    {
        if (who != null)
        {
            who.Text = me != null ? $"{me.Nickname} ({me.Username})" : "";
        }
        RenderLobby();
        RenderInvites();
        RenderTable();
        UpdateTitle();
    }

    void SetStatus(string text)
    {
        statusText = text;
        status.Text = text == "online" ? "● online" : text;
        status.AddThemeColorOverride("font_color", text == "online" ? Style.P.Mine : Style.P.Muted);
    }

    void Notice(string text) => notice.Text = text;

    void UpdateTitle()
    {
        var turn = games.Values.Any(g => g.Game.YourTurn && g.Game.Status == GameStatus.Active);
        GetWindow().Title = turn ? "● Your turn — Cribbage" : "Cribbage";
    }

    void RenderBotPicks()
    {
        Clear(botPicks);
        foreach (var (name, label, blurb) in Text.Bots)
        {
            if (!bots.Contains(name))
            {
                continue;
            }
            var b = Style.Button(label, () => CreateGame(name));
            b.TooltipText = blurb;
            b.Alignment = HorizontalAlignment.Left;
            botPicks.AddChild(b);
        }
    }

    void RenderLobby()
    {
        Clear(myList);
        foreach (var (id, gs) in games)
        {
            var g = gs.Game;
            if (g.Status == GameStatus.Finished)
            {
                continue;
            }
            var tag = g.Status == GameStatus.Waiting ? "open" : g.YourTurn ? "your turn" : "";
            myList.AddChild(GameItem(g, tag, () => Select(id)));
        }
        if (myList.GetChildCount() == 0)
        {
            myList.AddChild(Style.Label("No games in progress", 14, Style.P.Muted));
        }
        Clear(openList);
        foreach (var g in openGames)
        {
            var id = g.GameId;
            openList.AddChild(Item($"{g.Players[0].Nickname}", "join",
                () => Send(new ClientMessage { JoinGame = new JoinGame { GameId = id } }), false));
        }
        if (openList.GetChildCount() == 0)
        {
            openList.AddChild(Style.Label("Nobody is waiting", 14, Style.P.Muted));
        }
        Clear(recentList);
        foreach (var g in recentGames.Take(10))
        {
            var id = g.GameId;
            var res = g.Winner == g.Seat ? "won" : g.Winner < 0 ? "cancelled" : "lost";
            recentList.AddChild(GameItem(g, res, () => { if (games.ContainsKey(id)) Select(id); }));
        }
        if (recentList.GetChildCount() == 0)
        {
            recentList.AddChild(Style.Label("None yet", 14, Style.P.Muted));
        }
    }

    Control GameItem(GameSummary g, string tag, System.Action onClick)
    {
        var score = g.Status != GameStatus.Waiting && g.Scores.Count == 2 ? $"  {g.Scores[g.Seat]}–{g.Scores[1 - g.Seat]}" : "";
        return Item($"vs {Text.Opponent(g)}{score}", tag, onClick, g.GameId == selected);
    }

    static Button Item(string text, string tag, System.Action onClick, bool current)
    {
        var b = Style.Button(tag == "" ? text : $"{text}  · {tag}", onClick);
        b.Alignment = HorizontalAlignment.Left;
        b.ClipText = true;
        b.AddThemeFontSizeOverride("font_size", 14);
        if (current)
        {
            b.AddThemeStyleboxOverride("normal", Style.Box(Style.P.ButtonHover, 6, Style.P.Accent));
        }
        else if (tag == "your turn" || tag == "join")
        {
            b.AddThemeColorOverride("font_color", Style.P.Accent);
        }
        return b;
    }

    static void Clear(Node n)
    {
        foreach (var c in n.GetChildren())
        {
            n.RemoveChild(c);
            c.QueueFree();
        }
    }

    // RenderTable draws the selected game's view; with animate, what is new
    // since the last render arrives with an animation.
    void RenderTable(bool animate = false)
    {
        var gs = selected != null && games.TryGetValue(selected, out var s) ? s : null;
        tableView.Visible = gs != null;
        emptyView.Visible = gs == null && me != null;
        RenderLog(gs);
        if (gs == null)
        {
            Clear(bannerLayer);
            bannerFor = null;
            return;
        }
        var v = gs.V;
        var g = gs.Game;
        var meSeat = gs.Me;
        var opp = 1 - meSeat;
        var finished = g.Status == GameStatus.Finished;
        var oppPlayer = opp < g.Players.Count ? g.Players[opp] : null;
        var theirName = Text.Opponent(g);
        oppName.Text = oppPlayer is { Bot: not "" } ? Text.BotLabel(oppPlayer.Bot) :
            oppPlayer is { UserId: not 0 } ? $"{theirName}  ·  {(oppPlayer.Connected ? "online" : "away")}" : theirName;
        meName.Text = me != null ? $"{me.Nickname}  ·  you" : "You";
        oppScore.Text = v.Scores[opp].ToString();
        meScore.Text = v.Scores[meSeat].ToString();
        board.Set(g.GameId, meSeat, v.Scores, v.Trail, animate);
        phaseLabel.Text = finished ? "Game over" : g.Status == GameStatus.Waiting ? "Waiting" :
            v.HandNumber > 0 ? $"Hand {v.HandNumber} · {(v.Showing ? "The show" : v.Phase == Phase.Pegging ? "Pegging" : "Discard")}" : "";
        resignButton.Text = g.Status == GameStatus.Waiting ? "Cancel game" : "Resign";
        resignButton.Visible = !finished;

        // Their hand: backs, or their cards once shown.
        Clear(oppHand);
        if (v.OppReveal != null)
        {
            foreach (var c in v.OppReveal)
            {
                oppHand.AddChild(new CardFace(c, CardFace.Small));
            }
        }
        else
        {
            for (var i = 0; i < v.OppHandSize; i++)
            {
                var f = new CardFace(null, CardFace.Small);
                oppHand.AddChild(f);
                if (animate && v.FreshOpp)
                {
                    f.Enter(CardFace.Entry.DealInOpp, i * .06);
                }
            }
        }

        // The deck, with the starter on top.
        Clear(starterSlot);
        starterSlot.AddChild(new CardFace(null, CardFace.Normal));
        var top = new CardFace(v.Starter, CardFace.Normal);
        starterSlot.AddChild(top);
        if (animate && v.FreshStarter)
        {
            top.Enter(CardFace.Entry.FlipIn);
        }

        // The crib: my discards face up, theirs face down, until it is shown.
        var cribMine = v.Dealer == meSeat;
        cribLabel.Text = v.Dealer < 0 ? "Crib" : cribMine ? "Your crib" : $"{theirName}'s crib";
        cribLabel.AddThemeColorOverride("font_color", v.Dealer < 0 ? Style.P.Muted : cribMine ? Style.P.Mine : Style.P.Theirs);
        Clear(cribCards);
        if (v.CribOpen != null)
        {
            foreach (var c in v.CribOpen)
            {
                cribCards.AddChild(new CardFace(c, CardFace.Small));
            }
        }
        else
        {
            var cards = new List<uint?>();
            if (v.OppDiscarded)
            {
                cards.Add(null);
                cards.Add(null);
            }
            cards.AddRange(v.MyDiscards.Select(c => (uint?)c));
            for (var i = 0; i < cards.Count; i++)
            {
                var f = new CardFace(cards[i], CardFace.Small);
                cribCards.AddChild(f);
                if (animate && v.FreshCrib && i >= cards.Count - 2)
                {
                    f.Enter(cards[i] == null ? CardFace.Entry.DealInOpp : CardFace.Entry.DealIn);
                }
            }
        }

        // Middle: pegging, or the show.
        var showing = v.Shows.Count > 0 && (v.Showing || v.Phase == Phase.Discard || finished);
        countLabel.Text = showing ? (v.Showing ? "The show" : "Last hand") : v.Phase == Phase.Pegging ? $"Count {v.Count}" : "";
        if (animate && v.Hot)
        {
            countLabel.PivotOffset = countLabel.Size / 2;
            var tw = countLabel.CreateTween();
            countLabel.Scale = new Vector2(1.35f, 1.35f);
            tw.TweenProperty(countLabel, "scale", Vector2.One, .45).SetTrans(Tween.TransitionType.Elastic).SetEase(Tween.EaseType.Out);
        }
        Clear(sequence);
        Clear(earlier);
        Clear(shows);
        if (showing)
        {
            foreach (var e in v.Shows)
            {
                var row = ShowRow(g, e);
                shows.AddChild(row);
                if (animate && v.FreshShows.Contains(e))
                {
                    row.Modulate = row.Modulate with { A = 0 };
                    row.CreateTween().TweenProperty(row, "modulate:a", 1f, .3);
                }
            }
        }
        else
        {
            for (var i = 0; i < v.Played.Count; i++)
            {
                var pc = v.Played[i];
                var current = i >= v.SeqStart;
                var (box, face) = PlayedCard(pc, meSeat, current ? CardFace.Normal : CardFace.Small);
                (current ? sequence : earlier).AddChild(box);
                if (animate && v.Fresh.Contains(pc.Card))
                {
                    face.Enter(pc.Seat == meSeat ? CardFace.Entry.DealIn : CardFace.Entry.DealInOpp);
                }
            }
        }

        // My hand, by rank.
        Clear(myHand);
        var discarding = v.Phase == Phase.Discard && v.MustAct && !v.Busy;
        var myPlay = v.Phase == Phase.Pegging && v.MustAct && !v.Busy;
        var hand = v.Hand.OrderBy(c => c % 13).ThenBy(c => c).ToList();
        for (var i = 0; i < hand.Count; i++)
        {
            var c = hand[i];
            var face = new CardFace(c, CardFace.Normal);
            if (discarding)
            {
                face.Clickable = true;
                face.Selected = picked.Contains(c);
                face.Clicked += () => TogglePick(c);
            }
            else if (myPlay)
            {
                var play = v.Legal.FirstOrDefault(a => a.Kind == ActionKind.Play && a.Cards[0] == c);
                face.Clickable = play != null;
                face.Dim = play == null;
                if (play != null)
                {
                    face.Clicked += () => Act(play);
                }
            }
            myHand.AddChild(face);
            if (animate && v.Fresh.Contains(c) && !v.Played.Any(p => p.Card == c))
            {
                face.Enter(CardFace.Entry.DealIn, i * .07);
            }
        }
        discardButton.Visible = discarding;
        discardButton.Disabled = picked.Count != 2;
        discardButton.Text = picked.Count == 2 ? "Discard" : $"Pick {2 - picked.Count} more";

        var go = false;
        if (finished)
        {
            prompt.Text = Text.GameOver(g);
        }
        else if (g.Status == GameStatus.Waiting)
        {
            prompt.Text = "Waiting for someone to join…";
        }
        else if (v.Busy)
        {
            prompt.Text = "";
        }
        else if (discarding)
        {
            prompt.Text = cribMine ? "Choose two cards for your crib" : $"Choose two cards for {theirName}'s crib";
            go = true;
        }
        else if (v.Phase == Phase.Discard)
        {
            prompt.Text = awaiting || v.Discarded ? $"Waiting for {theirName} to discard…" : "";
        }
        else if (myPlay)
        {
            prompt.Text = "Your turn — play a card";
            go = true;
        }
        else if (v.Phase == Phase.Pegging)
        {
            prompt.Text = $"{theirName} to play…";
        }
        else
        {
            prompt.Text = "";
        }
        prompt.AddThemeColorOverride("font_color", go ? Style.P.Accent : Style.P.Ink);

        RenderBanner(gs, animate);
        v.ClearHints();
    }

    static (Control, CardFace) PlayedCard(PlayedCard pc, int meSeat, Vector2 size)
    {
        var box = new VBoxContainer();
        box.AddThemeConstantOverride("separation", 3);
        var face = new CardFace(pc.Card, size);
        box.AddChild(face);
        var bar = new ColorRect
        {
            Color = pc.Seat == meSeat ? Style.P.Mine : Style.P.Theirs,
            CustomMinimumSize = new Vector2(size.X - 16, 3),
            SizeFlagsHorizontal = SizeFlags.ShrinkCenter,
        };
        box.AddChild(bar);
        return (box, face);
    }

    static Control ShowRow(GameSummary g, Event e)
    {
        var row = new HBoxContainer();
        row.AddThemeConstantOverride("separation", 8);
        var who = Text.SeatName(g, e.Seat);
        var whose = who == "You" ? "Your" : who + "'s";
        var mine = e.Seat == g.Seat;
        var label = Style.Label($"{whose} {(e.Kind == EventKind.ShowCrib ? "crib" : "hand")}", 14, mine ? Style.P.Mine : Style.P.Theirs);
        label.CustomMinimumSize = new Vector2(120, 0);
        row.AddChild(label);
        var fan = Fan(-14);
        foreach (var c in e.Cards)
        {
            fan.AddChild(new CardFace(c, CardFace.Small));
        }
        row.AddChild(fan);
        row.AddChild(Style.Label("+", 16, Style.P.Muted));
        row.AddChild(new CardFace(e.Card, CardFace.Small));
        var pts = Style.Label($"{e.Points}", 22, mine ? Style.P.Mine : Style.P.Theirs);
        pts.CustomMinimumSize = new Vector2(30, 0);
        pts.HorizontalAlignment = HorizontalAlignment.Right;
        row.AddChild(pts);
        var detail = Style.Label(Text.Breakdown(e.Show), 13, Style.P.Muted);
        detail.SizeFlagsHorizontal = SizeFlags.ExpandFill;
        detail.CustomMinimumSize = new Vector2(150, 0);
        detail.AutowrapMode = TextServer.AutowrapMode.WordSmart;
        row.AddChild(detail);
        return row;
    }

    // RenderLog appends the selected game's new log lines, starting over
    // when the game changes.
    void RenderLog(GameState? gs)
    {
        if (gs == null)
        {
            log.Clear();
            logGame = null;
            return;
        }
        if (logGame != gs.Game.GameId || logShown > gs.Log.Count)
        {
            log.Clear();
            logGame = gs.Game.GameId;
            logShown = System.Math.Max(0, gs.Log.Count - 300);
        }
        var p = Style.P;
        foreach (var l in gs.Log.Skip(logShown))
        {
            var color = l.Neutral ? p.Muted : l.Mine ? p.Mine : p.Theirs;
            if (!l.Neutral && l.Points == 0)
            {
                color = color.Lerp(p.Ink, .55f);
            }
            log.AppendText($"[color=#{color.ToHtml(false)}]{Escape(l.Text)}[/color]" +
                (l.Points > 0 ? $" [b][color=#{p.Accent.ToHtml(false)}]+{l.Points}[/color][/b]" : "") + "\n");
        }
        logShown = gs.Log.Count;
        while (log.GetParagraphCount() > 400)
        {
            log.RemoveParagraph(0);
        }
    }

    static string Escape(string s) => s.Replace("[", "[lb]");

    void RenderBanner(GameState gs, bool animate)
    {
        var v = gs.V;
        var g = gs.Game;
        if (v.Over is not { } over || v.BannerClosed || v.Busy)
        {
            Clear(bannerLayer);
            bannerFor = null;
            return;
        }
        if (bannerFor == g.GameId)
        {
            return;
        }
        bannerFor = g.GameId;
        Clear(bannerLayer);
        var meSeat = gs.Me;
        var won = over.Winner == meSeat;
        var loser = v.Scores[1 - over.Winner];
        var skunk = over.Resigned ? 0 : loser < 61 ? 2 : loser < 91 ? 1 : 0;
        var opp = 1 - meSeat;
        var bot = opp < g.Players.Count ? g.Players[opp].Bot : "";

        var panel = new PanelContainer { MouseFilter = MouseFilterEnum.Stop };
        var sb = Style.Box(Style.P.Panel, 16, Style.P.Accent, 2);
        sb.ContentMarginLeft = sb.ContentMarginRight = 36;
        sb.ContentMarginTop = sb.ContentMarginBottom = 22;
        sb.ShadowColor = new Color(0, 0, 0, .5f);
        sb.ShadowSize = 18;
        panel.AddThemeStyleboxOverride("panel", sb);
        var box = new VBoxContainer();
        box.AddThemeConstantOverride("separation", 8);
        panel.AddChild(box);
        Label Centered(string text, int size, Color c)
        {
            var l = Style.Label(text, size, c);
            l.HorizontalAlignment = HorizontalAlignment.Center;
            box.AddChild(l);
            return l;
        }
        Centered(won ? "You win!" : "You lose", 34, won ? Style.P.Accent : Style.P.Ink);
        Centered($"{v.Scores[meSeat]} – {v.Scores[opp]}", 26, Style.P.Ink);
        if (skunk > 0)
        {
            Centered(skunk == 2 ? "Double skunk!" : "Skunk!", 18, Style.P.Theirs);
        }
        if (over.Resigned)
        {
            Centered(won ? "Your opponent resigned." : "You resigned.", 15, Style.P.Muted);
        }
        var row = new HBoxContainer { Alignment = BoxContainer.AlignmentMode.Center };
        row.AddThemeConstantOverride("separation", 12);
        if (bot != "")
        {
            row.AddChild(Style.Primary("Play again", () => { v.BannerClosed = true; RenderTable(); CreateGame(bot); }));
        }
        row.AddChild(Style.Button("View table", () => { v.BannerClosed = true; RenderTable(); }));
        box.AddChild(Style.Expand());
        box.AddChild(row);
        bannerLayer.AddChild(panel);

        if (animate && v.OverFresh)
        {
            panel.Scale = new Vector2(.7f, .7f);
            panel.Modulate = panel.Modulate with { A = 0 };
            panel.Resized += () => panel.PivotOffset = panel.Size / 2;
            var tw = panel.CreateTween().SetParallel();
            tw.TweenProperty(panel, "scale", Vector2.One, .45).SetTrans(Tween.TransitionType.Back).SetEase(Tween.EaseType.Out);
            tw.TweenProperty(panel, "modulate:a", 1f, .25);
            Sfx.Play(won ? "win" : "lose");
            if (won)
            {
                Confetti();
            }
        }
        v.OverFresh = false;
    }

    void Confetti()
    {
        var p = Style.P;
        var c = new CpuParticles2D
        {
            Amount = 140,
            Lifetime = 2.6,
            OneShot = true,
            Explosiveness = .92f,
            Direction = Vector2.Up,
            Spread = 70,
            InitialVelocityMin = 320,
            InitialVelocityMax = 620,
            Gravity = new Vector2(0, 700),
            ScaleAmountMin = 4,
            ScaleAmountMax = 8,
            AngularVelocityMin = -360,
            AngularVelocityMax = 360,
            ColorInitialRamp = new Gradient
            {
                Offsets = new[] { 0f, .25f, .5f, .75f, 1f },
                Colors = new[] { p.Accent, p.Mine, p.Theirs, new Color("#ffffff"), p.Accent },
            },
        };
        fxLayer.AddChild(c);
        var r = bannerLayer.GetGlobalRect();
        c.Position = new Vector2(r.GetCenter().X, r.Position.Y + r.Size.Y * .55f) - fxLayer.GetGlobalRect().Position;
        c.Emitting = true;
        GetTree().CreateTimer(3.5).Timeout += c.QueueFree;
    }

    // ---------------------------------------------------------------
    // Actions

    void TogglePick(uint c)
    {
        if (!picked.Remove(c) && picked.Count < 2)
        {
            picked.Add(c);
        }
        Sfx.Play("card");
        RenderTable();
    }

    void Discard()
    {
        if (selected == null || picked.Count != 2)
        {
            return;
        }
        var a = games[selected].Update?.Legal.FirstOrDefault(a =>
            a.Kind == ActionKind.Discard && a.Cards.All(picked.Contains));
        if (a != null)
        {
            Act(a);
        }
    }

    void ConfirmResign()
    {
        var id = selected;
        var waiting = id != null && games[id].Game.Status == GameStatus.Waiting;
        var d = new ConfirmationDialog
        {
            DialogText = waiting ? "Cancel this game?" : "Resign this game?",
            OkButtonText = waiting ? "Cancel game" : "Resign",
            CancelButtonText = "Keep playing",
        };
        ui.AddChild(d);
        d.Confirmed += () => Send(new ClientMessage { Resign = new Resign { GameId = id } });
        d.VisibilityChanged += () => { if (!d.Visible) d.QueueFree(); };
        d.PopupCentered();
    }

    void SaveScreenshot()
    {
        var img = GetViewport().GetTexture().GetImage();
        var err = img.SavePng(opts.Screenshot);
        GD.Print($"screenshot {opts.Screenshot}: {err}");
        GetTree().Quit();
    }
}
