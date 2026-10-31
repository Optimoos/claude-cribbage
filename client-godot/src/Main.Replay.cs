using System.Collections.Generic;
using System.Threading.Tasks;
using Cribbage.Protocol;
using Cribbage.Ui;
using Godot;

namespace Cribbage;

// Replay shows each update's events one at a time, as the web client does:
// cards slide in, pegs hop, points float up, and then the table settles on
// the server's state. Clicking the table skips ahead; in rapid play
// updates apply at once.
public partial class Main
{
    sealed class Waiter
    {
        public double Until;
        public bool Tail;
        public readonly TaskCompletionSource Done = new();
    }

    readonly Queue<GameUpdate> replayQueue = new();
    readonly List<Waiter> waiters = new();
    bool replaying, skipping;

    void Enqueue(GameUpdate u)
    {
        replayQueue.Enqueue(u);
        if (!replaying)
        {
            Drain();
        }
    }

    async void Drain()
    {
        replaying = true;
        while (replayQueue.Count > 0)
        {
            try
            {
                await Replay(replayQueue.Dequeue());
            }
            catch (System.Exception e)
            {
                GD.PushError(e.ToString());
            }
        }
        replaying = false;
        skipping = false;
    }

    // Wait pauses the replay; a tail wait is one the player may end by acting.
    Task Wait(int ms, bool tail = false)
    {
        if (skipping || Rapid || ms <= 0)
        {
            return Task.CompletedTask;
        }
        var w = new Waiter { Until = Now + ms / 1000.0, Tail = tail };
        waiters.Add(w);
        return w.Done.Task;
    }

    void PollWaiters()
    {
        for (var i = waiters.Count - 1; i >= 0; i--)
        {
            if (Now >= waiters[i].Until)
            {
                Release(i);
            }
        }
    }

    void Release(int i)
    {
        var w = waiters[i];
        waiters.RemoveAt(i);
        w.Done.TrySetResult();
    }

    // EndTails cuts short a pause the player may end by acting.
    void EndTails()
    {
        for (var i = waiters.Count - 1; i >= 0; i--)
        {
            if (waiters[i].Tail)
            {
                Release(i);
            }
        }
    }

    // SkipAnimation finishes any replay in progress at once.
    void SkipAnimation()
    {
        if (!replaying)
        {
            return;
        }
        skipping = true;
        for (var i = waiters.Count - 1; i >= 0; i--)
        {
            Release(i);
        }
    }

    async Task Replay(GameUpdate u)
    {
        var id = u.Game.GameId;
        if (!games.TryGetValue(id, out var gs))
        {
            return;
        }
        bool Animated() => !skipping && !Rapid && id == selected;
        var v = gs.V;
        if (u.Full)
        {
            gs.Restart(u);
        }
        else
        {
            var oppActed = false;
            v.Busy = u.Events.Count > 0;
            for (var i = 0; i < u.Events.Count; i++)
            {
                var e = u.Events[i];
                var fx = gs.Apply(e, u);
                if (e.Seat != u.Game.Seat)
                {
                    oppActed = true;
                }
                if (!Animated())
                {
                    continue;
                }
                // When the last event leaves me to act, unlock the hand now
                // rather than after the points fade; acting cuts the pause.
                var unlock = i == u.Events.Count - 1 && u.Observation is { MustAct: true };
                if (unlock)
                {
                    v.Sync(u);
                    v.Busy = false;
                }
                RenderTable(true);
                Effects(gs, fx);
                if (unlock)
                {
                    if (oppActed)
                    {
                        Sfx.Play("turn");
                    }
                    await Wait(fx.Ms, true);
                    return;
                }
                await Wait(fx.Ms);
            }
            v.Sync(u);
            v.Busy = false;
            if (oppActed && v.MustAct && Animated())
            {
                Sfx.Play("turn");
            }
        }
        if (id == selected)
        {
            RenderTable(Animated());
        }
    }

    void Effects(GameState gs, Fx fx)
    {
        if (fx.Sound != null)
        {
            Sfx.Play(fx.Sound);
        }
        foreach (var (seat, pts) in fx.Scored)
        {
            var mine = seat == gs.Me;
            FloatAt(fx.At, seat == gs.Me, $"+{pts}", fx.Label, mine ? Style.P.Mine : Style.P.Theirs, mine ? Style.P.MineInk : Style.P.TheirsInk);
        }
        if (fx.Scored.Count > 0)
        {
            GetTree().CreateTimer(.12).Timeout += () => Sfx.Play("score");
        }
        if (fx.Note != null)
        {
            FloatAt(fx.At, false, fx.Note, "", Style.P.Panel, Style.P.Ink);
        }
    }

    // FloatAt shows a label rising near a player's score, the play area or
    // the deck, stacked above any still rising from the same spot.
    void FloatAt(string at, bool mine, string text, string small, Color bg, Color ink)
    {
        Control anchor = at switch
        {
            "deck" => starterSlot,
            "play" => sequence.GetChildCount() > 0 ? sequence : countLabel,
            _ => mine ? meScore : oppScore,
        };
        var key = $"{at}:{mine}";
        var stacked = 0;
        foreach (var c in fxLayer.GetChildren())
        {
            if (c is Control f && (string)f.GetMeta("key", "") == key)
            {
                stacked++;
            }
        }
        var box = new PanelContainer { MouseFilter = MouseFilterEnum.Ignore };
        box.SetMeta("key", key);
        var sb = Style.Box(bg, 14);
        sb.ShadowColor = new Color(0, 0, 0, .4f);
        sb.ShadowSize = 6;
        box.AddThemeStyleboxOverride("panel", sb);
        var col = new VBoxContainer();
        col.AddThemeConstantOverride("separation", 0);
        box.AddChild(col);
        var big = Style.Label(text, 20, ink);
        big.HorizontalAlignment = HorizontalAlignment.Center;
        col.AddChild(big);
        if (small != "")
        {
            var s = Style.Label(small, 12, ink);
            s.HorizontalAlignment = HorizontalAlignment.Center;
            col.AddChild(s);
        }
        fxLayer.AddChild(box);
        var size = box.GetCombinedMinimumSize();
        var r = anchor.GetGlobalRect();
        var center = r.GetCenter() - fxLayer.GetGlobalRect().Position;
        var x = Mathf.Clamp(center.X - size.X / 2, 4, fxLayer.Size.X - size.X - 4);
        var y = Mathf.Max(center.Y - size.Y / 2, 4) - stacked * (size.Y + 6);
        box.Position = new Vector2(x, y);
        box.Scale = new Vector2(.6f, .6f);
        box.PivotOffset = size / 2;
        var tw = box.CreateTween();
        tw.TweenProperty(box, "scale", Vector2.One, .25).SetTrans(Tween.TransitionType.Back).SetEase(Tween.EaseType.Out);
        tw.Parallel().TweenProperty(box, "position:y", y - 50, 1.6).SetEase(Tween.EaseType.Out);
        tw.Parallel().TweenProperty(box, "modulate:a", 0f, .5).SetDelay(1.1);
        tw.TweenCallback(Callable.From(box.QueueFree));
    }
}
