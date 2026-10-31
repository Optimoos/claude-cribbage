using System.Linq;
using Cribbage.Protocol;
using Godot;

namespace Cribbage;

// Autoplay (--autoplay) lets the client play one game by itself through
// the real UI code, so it can be tested without a person:
//   - it registers --user (or logs in if the name is taken), unless a
//     stored session resumes;
//   - it carries on a game already in progress, or else starts one with
//     --vs BOT, --open (an open game), --join (the first open game),
//     --invite USER or --accept (the first invite to it);
//   - it plays random legal moves and quits when the game ends, printing
//     the result, or kills itself after --quit-after moves.
public partial class Main
{
    readonly RandomNumberGenerator rng = new();
    bool authSent, decided;
    string? autoGame;
    int moves;
    double nextPoll, nextMove;

    static double Now => Time.GetTicksMsec() / 1000.0;

    void AutoplayTick()
    {
        if (conn is not { IsOpen: true })
        {
            return;
        }
        if (me == null)
        {
            if (!authSent && settings.Token == "")
            {
                authSent = true;
                Say($"registering {opts.User}");
                Send(new ClientMessage { Register = new Register { Username = opts.User, Password = opts.Password, Nickname = opts.User } });
            }
            return;
        }
        if (decided && autoGame == null && opts.Mode == "accept" && Now >= nextPoll)
        {
            nextPoll = Now + 1;
            var inv = invites.FirstOrDefault(i => i.Incoming);
            if (inv != null)
            {
                Say($"accepting {inv.From.Nickname}'s invite");
                Respond(inv, true);
            }
            else
            {
                Send(new ClientMessage { ListGames = new ListGames() });
            }
        }
        if (decided && autoGame == null && opts.Mode == "join" && Now >= nextPoll)
        {
            nextPoll = Now + 1;
            var open = openGames.FirstOrDefault();
            if (open != null)
            {
                Say($"joining {open.GameId}");
                Send(new ClientMessage { JoinGame = new JoinGame { GameId = open.GameId } });
            }
            else
            {
                Send(new ClientMessage { ListGames = new ListGames() });
            }
        }
        if (autoGame == null || !games.TryGetValue(autoGame, out var gs))
        {
            return;
        }
        if (gs.Finished)
        {
            var g = gs.Game;
            Say($"game {g.GameId} over after {moves} moves: {Text.GameOver(g)}");
            opts.Autoplay = false;
            if (opts.Screenshot == "")
            {
                GetTree().Quit(0);
            }
            return;
        }
        if (selected != autoGame)
        {
            Select(autoGame);
        }
        var o = gs.Obs;
        if (o == null || !o.MustAct || awaiting || gs.Game.Status != GameStatus.Active || Now < nextMove)
        {
            return;
        }
        var legal = gs.Update!.Legal;
        if (legal.Count == 0)
        {
            return;
        }
        nextMove = Now + opts.Pace;
        var a = legal[rng.RandiRange(0, legal.Count - 1)];
        if (a.Kind == ActionKind.Discard)
        {
            // Through the UI path: pick both cards, then press Discard.
            picked.Clear();
            foreach (var c in a.Cards)
            {
                TogglePick(c);
            }
            Discard();
        }
        else
        {
            Act(a);
        }
        moves++;
        if (opts.QuitAfter > 0 && moves >= opts.QuitAfter)
        {
            Say($"stopping abruptly after {moves} moves in game {autoGame}");
            OS.Kill(OS.GetProcessId());
        }
    }

    void AutoplayMessage(ServerMessage m)
    {
        if (!opts.Autoplay)
        {
            return;
        }
        switch (m.BodyCase)
        {
            case ServerMessage.BodyOneofCase.AuthOk:
                Say($"signed in as {m.AuthOk.Username}");
                break;
            case ServerMessage.BodyOneofCase.Error when me == null:
                if (m.Error.Code == ErrorCode.UsernameTaken)
                {
                    Send(new ClientMessage { Login = new Login { Username = opts.User, Password = opts.Password } });
                }
                else if (settings.Token == "" && authSent)
                {
                    Say($"cannot sign in: {m.Error.Message}");
                    GetTree().Quit(2);
                }
                break;
            case ServerMessage.BodyOneofCase.Error:
                Say($"server error: {m.Error.Code} {m.Error.Message}");
                break;
            case ServerMessage.BodyOneofCase.GameList when !decided:
                decided = true;
                var resume = m.GameList.Mine.FirstOrDefault(g => g.Status is GameStatus.Active or GameStatus.Waiting);
                if (resume != null)
                {
                    autoGame = resume.GameId;
                    Say($"resuming game {autoGame} at {string.Join("–", resume.Scores)}");
                }
                else if (opts.Mode == "vs")
                {
                    Say($"starting a game against {opts.Vs}");
                    CreateGame(opts.Vs);
                }
                else if (opts.Mode == "invite")
                {
                    Say($"inviting {opts.Invite}");
                    Send(new ClientMessage { SendInvite = new SendInvite { Username = opts.Invite } });
                }
                else if (opts.Mode == "open")
                {
                    Say("opening a game");
                    CreateGame("");
                }
                break;
            case ServerMessage.BodyOneofCase.GameUpdate when autoGame == null && decided:
                if (m.GameUpdate.Game.Status != GameStatus.Finished)
                {
                    autoGame = m.GameUpdate.Game.GameId;
                    Say($"playing game {autoGame}");
                }
                break;
        }
    }

    static void Say(string s) => GD.Print("autoplay: " + s);
}
