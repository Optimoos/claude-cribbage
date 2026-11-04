using Cribbage.Protocol;
using Cribbage.Ui;
using Godot;

namespace Cribbage;

// The sign-in screen. There are no passwords: people sign in with a code
// mailed to them, or by approving this computer from a device where they
// are signed in (the web client, which can use passkeys). Creating an
// account mails a code too; the account exists once it is entered. The
// links in those emails open in a web browser, so here the code is the way.
public partial class Main
{
    enum LoginStage { Start, Register, Code, Device }

    LoginStage loginStage = LoginStage.Start;
    // Bumped on every change of stage, so a reply to a request made on an
    // earlier stage (after Back, say) is ignored.
    int loginSeq;

    // Waiting for an emailed code: the challenge it goes with, when it
    // expires (Now), and how to ask for it again.
    string codeChallenge = "";
    uint codeLength = 8;
    double codeExpires;
    string codeName = ""; // the username or email it was asked for
    Register? codeRegister; // set when the code creates an account

    // Waiting for another device to approve this one.
    DeviceCode? device;
    double deviceExpires;
    bool deviceExpired;

    Control startPane = null!, registerPane = null!, codePane = null!, devicePane = null!;
    LineEdit regUser = null!, regNick = null!, regEmail = null!, signInCode = null!;
    Label codeIntro = null!, codeTimer = null!, deviceCodeLabel = null!, deviceUrl = null!, deviceStatus = null!, deviceTimer = null!;
    Button codeSubmit = null!, deviceOpen = null!, deviceRestart = null!;
    int lastTick = -1;

    Control BuildLogin()
    {
        var shade = new ColorRect { Color = Style.P.Page with { A = 0.94f } };
        var center = new CenterContainer();
        shade.AddChild(center);
        center.SetAnchorsAndOffsetsPreset(LayoutPreset.FullRect);
        var panel = new PanelContainer { CustomMinimumSize = new Vector2(400, 0) };
        center.AddChild(panel);
        var box = new VBoxContainer();
        box.AddThemeConstantOverride("separation", 10);
        panel.AddChild(box);
        box.AddChild(Style.Label("♠ Cribbage", 32, Style.P.Accent));

        startPane = Pane(box);
        registerPane = Pane(box);
        codePane = Pane(box);
        devicePane = Pane(box);
        BuildStart((VBoxContainer)startPane);
        BuildRegister((VBoxContainer)registerPane);
        BuildCode((VBoxContainer)codePane);
        BuildDevice((VBoxContainer)devicePane);

        loginError = Wrapped("", 14, Style.P.Theirs);
        box.AddChild(loginError);
        box.AddChild(Links());
        RenderLogin();
        return shade;
    }

    static VBoxContainer Pane(VBoxContainer parent)
    {
        var p = new VBoxContainer();
        p.AddThemeConstantOverride("separation", 10);
        parent.AddChild(p);
        return p;
    }

    static LineEdit LoginField(VBoxContainer box, string label, string placeholder = "")
    {
        box.AddChild(Style.Label(label, 14, Style.P.Muted));
        var e = new LineEdit { PlaceholderText = placeholder, MaxLength = 254 };
        box.AddChild(e);
        return e;
    }

    static HBoxContainer Row(VBoxContainer box, params Control[] items)
    {
        var row = new HBoxContainer();
        row.AddThemeConstantOverride("separation", 10);
        foreach (var c in items)
        {
            row.AddChild(c);
        }
        box.AddChild(row);
        return row;
    }

    void BuildStart(VBoxContainer box)
    {
        box.AddChild(Style.Label("Sign in or create an account.", 15, Style.P.Muted));
        serverEdit = LoginField(box, "Server", "wss://host/rtc");
        serverEdit.Text = settings.Server;
        serverEdit.TextSubmitted += _ => ChangeServer();
        serverEdit.FocusExited += ChangeServer;
        userEdit = LoginField(box, "Username or email");
        userEdit.Text = settings.LastUser;
        userEdit.TextSubmitted += _ => RequestCode();
        Row(box, Style.Primary("Email me a code", RequestCode));
        box.AddChild(Style.Label("or", 14, Style.P.Muted));
        Row(box, Style.Button("Sign in with browser", StartDevice));
        box.AddChild(Wrapped("Approve this computer from a phone or computer where you're signed in to Cribbage.", 13, Style.P.Muted));
        var reg = Row(box, Style.Label("New here?", 14, Style.P.Muted), Style.Button("Create account", () => SetLoginStage(LoginStage.Register)));
        reg.Alignment = BoxContainer.AlignmentMode.Begin;
    }

    void BuildRegister(VBoxContainer box)
    {
        box.AddChild(Style.Label("Create an account", 20));
        regUser = LoginField(box, "Username", "used to sign in and to invite you");
        regNick = LoginField(box, "Nickname (optional)", "shown to other players");
        regEmail = LoginField(box, "Email", "we'll send a code to it");
        regUser.TextSubmitted += _ => regNick.GrabFocus();
        regNick.TextSubmitted += _ => regEmail.GrabFocus();
        regEmail.TextSubmitted += _ => Register();
        Row(box, Style.Primary("Create account", Register), Style.Button("Back", () => SetLoginStage(LoginStage.Start)));
    }

    void BuildCode(VBoxContainer box)
    {
        box.AddChild(Style.Label("Check your email", 20));
        codeIntro = Wrapped("", 14);
        box.AddChild(codeIntro);
        box.AddChild(Wrapped("The email also has a link, but it signs in a web browser, not this app.", 13, Style.P.Muted));
        signInCode = new LineEdit { PlaceholderText = "1234 5678", MaxLength = 24, Alignment = HorizontalAlignment.Center };
        signInCode.AddThemeFontSizeOverride("font_size", 24);
        signInCode.TextSubmitted += _ => EnterCode();
        box.AddChild(signInCode);
        codeTimer = Style.Label("", 13, Style.P.Muted);
        box.AddChild(codeTimer);
        codeSubmit = Style.Primary("Sign in", EnterCode);
        Row(box, codeSubmit, Style.Button("Send again", SendCodeAgain), Style.Button("Back", CodeBack));
    }

    void BuildDevice(VBoxContainer box)
    {
        box.AddChild(Style.Label("Sign in with browser", 20));
        box.AddChild(Wrapped("Open the link on a phone or computer where you're signed in to Cribbage, and approve this code.", 14));
        deviceCodeLabel = Style.Label("", 40, Style.P.Accent);
        deviceCodeLabel.HorizontalAlignment = HorizontalAlignment.Center;
        box.AddChild(deviceCodeLabel);
        deviceUrl = Wrapped("", 13, Style.P.Muted);
        deviceUrl.AutowrapMode = TextServer.AutowrapMode.Arbitrary;
        deviceUrl.HorizontalAlignment = HorizontalAlignment.Center;
        box.AddChild(deviceUrl);
        deviceStatus = Wrapped("", 14);
        box.AddChild(deviceStatus);
        deviceTimer = Style.Label("", 13, Style.P.Muted);
        box.AddChild(deviceTimer);
        deviceOpen = Style.Primary("Open link", () => { if (device is { Url: not "" } d) OS.ShellOpen(d.Url); });
        deviceRestart = Style.Button("New code", StartDevice);
        Row(box, deviceOpen, deviceRestart, Style.Button("Back", () => SetLoginStage(LoginStage.Start)));
    }

    void SetLoginStage(LoginStage stage)
    {
        loginSeq++;
        loginStage = stage;
        if (stage != LoginStage.Device)
        {
            device = null;
        }
        LoginMessage("");
        RenderLogin();
        FocusLogin();
    }

    // ResetLogin forgets a sign-in in progress, once signed in.
    void ResetLogin()
    {
        codeChallenge = "";
        codeRegister = null;
        device = null;
        loginSeq++;
        loginStage = LoginStage.Start;
        RenderLogin();
    }

    void RenderLogin()
    {
        startPane.Visible = loginStage == LoginStage.Start;
        registerPane.Visible = loginStage == LoginStage.Register;
        codePane.Visible = loginStage == LoginStage.Code;
        devicePane.Visible = loginStage == LoginStage.Device;
        // "an 8-digit code", "a 6-digit code"
        var digits = (codeLength is 8 or 11 or 18 ? "an " : "a ") + $"{codeLength}-digit code";
        codeIntro.Text = codeRegister != null
            ? $"We've emailed {digits} to {codeRegister.Email}. Enter it to finish creating your account."
            : codeName.Contains('@')
                ? $"If {codeName} belongs to an account, we've emailed it {digits}. Enter it here."
                : $"If “{codeName}” has an account, we've emailed it {digits}. Enter it here.";
        codeSubmit.Text = codeRegister != null ? "Create account" : "Sign in";
        deviceCodeLabel.Text = device?.Code ?? "";
        deviceUrl.Text = device?.Url ?? "";
        deviceUrl.Visible = deviceOpen.Visible = device is { Url: not "" } && !deviceExpired;
        deviceRestart.Visible = deviceExpired;
        if (device == null && !deviceExpired)
        {
            deviceStatus.Text = "Getting a code…";
        }
        lastTick = -1;
        LoginTick();
    }

    void FocusLogin()
    {
        if (!loginShown)
        {
            return;
        }
        switch (loginStage)
        {
            case LoginStage.Start:
                userEdit.GrabFocus();
                break;
            case LoginStage.Register:
                regUser.GrabFocus();
                break;
            case LoginStage.Code:
                signInCode.GrabFocus();
                break;
        }
    }

    // LoginTick counts down the code's and the device code's lifetimes.
    void LoginTick()
    {
        if (!loginShown && lastTick >= 0)
        {
            return;
        }
        var now = (int)Now;
        if (now == lastTick)
        {
            return;
        }
        lastTick = now;
        if (loginStage == LoginStage.Code)
        {
            var left = codeExpires - Now;
            codeTimer.Text = left > 0 ? $"The code expires in {Countdown(left)}." : "This code has expired. Choose Send again for a new one.";
        }
        if (loginStage == LoginStage.Device && device != null)
        {
            var left = deviceExpires - Now;
            if (left <= 0 && !deviceExpired)
            {
                DeviceExpired();
                return;
            }
            deviceTimer.Text = deviceExpired ? "" : $"The code expires in {Countdown(left)}.";
        }
        else
        {
            deviceTimer.Text = "";
        }
    }

    static string Countdown(double seconds)
    {
        var s = (int)System.Math.Ceiling(seconds);
        return s >= 120 ? $"{(s + 59) / 60} minutes" : $"{s / 60}:{s % 60:00}";
    }

    // LoginRequest sends m if connected, saying so if not; onReply runs
    // only while the stage it was sent from is current.
    bool LoginRequest(ClientMessage m, System.Func<ServerMessage, bool> onReply)
    {
        ChangeServer();
        if (conn is not { IsOpen: true })
        {
            LoginMessage(upgradeMessage != "" ? upgradeMessage : "Not connected yet…");
            return false;
        }
        var seq = loginSeq;
        var sent = Request(m, r => r.AuthOk == null && (seq != loginSeq || onReply(r)));
        if (sent == 0)
        {
            LoginMessage("Not connected yet…");
            return false;
        }
        return true;
    }

    // ---------------------------------------------------------------
    // Signing in, or creating an account, with an emailed code

    void RequestCode()
    {
        var name = userEdit.Text.Trim();
        if (name == "")
        {
            LoginMessage("Enter your username or email address.");
            userEdit.GrabFocus();
            return;
        }
        LoginMessage("Sending…", ok: true);
        LoginRequest(new ClientMessage { RequestSignIn = new RequestSignIn { Name = name } }, m =>
        {
            if (m.SignInStarted == null)
            {
                LoginMessage(m.Error?.Message ?? "Something went wrong; try again.");
                return true;
            }
            settings.LastUser = name;
            codeName = name;
            codeRegister = null;
            CodeStarted(m.SignInStarted);
            return true;
        });
    }

    void Register()
    {
        var r = new Register { Username = regUser.Text.Trim(), Nickname = regNick.Text.Trim(), Email = regEmail.Text.Trim() };
        if (r.Username == "")
        {
            LoginMessage("Choose a username.");
            regUser.GrabFocus();
            return;
        }
        if (r.Email == "")
        {
            LoginMessage("Enter your email address: we'll send a code to it.");
            regEmail.GrabFocus();
            return;
        }
        LoginMessage("Sending…", ok: true);
        LoginRequest(new ClientMessage { Register = r }, m =>
        {
            if (m.SignInStarted == null)
            {
                LoginMessage(m.Error?.Message ?? "Something went wrong; try again.");
                return true;
            }
            settings.LastUser = r.Username;
            codeRegister = r;
            CodeStarted(m.SignInStarted);
            return true;
        });
    }

    void CodeStarted(SignInStarted s)
    {
        codeChallenge = s.Challenge;
        codeLength = s.CodeLength > 0 ? s.CodeLength : 8;
        codeExpires = Now + s.ExpiresIn;
        signInCode.Text = "";
        SetLoginStage(LoginStage.Code);
    }

    void SendCodeAgain()
    {
        var msg = codeRegister != null
            ? new ClientMessage { Register = codeRegister }
            : new ClientMessage { RequestSignIn = new RequestSignIn { Name = codeName } };
        LoginMessage("Sending…", ok: true);
        LoginRequest(msg, m =>
        {
            if (m.SignInStarted == null)
            {
                LoginMessage(m.Error?.Message ?? "Something went wrong; try again.");
                return true;
            }
            codeChallenge = m.SignInStarted.Challenge;
            codeExpires = Now + m.SignInStarted.ExpiresIn;
            LoginMessage(codeRegister != null ? "We've sent a new code." : "We've sent the code again.", ok: true);
            lastTick = -1;
            LoginTick();
            signInCode.GrabFocus();
            return true;
        });
    }

    void CodeBack() => SetLoginStage(codeRegister != null ? LoginStage.Register : LoginStage.Start);

    void EnterCode()
    {
        var code = signInCode.Text.Trim();
        if (code == "")
        {
            LoginMessage("Enter the code from the email.");
            return;
        }
        LoginMessage("Checking…", ok: true);
        LoginRequest(new ClientMessage { EnterCode = new EnterCode { Challenge = codeChallenge, Code = code } }, m =>
        {
            var e = m.Error;
            if (e == null)
            {
                return false;
            }
            if (e.Code == ErrorCode.NotFound)
            {
                // Expired, or too many wrong codes: only a new one helps.
                var registering = codeRegister != null;
                CodeBack();
                LoginMessage(registering
                    ? "That code has expired or had too many wrong tries. Create the account again for a new one."
                    : "That code has expired or had too many wrong tries. Ask for a new one.");
                return true;
            }
            LoginMessage(e.Code == ErrorCode.InvalidCredentials ? "That code is wrong. Check it and try again." : e.Message);
            signInCode.SelectAll();
            signInCode.GrabFocus();
            return true;
        });
    }

    // ---------------------------------------------------------------
    // Signing in by approval from another device

    void StartDevice()
    {
        deviceExpired = false;
        SetLoginStage(LoginStage.Device);
        if (!LoginRequest(new ClientMessage { StartDeviceSignIn = new StartDeviceSignIn() }, m =>
        {
            if (m.DeviceCode == null)
            {
                deviceStatus.Text = "";
                LoginMessage(m.Error?.Message ?? "Something went wrong; try again.");
                deviceExpired = true; // offer to start again
                RenderLogin();
                return true;
            }
            device = m.DeviceCode;
            deviceExpires = Now + device.ExpiresIn;
            deviceStatus.Text = "Waiting for approval…";
            RenderLogin();
            // Open the approval page, except when only taking screenshots.
            if (device.Url != "" && opts.Show == "" && opts.Screenshot == "")
            {
                OS.ShellOpen(device.Url);
            }
            ClaimDevice();
            return true;
        }))
        {
            deviceStatus.Text = "";
            deviceExpired = true;
            RenderLogin();
        }
    }

    // ClaimDevice asks to be signed in as whoever approved the code: until
    // someone does, the server waits and sends DeviceApproved.
    void ClaimDevice()
    {
        if (device == null || deviceExpired)
        {
            return;
        }
        LoginRequest(new ClientMessage { ClaimDeviceSignIn = new ClaimDeviceSignIn { Challenge = device.Challenge } }, m =>
        {
            if (m.Ack != null)
            {
                return true; // waiting for approval
            }
            if (m.Error is { Code: ErrorCode.NotFound })
            {
                DeviceExpired();
                return true;
            }
            if (m.Error != null)
            {
                LoginMessage(m.Error.Message);
                return true;
            }
            return false;
        });
    }

    void OnDeviceApproved()
    {
        if (loginStage == LoginStage.Device && device != null)
        {
            deviceStatus.Text = "Approved. Signing in…";
            ClaimDevice();
        }
    }

    void DeviceExpired()
    {
        deviceExpired = true;
        deviceStatus.Text = "This code has expired. Choose New code to start again.";
        RenderLogin();
    }

    // ResumeLogin carries on a sign-in after reconnecting: the server
    // forgets which connection was waiting for approval.
    void ResumeLogin()
    {
        if (loginStage == LoginStage.Device && device != null && !deviceExpired)
        {
            ClaimDevice();
        }
    }
}
