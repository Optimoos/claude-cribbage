using System.Collections.Generic;
using System.Linq;
using Cribbage.Protocol;

namespace Cribbage;

// Text renders cards, scores and events as the web client does.
public static class Text
{
    static readonly string[] Ranks = { "A", "2", "3", "4", "5", "6", "7", "8", "9", "10", "J", "Q", "K" };
    static readonly string[] Suits = { "♣", "♦", "♥", "♠" };

    public static readonly (string Name, string Label, string Blurb)[] Bots =
    {
        ("expert", "Expert", "Searches and plays to win"),
        ("expert-medium", "Expert (medium)", "A lighter search"),
        ("heuristic2", "Heuristic bot (strong)", "Looks ahead and plays to win"),
        ("expert-easy", "Expert (easy)", "A quick look ahead"),
        ("nn", "Neural network", "Learned by self-play"),
        ("heuristic", "Heuristic bot", "Plays the expected value"),
        ("random", "Random bot", "For practice"),
    };

    public static string BotLabel(string bot)
    {
        foreach (var b in Bots)
        {
            if (b.Name == bot)
            {
                return b.Label;
            }
        }
        return "Computer";
    }

    public static string Rank(uint c) => Ranks[c % 13];
    public static string Suit(uint c) => Suits[c / 13];
    public static string Card(uint c) => Rank(c) + Suit(c);
    public static bool IsRed(uint c) => c / 13 is 1 or 2;
    public static int Value(uint c) => (int)System.Math.Min(c % 13 + 1, 10);

    public static string SeatName(GameSummary g, int seat)
    {
        if (seat == g.Seat)
        {
            return "You";
        }
        return seat >= 0 && seat < g.Players.Count ? g.Players[seat].Nickname : $"Seat {seat}";
    }

    public static string Opponent(GameSummary g)
    {
        var s = 1 - g.Seat;
        if (s < 0 || s >= g.Players.Count || (g.Players[s].Nickname == "" && g.Players[s].Bot == ""))
        {
            return "waiting for opponent";
        }
        return g.Players[s].Nickname;
    }

    public static string Breakdown(ShowScore? b)
    {
        var parts = new List<string>();
        if (b != null)
        {
            if (b.Fifteens > 0) parts.Add($"fifteens {b.Fifteens}");
            if (b.Pairs > 0) parts.Add($"pairs {b.Pairs}");
            if (b.Runs > 0) parts.Add($"runs {b.Runs}");
            if (b.Flush > 0) parts.Add($"flush {b.Flush}");
            if (b.Nobs > 0) parts.Add($"nobs {b.Nobs}");
        }
        return parts.Count > 0 ? string.Join(", ", parts) : "nineteen (nothing)";
    }

    static string PegDetail(PegScore? p)
    {
        var parts = new List<string>();
        if (p != null)
        {
            if (p.Fifteen > 0) parts.Add("fifteen");
            if (p.ThirtyOne > 0) parts.Add("thirty-one");
            if (p.Pairs == 2) parts.Add("pair");
            if (p.Pairs == 6) parts.Add("pair royal");
            if (p.Pairs == 12) parts.Add("double pair royal");
            if (p.Run > 0) parts.Add($"run of {p.Run}");
        }
        return string.Join(" + ", parts);
    }

    public static string Describe(GameSummary g, Event e)
    {
        var who = SeatName(g, e.Seat);
        var s = who == "You" ? "" : "s";
        var whose = who == "You" ? "Your" : who + "'s";
        var cards = string.Join(" ", e.Cards.Select(Card));
        return e.Kind switch
        {
            EventKind.CutForDeal => $"Cut for deal: {SeatName(g, 0)} {Card(e.Cards[0])}, {SeatName(g, 1)} {Card(e.Cards[1])}",
            EventKind.Deal => $"New hand — {who} deal{s}",
            EventKind.Discard => $"{who} discard{s} to the crib",
            EventKind.Starter => $"Starter: {Card(e.Card)}" + (e.Points > 0 ? $" — {who} score{s} 2 for his heels" : ""),
            EventKind.Play => $"{who} play{s} {Card(e.Card)} ({e.Count})" + (e.Points > 0 ? $" — {PegDetail(e.Peg)}" : ""),
            EventKind.Go => $"{who} say{s} go",
            EventKind.GoPoint => $"{who} score{s} 1 for the go",
            EventKind.LastCard => $"{who} score{s} 1 for last card",
            EventKind.CountReset => "Count goes back to 0",
            EventKind.ShowHand => $"{whose} hand {cards} + {Card(e.Card)}: {Breakdown(e.Show)}",
            EventKind.ShowCrib => $"{whose} crib {cards} + {Card(e.Card)}: {Breakdown(e.Show)}",
            EventKind.GameOver => $"{who} win{s} {e.Scores[e.Seat]}–{e.Scores[1 - e.Seat]}" +
                (e.Skunk == 2 ? " — double skunk!" : e.Skunk == 1 ? " — skunk!" : ""),
            EventKind.Resign => $"{who} resign{s}",
            _ => e.Kind.ToString(),
        };
    }

    public static string GameOver(GameSummary g)
    {
        if (g.Winner < 0)
        {
            return "Game cancelled.";
        }
        var won = g.Winner == g.Seat;
        return $"{(won ? "You won" : "You lost")} {g.Scores[g.Seat]}–{g.Scores[1 - g.Seat]}{(g.Resigned ? " (resignation)" : "")}.";
    }
}
