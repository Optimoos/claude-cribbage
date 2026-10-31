using System.Collections.Generic;
using System.Linq;
using Cribbage.Protocol;

namespace Cribbage;

// GameState is what the client knows about one game. Update is the latest
// update from the server; the table shows V, which replays each update's
// events one at a time and then settles on the server's observation.
public sealed class GameState
{
    public sealed record Line(string Text, int Points, bool Mine, bool Neutral);

    public GameSummary Game;
    public GameUpdate? Update; // the latest update with an observation
    public readonly List<Line> Log = new();
    public View V = new();

    public GameState(GameSummary g)
    {
        Game = g;
        V.Reset(g);
    }

    public Observation? Obs => Update?.Observation;
    public int Me => Game.Seat < 0 ? 0 : Game.Seat;
    public bool Finished => Game.Status == GameStatus.Finished;

    // Received records u as the latest state; the view catches up as it
    // replays.
    public void Received(GameUpdate u)
    {
        Game = u.Game;
        if (u.Observation != null)
        {
            Update = u;
        }
    }

    // Restart makes the view the server's state with nothing to replay,
    // as after a reconnect.
    public void Restart(GameUpdate u)
    {
        V = new View();
        V.Reset(u.Game);
        V.Sync(u);
    }

    // Apply replays one event onto the view, logs it, and returns what to
    // show for it.
    public Fx Apply(Event e, GameUpdate u)
    {
        var g = u.Game;
        var v = V;
        var me = Me;
        var mine = e.Seat == me;
        var fx = new Fx();
        var neutral = e.Kind is EventKind.Deal or EventKind.CountReset or EventKind.CutForDeal;
        Log.Add(new Line(Text.Describe(g, e), e.Points, mine, neutral));
        if (e.Scores.Count == 2)
        {
            for (var s = 0; s < 2; s++)
            {
                if (e.Scores[s] != v.Scores[s])
                {
                    fx.Scored.Add((s, e.Scores[s] - v.Scores[s]));
                    v.Trail[s] = v.Scores[s];
                    v.Scores[s] = e.Scores[s];
                }
            }
        }
        switch (e.Kind)
        {
            case EventKind.CutForDeal:
                fx.Ms = 1300;
                fx.Note = $"Cut: {Text.Card(e.Cards[me == 1 ? 1 : 0])} vs {Text.Card(e.Cards[me == 1 ? 0 : 1])}";
                fx.At = "deck";
                break;
            case EventKind.Deal:
                v.Dealer = e.Seat;
                v.Phase = Phase.Discard;
                v.Showing = false;
                v.Played.Clear();
                v.SeqStart = v.Count = 0;
                v.Starter = null;
                v.Discarded = v.OppDiscarded = false;
                v.MyDiscards.Clear();
                v.OppReveal = v.CribOpen = null;
                v.Hand = DealtHand(u);
                v.OppHandSize = 6;
                v.Legal.Clear();
                v.MustAct = false;
                v.Fresh.UnionWith(v.Hand);
                v.FreshOpp = true;
                fx.Sound = "deal";
                fx.Ms = 1000;
                break;
            case EventKind.Discard:
                if (mine)
                {
                    var d = u.Observation?.Discards.ToList() ?? new List<uint>();
                    v.Hand.RemoveAll(d.Contains);
                    v.MyDiscards = d;
                    v.Discarded = true;
                }
                else
                {
                    v.OppDiscarded = true;
                    v.OppHandSize = System.Math.Max(0, v.OppHandSize - 2);
                }
                v.FreshCrib = true;
                fx.Sound = "card";
                fx.Ms = mine ? 300 : 650;
                break;
            case EventKind.Starter:
                v.Starter = e.Card;
                v.Phase = Phase.Pegging;
                v.Shows.Clear();
                v.FreshStarter = true;
                fx.Sound = "card";
                fx.Ms = 1100;
                if (e.Points > 0)
                {
                    fx.Label = "his heels";
                    fx.At = "deck";
                    fx.Ms = 1600;
                }
                break;
            case EventKind.Play:
                v.Played.Add(new PlayedCard { Card = e.Card, Seat = e.Seat });
                v.Count = e.Count;
                if (mine)
                {
                    v.Hand.Remove(e.Card);
                }
                else
                {
                    v.OppHandSize = System.Math.Max(0, v.OppHandSize - 1);
                }
                v.Fresh.Add(e.Card);
                v.Hot = e.Count is 15 or 31;
                fx.Sound = "card";
                fx.Ms = mine ? 500 : 950;
                if (e.Points > 0)
                {
                    fx.Label = Text.PegDetail(e.Peg);
                    fx.At = "play";
                    fx.Ms += 600;
                }
                break;
            case EventKind.Go:
                fx.Note = $"{(mine ? "You" : "They")} say “Go”";
                fx.At = "play";
                fx.Sound = "go";
                fx.Ms = 900;
                break;
            case EventKind.GoPoint:
                fx.Label = "for the go";
                fx.At = "play";
                fx.Ms = 900;
                break;
            case EventKind.LastCard:
                fx.Label = "last card";
                fx.At = "play";
                fx.Ms = 900;
                break;
            case EventKind.CountReset:
                v.SeqStart = v.Played.Count;
                v.Count = 0;
                fx.Ms = 550;
                break;
            case EventKind.ShowHand:
            case EventKind.ShowCrib:
                var crib = e.Kind == EventKind.ShowCrib;
                if (!v.Showing)
                {
                    v.Showing = true;
                    v.Shows.Clear();
                }
                v.Shows.Add(e);
                v.FreshShows.Add(e);
                if (crib)
                {
                    v.CribOpen = e.Cards.ToList();
                }
                else if (!mine)
                {
                    v.OppReveal = e.Cards.ToList();
                }
                fx.Label = crib ? "crib" : "hand";
                fx.Ms = 330;
                break;
            case EventKind.GameOver:
                v.Over = (e.Seat, false);
                v.OverFresh = true;
                fx.Ms = 0;
                break;
            case EventKind.Resign:
                v.Over = (1 - e.Seat, true);
                v.OverFresh = true;
                fx.Ms = 0;
                break;
        }
        return fx;
    }

    static List<uint> DealtHand(GameUpdate u)
    {
        var o = u.Observation;
        if (o == null)
        {
            return new List<uint>();
        }
        return o.Discarded ? o.Kept.Concat(o.Discards).ToList() : o.Hand.ToList();
    }
}

// Fx is what to show for one replayed event, and how long to pause after.
public sealed class Fx
{
    public int Ms = 600;
    public readonly List<(int Seat, int Points)> Scored = new();
    public string Label = "";
    public string At = "plate"; // where points float up: plate, deck or play
    public string? Sound;
    public string? Note;
}

// View is the table as currently shown.
public sealed class View
{
    public GameSummary Game = new();
    public Phase Phase;
    public bool Showing; // replaying the show
    public int HandNumber, Dealer = -1;
    public List<uint> Hand = new();
    public int OppHandSize;
    public bool Discarded, OppDiscarded;
    public List<uint> MyDiscards = new();
    public uint? Starter;
    public readonly List<PlayedCard> Played = new();
    public int SeqStart, Count;
    public readonly List<Event> Shows = new();
    public List<uint>? OppReveal, CribOpen;
    public readonly int[] Scores = new int[2], Trail = new int[2];
    public bool MustAct, Busy;
    public readonly List<Action> Legal = new();
    public (int Winner, bool Resigned)? Over;
    public bool OverFresh, BannerClosed;

    // One-shot animation hints, cleared after each render.
    public readonly HashSet<uint> Fresh = new();
    public readonly HashSet<Event> FreshShows = new();
    public bool FreshOpp, FreshStarter, FreshCrib, Hot;

    public void Reset(GameSummary g)
    {
        Game = g;
        if (g.Scores.Count == 2)
        {
            for (var s = 0; s < 2; s++)
            {
                Scores[s] = Trail[s] = g.Scores[s];
            }
        }
    }

    public void ClearHints()
    {
        Fresh.Clear();
        FreshShows.Clear();
        FreshOpp = FreshStarter = FreshCrib = Hot = false;
    }

    // Sync makes the view agree with the server's observation.
    public void Sync(GameUpdate u)
    {
        var g = u.Game;
        var o = u.Observation;
        Game = g;
        var scores = o != null ? o.Scores : g.Scores;
        if (scores.Count == 2)
        {
            for (var s = 0; s < 2; s++)
            {
                if (scores[s] != Scores[s])
                {
                    Trail[s] = Scores[s];
                    Scores[s] = scores[s];
                }
            }
        }
        if (g.Status == GameStatus.Finished && Over == null && g.Winner >= 0)
        {
            Over = (g.Winner, g.Resigned);
        }
        Legal.Clear();
        Legal.AddRange(u.Legal);
        if (o == null)
        {
            Phase = Phase.Unspecified;
            MustAct = false;
            return;
        }
        if (o.Phase == Phase.Pegging)
        {
            Shows.Clear();
        }
        if (o.Phase != Phase.GameOver)
        {
            OppReveal = CribOpen = null;
        }
        Showing = false;
        Phase = o.Phase;
        HandNumber = o.HandNumber;
        Dealer = o.Dealer;
        Hand = o.Hand.ToList();
        OppHandSize = o.OppHandSize;
        Discarded = o.Discarded;
        OppDiscarded = o.OppDiscarded;
        MyDiscards = o.Discards.ToList();
        Starter = o.HasStarter ? o.Starter : null;
        Played.Clear();
        Played.AddRange(o.Played);
        SeqStart = o.SeqStart;
        Count = o.Count;
        MustAct = o.MustAct;
    }
}
