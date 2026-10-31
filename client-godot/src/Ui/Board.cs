using Godot;

namespace Cribbage.Ui;

// Board draws a cribbage board: three streets of 40 holes for each player,
// with a front peg at the score and a back peg at the score before it.
// When a score changes the back peg leapfrogs to the new hole.
public partial class Board : Control
{
    const int PerRow = 40, Rows = 3;
    const float Step = 14, GroupGap = 6, Lane = 13, RowGap = 14, Margin = 34;

    readonly int[] score = new int[2], back = new int[2];
    // Where each player's moving peg is drawn, and how far through its hop.
    readonly int[] from = new int[2];
    readonly float[] hop = { 1, 1 };
    int mine;
    string game = "";

    public Board()
    {
        var w = Margin * 2 + Step * (PerRow - 1) + GroupGap * (PerRow / 5 - 1);
        var h = 16 + Rows * (2 * Lane) + (Rows - 1) * RowGap + 16;
        CustomMinimumSize = new Vector2(w, h);
    }

    // Set shows seat `me`'s pegs on the lower lane of each street. With
    // animate, a peg that moved hops from its old hole.
    public void Set(string gameId, int me, int[] scores, int[] trail, bool animate)
    {
        var same = gameId == game;
        game = gameId;
        mine = me;
        for (var s = 0; s < 2; s++)
        {
            var moved = same && scores[s] != score[s];
            if (moved && animate)
            {
                from[s] = back[s] == trail[s] ? score[s] : back[s];
                var seat = s;
                hop[s] = 0;
                var tw = CreateTween();
                tw.TweenMethod(Callable.From<float>(t => { hop[seat] = t; QueueRedraw(); }), 0f, 1f, .45)
                    .SetTrans(Tween.TransitionType.Sine).SetEase(Tween.EaseType.InOut);
                Sfx.Play("peg");
            }
            else if (!animate || !same)
            {
                hop[s] = 1;
            }
            score[s] = scores[s];
            back[s] = trail[s];
        }
        QueueRedraw();
    }

    Vector2 Hole(int seat, int n)
    {
        var lane = seat == mine ? 1 : 0;
        var left = (Size.X - CustomMinimumSize.X) / 2;
        if (n <= 0)
        {
            return new Vector2(left + 12, 16 + lane * Lane + 6);
        }
        if (n >= 121)
        {
            return new Vector2(left + CustomMinimumSize.X - 12, 16 + (Rows * 2 * Lane + (Rows - 1) * RowGap) / 2);
        }
        var i = n - 1;
        var row = i / PerRow;
        var col = i % PerRow;
        var x = left + Margin + col * Step + col / 5 * GroupGap;
        var y = 16 + row * (2 * Lane + RowGap) + lane * Lane + 6;
        return new Vector2(x, y);
    }

    public override void _Draw()
    {
        var p = Style.P;
        var rect = new Rect2((Size.X - CustomMinimumSize.X) / 2, 0, CustomMinimumSize.X, CustomMinimumSize.Y);
        Style.Box(p.Wood, 12, p.WoodDark, 2).Draw(GetCanvasItem(), rect);
        var font = ThemeDB.FallbackFont;
        for (var row = 1; row < Rows; row++)
        {
            var y = 16 + row * (2 * Lane + RowGap) - 4;
            DrawString(font, new Vector2(rect.Position.X + 6, y + 2 * Lane - 2), $"{row * PerRow}",
                HorizontalAlignment.Left, -1, 10, p.BoardInk with { A = 0.6f });
        }
        for (var s = 0; s < 2; s++)
        {
            for (var n = 1; n <= 120; n++)
            {
                DrawCircle(Hole(s, n), 2.6f, p.Hole);
            }
            DrawCircle(Hole(s, 0), 3.2f, p.Hole);
        }
        DrawCircle(Hole(0, 121), 5, p.Hole);

        for (var s = 0; s < 2; s++)
        {
            var c = s == mine ? p.Mine : p.Theirs;
            if (hop[s] < 1)
            {
                // The peg that was behind jumps over the other one.
                var a = Hole(s, from[s]);
                var b = Hole(s, score[s]);
                var at = a.Lerp(b, hop[s]) + new Vector2(0, -18 * Mathf.Sin(hop[s] * Mathf.Pi));
                if (back[s] != score[s])
                {
                    Peg(Hole(s, back[s]), c.Darkened(0.25f));
                }
                Peg(at, c);
                continue;
            }
            if (back[s] != score[s])
            {
                Peg(Hole(s, back[s]), c.Darkened(0.25f));
            }
            Peg(Hole(s, score[s]), c);
        }
    }

    void Peg(Vector2 at, Color c)
    {
        DrawCircle(at + new Vector2(1, 1.5f), 5.5f, new Color(0, 0, 0, 0.4f));
        DrawCircle(at, 5.5f, c);
        DrawCircle(at + new Vector2(-1.5f, -1.5f), 1.8f, c.Lightened(0.5f));
    }
}
