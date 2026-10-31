using System.Collections.Generic;
using Cribbage.Protocol;

namespace Cribbage;

// GameState is what the client knows about one game: the latest update
// from the server plus what it has gathered from events (the log, the
// last hand's counts and where each back peg stands).
public sealed class GameState
{
    public sealed record Line(string Text, int Points, bool Mine);

    public GameSummary Game;
    public GameUpdate? Update; // the latest update with an observation
    public readonly int[] Scores = new int[2], Trail = new int[2];
    public readonly List<Line> Log = new();
    public readonly List<Event> Shows = new(); // the last hand's counts

    public GameState(GameSummary g)
    {
        Game = g;
        SetScores(g.Scores, true);
    }

    public Observation? Obs => Update?.Observation;
    public int Me => Game.Seat < 0 ? 0 : Game.Seat;
    public bool Finished => Game.Status == GameStatus.Finished;

    public void Apply(GameUpdate u)
    {
        var first = Update == null;
        Game = u.Game;
        if (u.Observation != null)
        {
            Update = u;
        }
        var o = u.Observation;
        foreach (var e in u.Events)
        {
            var mine = e.Seat == Me;
            var points = e.Kind is EventKind.ShowHand or EventKind.ShowCrib or EventKind.Play or EventKind.Starter
                or EventKind.GoPoint or EventKind.LastCard ? e.Points : 0;
            Log.Add(new Line(Text.Describe(Game, e), points, mine));
            if (e.Kind is EventKind.ShowHand or EventKind.ShowCrib)
            {
                Shows.Add(e);
            }
            if (e.Scores.Count == 2)
            {
                SetScores(e.Scores, false);
            }
        }
        if (o != null && o.Phase == Phase.Pegging)
        {
            Shows.Clear();
        }
        SetScores(o != null ? o.Scores : u.Game.Scores, first && u.Full);
    }

    void SetScores(IList<int> s, bool reset)
    {
        if (s.Count != 2)
        {
            return;
        }
        for (var i = 0; i < 2; i++)
        {
            if (reset)
            {
                Scores[i] = Trail[i] = s[i];
            }
            else if (s[i] != Scores[i])
            {
                Trail[i] = Scores[i];
                Scores[i] = s[i];
            }
        }
    }
}
