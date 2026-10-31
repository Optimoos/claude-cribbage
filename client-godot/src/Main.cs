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

    // Login
    Control loginView = null!;
    LineEdit serverEdit = null!, userEdit = null!, passEdit = null!, nickEdit = null!;
    Label loginError = null!;

    // Lobby
    Control lobbyView = null!;
    Label status = null!, who = null!, notice = null!;
    VBoxContainer botPicks = null!, myList = null!, openList = null!, recentList = null!;

    // Table
    Control tableView = null!, emptyView = null!;
    Label oppName = null!, oppScore = null!, meName = null!, meScore = null!, prompt = null!, cribLabel = null!, countLabel = null!;
    HBoxContainer oppHand = null!, cribCards = null!, sequence = null!, earlier = null!, myHand = null!;
    VBoxContainer shows = null!;
    Control starterSlot = null!;
    Board board = null!;
    Button discardButton = null!, resignButton = null!;
    RichTextLabel log = null!;
    PanelContainer banner = null!;
    Label bannerText = null!;
    Button bannerAgain = null!;
    string? bannerClosedFor;

    public override void _Ready()
    {
        opts = Options.Parse(OS.GetCmdlineUserArgs());
        settings = new Settings(opts.Profile);
        if (opts.Server != null)
        {
            settings.Server = opts.Server;
        }
        Theme = Style.Build();
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
        if (opts.Autoplay)
        {
            AutoplayTick();
        }
    }

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
                who.Text = $"{me.Nickname} ({me.Username})";
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
            case ServerMessage.BodyOneofCase.GameList:
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
        gs.Apply(u);
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
        if (id == selected)
        {
            RenderTable();
        }
    }

    void Select(string id)
    {
        if (selected != id)
        {
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
        picked.Clear();
        awaiting = true;
        RenderTable();
    }

    void CreateGame(string bot) => Send(new ClientMessage { CreateGame = new CreateGame { Bot = bot } });

    // ---------------------------------------------------------------
    // Layout

    void BuildUi()
    {
        var bg = new ColorRect { Color = Style.Felt };
        AddChild(bg);
        bg.SetAnchorsAndOffsetsPreset(LayoutPreset.FullRect);

        var root = new HBoxContainer();
        root.AddThemeConstantOverride("separation", 0);
        AddChild(root);
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
        tableView = BuildTable();
        split.AddChild(tableView);
        emptyView = new CenterContainer { SizeFlagsHorizontal = SizeFlags.ExpandFill };
        emptyView.AddChild(Style.Label("Pick an opponent to start a game.", 20, Style.Muted));
        split.AddChild(emptyView);
        split.AddChild(BuildLog());

        loginView = BuildLogin();
        AddChild(loginView);
        loginView.SetAnchorsAndOffsetsPreset(LayoutPreset.FullRect);
        banner = BuildBanner();
        AddChild(banner);
    }

    Control BuildLogin()
    {
        var shade = new ColorRect { Color = new Color(0.02f, 0.08f, 0.05f, 0.92f) };
        var center = new CenterContainer();
        shade.AddChild(center);
        center.SetAnchorsAndOffsetsPreset(LayoutPreset.FullRect);
        var panel = new PanelContainer { CustomMinimumSize = new Vector2(380, 0) };
        center.AddChild(panel);
        var box = new VBoxContainer();
        box.AddThemeConstantOverride("separation", 10);
        panel.AddChild(box);
        box.AddChild(Style.Label("♠ Cribbage", 32, Style.Gold));
        box.AddChild(Style.Label("Log in or create an account.", 15, Style.Muted));

        LineEdit Field(string label, string placeholder)
        {
            box.AddChild(Style.Label(label, 14, Style.Muted));
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
        loginError = Style.Label("", 14, new Color("#ff9a8a"));
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
        loginView.Visible = show && !opts.Autoplay;
        if (loginView.Visible)
        {
            (userEdit.Text == "" ? userEdit : passEdit).GrabFocus();
        }
    }

    Control BuildLobby()
    {
        var panel = new PanelContainer { CustomMinimumSize = new Vector2(290, 0) };
        panel.AddThemeStyleboxOverride("panel", Style.Box(Style.Panel, 0));
        var scroll = new ScrollContainer { HorizontalScrollMode = ScrollContainer.ScrollMode.Disabled };
        panel.AddChild(scroll);
        var box = new VBoxContainer { SizeFlagsHorizontal = SizeFlags.ExpandFill };
        box.AddThemeConstantOverride("separation", 8);
        scroll.AddChild(box);

        var title = new HBoxContainer();
        title.AddChild(Style.Label("♠ Cribbage", 24, Style.Gold));
        title.AddChild(Style.Expand());
        status = Style.Label("", 13, Style.Muted);
        title.AddChild(status);
        box.AddChild(title);
        var acct = new HBoxContainer();
        who = Style.Label("", 13, Style.Muted);
        who.SizeFlagsHorizontal = SizeFlags.ExpandFill;
        who.ClipText = true;
        acct.AddChild(who);
        acct.AddChild(Style.Button("Log out", Logout));
        box.AddChild(acct);

        box.AddChild(Heading("New game"));
        botPicks = new VBoxContainer();
        box.AddChild(botPicks);
        RenderBotPicks();
        var people = new HBoxContainer();
        people.AddChild(Style.Button("Find a match", () => Send(new ClientMessage { FindMatch = new FindMatch() })));
        people.AddChild(Style.Button("Open game", () => CreateGame("")));
        people.AddChild(Style.Button("↻", () => Send(new ClientMessage { ListGames = new ListGames() })));
        box.AddChild(people);
        notice = Style.Label("", 14, Style.Gold);
        notice.AutowrapMode = TextServer.AutowrapMode.WordSmart;
        box.AddChild(notice);

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

    static Label Heading(string text)
    {
        var l = Style.Label(text.ToUpperInvariant(), 12, Style.Muted);
        l.AddThemeConstantOverride("line_spacing", 0);
        return l;
    }

    void Logout()
    {
        settings.Token = "";
        me = null;
        games.Clear();
        selected = null;
        Connect(); // a fresh, unauthenticated connection
        ShowLogin(true);
        RenderAll();
    }

    Control BuildTable()
    {
        var box = new VBoxContainer { SizeFlagsHorizontal = SizeFlags.ExpandFill };
        box.AddThemeConstantOverride("separation", 12);

        var top = new HBoxContainer();
        top.AddThemeConstantOverride("separation", 16);
        box.AddChild(top);
        top.AddChild(Plate(out oppName, out oppScore, Style.Theirs));
        oppHand = Fan(-40);
        top.AddChild(oppHand);

        var boardRow = new CenterContainer();
        board = new Board();
        boardRow.AddChild(board);
        box.AddChild(boardRow);

        var mid = new HBoxContainer { SizeFlagsVertical = SizeFlags.ExpandFill };
        mid.AddThemeConstantOverride("separation", 20);
        box.AddChild(mid);
        var crib = new VBoxContainer { CustomMinimumSize = new Vector2(150, 0) };
        cribLabel = Style.Label("Crib", 14, Style.Muted);
        crib.AddChild(cribLabel);
        cribCards = Fan(-32);
        crib.AddChild(cribCards);
        mid.AddChild(crib);

        var center = new VBoxContainer { SizeFlagsHorizontal = SizeFlags.ExpandFill };
        center.AddThemeConstantOverride("separation", 6);
        countLabel = Style.Label("", 22, Style.Gold);
        center.AddChild(countLabel);
        sequence = Fan(-28);
        center.AddChild(sequence);
        earlier = Fan(-18);
        center.AddChild(earlier);
        shows = new VBoxContainer();
        shows.AddThemeConstantOverride("separation", 6);
        center.AddChild(shows);
        mid.AddChild(center);

        var starter = new VBoxContainer();
        starter.AddChild(Style.Label("Starter", 14, Style.Muted));
        starterSlot = new HBoxContainer();
        starter.AddChild(starterSlot);
        mid.AddChild(starter);

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
        bottom.AddChild(Plate(out meName, out meScore, Style.Mine));
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
        p.AddThemeStyleboxOverride("panel", Style.Box(new Color(0, 0, 0, 0.25f), 8, color with { A = 0.6f }));
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
        var panel = new PanelContainer { CustomMinimumSize = new Vector2(270, 0) };
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

    PanelContainer BuildBanner()
    {
        var p = new PanelContainer { Visible = false };
        p.AddThemeStyleboxOverride("panel", Style.Box(new Color("#10281c"), 12, Style.Gold, 2));
        var box = new VBoxContainer();
        box.AddThemeConstantOverride("separation", 12);
        p.AddChild(box);
        bannerText = Style.Label("", 28, Style.Gold);
        bannerText.HorizontalAlignment = HorizontalAlignment.Center;
        box.AddChild(bannerText);
        var row = new HBoxContainer { Alignment = BoxContainer.AlignmentMode.Center };
        row.AddThemeConstantOverride("separation", 12);
        bannerAgain = Style.Primary("Play again", PlayAgain);
        row.AddChild(bannerAgain);
        row.AddChild(Style.Button("Close", () => { bannerClosedFor = selected; banner.Visible = false; }));
        box.AddChild(row);
        return p;
    }

    // ---------------------------------------------------------------
    // Rendering

    void RenderAll()
    {
        RenderLobby();
        RenderTable();
        UpdateTitle();
    }

    void SetStatus(string text)
    {
        status.Text = text;
        status.AddThemeColorOverride("font_color", text == "online" ? new Color("#7fdc9a") : Style.Muted);
    }

    void Notice(string text) => notice.Text = text;

    void UpdateTitle()
    {
        var turn = games.Values.Any(g => g.Game.YourTurn && g.Game.Status == GameStatus.Active);
        GetWindow().Title = turn ? "● Your turn — Cribbage" : "Cribbage";
    }

    void RenderBotPicks()
    {
        if (botPicks == null)
        {
            return;
        }
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
            myList.AddChild(Style.Label("No games in progress", 14, Style.Muted));
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
            openList.AddChild(Style.Label("Nobody is waiting", 14, Style.Muted));
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
            recentList.AddChild(Style.Label("None yet", 14, Style.Muted));
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
            b.AddThemeStyleboxOverride("normal", Style.Box(new Color("#2d6847"), 6, Style.Gold));
        }
        else if (tag == "your turn" || tag == "join")
        {
            b.AddThemeColorOverride("font_color", Style.Gold);
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

    void RenderTable()
    {
        var gs = selected != null && games.TryGetValue(selected, out var s) ? s : null;
        tableView.Visible = gs != null;
        emptyView.Visible = gs == null;
        RenderLog(gs);
        if (gs == null)
        {
            banner.Visible = false;
            return;
        }
        var g = gs.Game;
        var o = gs.Obs;
        var meSeat = gs.Me;
        var opp = 1 - meSeat;
        var oppPlayer = opp < g.Players.Count ? g.Players[opp] : null;
        var oppLabel = Text.Opponent(g);
        oppName.Text = oppPlayer is { Bot: not "" } ? Text.BotLabel(oppPlayer.Bot) :
            oppPlayer is { UserId: not 0 } ? $"{oppLabel}  ·  {(oppPlayer.Connected ? "online" : "away")}" : oppLabel;
        meName.Text = me != null ? $"{me.Nickname}  ·  you" : "You";
        oppScore.Text = gs.Scores[opp].ToString();
        meScore.Text = gs.Scores[meSeat].ToString();
        board.Set(meSeat, gs.Scores, gs.Trail);

        Clear(oppHand);
        Clear(cribCards);
        Clear(sequence);
        Clear(earlier);
        Clear(shows);
        Clear(starterSlot);
        Clear(myHand);
        countLabel.Text = "";
        discardButton.Visible = false;
        resignButton.Visible = g.Status == GameStatus.Active;
        RenderBanner(gs);
        if (o == null)
        {
            prompt.Text = g.Status == GameStatus.Waiting ? "Waiting for an opponent to join…" : "";
            cribLabel.Text = "Crib";
            return;
        }

        var theirName = Text.SeatName(g, opp);
        for (var i = 0; i < o.OppHandSize; i++)
        {
            oppHand.AddChild(new CardFace(null, CardFace.Small));
        }
        var myCrib = o.Dealer == meSeat;
        cribLabel.Text = myCrib ? "Your crib" : $"{theirName}'s crib";
        var inCrib = (o.Discarded ? 2 : 0) + (o.OppDiscarded ? 2 : 0);
        for (var i = 0; i < inCrib; i++)
        {
            cribCards.AddChild(new CardFace(null, CardFace.Small));
        }
        if (o.HasStarter)
        {
            starterSlot.AddChild(new CardFace(o.Starter, CardFace.Normal));
        }
        else
        {
            starterSlot.AddChild(new CardFace(null, CardFace.Normal));
        }

        var legal = awaiting ? new List<Action>() : gs.Update!.Legal.ToList();
        var myTurn = o.MustAct && !awaiting && g.Status == GameStatus.Active;

        if (o.Phase == Phase.Pegging)
        {
            countLabel.Text = $"Count {o.Count}";
            for (var i = 0; i < o.Played.Count; i++)
            {
                var pc = o.Played[i];
                var current = i >= o.SeqStart;
                var c = PlayedCard(pc, meSeat, current ? CardFace.Normal : CardFace.Small);
                (current ? sequence : earlier).AddChild(c);
            }
        }
        else if (gs.Shows.Count > 0)
        {
            countLabel.Text = "Last hand";
            foreach (var e in gs.Shows)
            {
                shows.AddChild(ShowRow(g, e));
            }
        }

        // My cards: the hand, or after discarding, the cards still held.
        var hand = o.Hand.ToList();
        foreach (var c in hand)
        {
            var face = new CardFace(c, CardFace.Normal);
            if (o.Phase == Phase.Discard && myTurn && !o.Discarded)
            {
                face.Clickable = true;
                face.Selected = picked.Contains(c);
                var card = c;
                face.Clicked += () => TogglePick(card);
            }
            else if (o.Phase == Phase.Pegging && myTurn)
            {
                var play = legal.FirstOrDefault(a => a.Kind == ActionKind.Play && a.Cards[0] == c);
                face.Clickable = play != null;
                face.Dim = play == null;
                if (play != null)
                {
                    face.Clicked += () => Act(play);
                }
            }
            myHand.AddChild(face);
        }
        if (o.Phase == Phase.Discard && o.Discarded)
        {
            foreach (var c in o.Discards)
            {
                var f = new CardFace(c, CardFace.Small) { Dim = true };
                myHand.AddChild(f);
            }
        }

        if (g.Status == GameStatus.Finished)
        {
            prompt.Text = Text.GameOver(g);
        }
        else if (o.Phase == Phase.Discard)
        {
            if (myTurn && !o.Discarded)
            {
                prompt.Text = myCrib ? "Choose two cards for your crib" : $"Choose two cards for {theirName}'s crib";
                discardButton.Visible = true;
                discardButton.Disabled = picked.Count != 2;
                discardButton.Text = picked.Count == 2 ? "Discard" : $"Pick {2 - picked.Count} more";
            }
            else
            {
                prompt.Text = o.OppDiscarded ? "" : $"Waiting for {theirName} to discard…";
            }
        }
        else if (o.Phase == Phase.Pegging)
        {
            prompt.Text = myTurn ? "Your turn — play a card" : awaiting ? "" : $"{theirName}'s turn";
        }
        else
        {
            prompt.Text = "";
        }
    }

    static Control PlayedCard(PlayedCard pc, int meSeat, Vector2 size)
    {
        var box = new VBoxContainer();
        box.AddThemeConstantOverride("separation", 3);
        box.AddChild(new CardFace(pc.Card, size));
        var bar = new ColorRect
        {
            Color = pc.Seat == meSeat ? Style.Mine : Style.Theirs,
            CustomMinimumSize = new Vector2(size.X - 16, 3),
            SizeFlagsHorizontal = SizeFlags.ShrinkCenter,
        };
        box.AddChild(bar);
        return box;
    }

    static Control ShowRow(GameSummary g, Event e)
    {
        var row = new HBoxContainer();
        row.AddThemeConstantOverride("separation", 8);
        var who = Text.SeatName(g, e.Seat);
        var whose = who == "You" ? "Your" : who + "'s";
        var label = Style.Label($"{whose} {(e.Kind == EventKind.ShowCrib ? "crib" : "hand")}", 14, e.Seat == g.Seat ? Style.Mine : Style.Theirs);
        label.CustomMinimumSize = new Vector2(120, 0);
        row.AddChild(label);
        var fan = Fan(-14);
        foreach (var c in e.Cards)
        {
            fan.AddChild(new CardFace(c, CardFace.Small));
        }
        row.AddChild(fan);
        row.AddChild(new CardFace(e.Card, CardFace.Small) { Dim = true });
        var pts = Style.Label($"+{e.Points}", 20, Style.Gold);
        row.AddChild(pts);
        var detail = Style.Label(Text.Breakdown(e.Show), 13, Style.Muted);
        detail.SizeFlagsHorizontal = SizeFlags.ExpandFill;
        detail.CustomMinimumSize = new Vector2(150, 0);
        detail.AutowrapMode = TextServer.AutowrapMode.WordSmart;
        row.AddChild(detail);
        return row;
    }

    void RenderLog(GameState? gs)
    {
        log.Clear();
        if (gs == null)
        {
            return;
        }
        foreach (var l in gs.Log.TakeLast(300))
        {
            var color = l.Points > 0 ? (l.Mine ? Style.Mine : Style.Theirs).ToHtml(false) : "d8ddd6";
            log.AppendText($"[color=#{color}]{Escape(l.Text)}[/color]" + (l.Points > 0 ? $" [b][color=#e8c46a]+{l.Points}[/color][/b]" : "") + "\n");
        }
    }

    static string Escape(string s) => s.Replace("[", "[lb]");

    void RenderBanner(GameState gs)
    {
        var g = gs.Game;
        if (!gs.Finished || bannerClosedFor == g.GameId)
        {
            banner.Visible = false;
            return;
        }
        bannerText.Text = Text.GameOver(g);
        var opp = 1 - gs.Me;
        bannerAgain.Visible = opp < g.Players.Count && g.Players[opp].Bot != "";
        banner.Visible = true;
        banner.ResetSize();
        banner.Position = (Size - banner.GetCombinedMinimumSize()) / 2 + new Vector2(lobbyView.Size.X / 2, 0);
    }

    // ---------------------------------------------------------------
    // Actions

    void TogglePick(uint c)
    {
        if (!picked.Remove(c) && picked.Count < 2)
        {
            picked.Add(c);
        }
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

    void PlayAgain()
    {
        if (selected == null)
        {
            return;
        }
        var g = games[selected].Game;
        var opp = 1 - games[selected].Me;
        bannerClosedFor = selected;
        CreateGame(g.Players[opp].Bot);
    }

    void ConfirmResign()
    {
        var id = selected;
        var d = new ConfirmationDialog { DialogText = "Resign this game?", OkButtonText = "Resign" };
        AddChild(d);
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
