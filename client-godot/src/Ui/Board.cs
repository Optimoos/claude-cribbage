using Godot;

namespace Cribbage.Ui;

// Board draws a cribbage board: three streets of 40 holes for each player,
// with a front peg at the score and a back peg at the score before it.
public partial class Board : Control
{
    const int PerRow = 40, Rows = 3;
    const float Step = 15, GroupGap = 7, Lane = 13, RowGap = 14, Margin = 34;

    readonly int[] score = new int[2], back = new int[2];
    readonly Color[] colors = { Style.Mine, Style.Theirs };
    int mine;

    public Board()
    {
        var w = Margin * 2 + Step * (PerRow - 1) + GroupGap * (PerRow / 5 - 1);
        var h = 16 + Rows * (2 * Lane) + (Rows - 1) * RowGap + 16;
        CustomMinimumSize = new Vector2(w, h);
    }

    // Set shows seat `me`'s pegs in the mine colour on the lower lane of
    // each street.
    public void Set(int me, int[] scores, int[] trail)
    {
        mine = me;
        for (var s = 0; s < 2; s++)
        {
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
        var rect = new Rect2((Size.X - CustomMinimumSize.X) / 2, 0, CustomMinimumSize.X, CustomMinimumSize.Y);
        Style.Box(Style.Wood, 12, Style.WoodDark, 2).Draw(GetCanvasItem(), rect);
        var font = ThemeDB.FallbackFont;
        for (var row = 0; row < Rows; row++)
        {
            var y = 16 + row * (2 * Lane + RowGap) - 4;
            DrawString(font, new Vector2(rect.Position.X + 6, y + 2 * Lane - 2), $"{row * PerRow}",
                HorizontalAlignment.Left, -1, 10, new Color(1, 1, 1, 0.45f));
        }
        var hole = new Color(0.12f, 0.07f, 0.03f);
        for (var s = 0; s < 2; s++)
        {
            for (var n = 1; n <= 120; n++)
            {
                DrawCircle(Hole(s, n), 2.6f, hole);
            }
            DrawCircle(Hole(s, 0), 3.2f, hole);
        }
        DrawCircle(Hole(0, 121), 5, hole);

        for (var s = 0; s < 2; s++)
        {
            if (back[s] != score[s])
            {
                Peg(Hole(s, back[s]), colors[s == mine ? 0 : 1].Darkened(0.25f));
            }
            Peg(Hole(s, score[s]), colors[s == mine ? 0 : 1]);
        }
    }

    void Peg(Vector2 at, Color c)
    {
        DrawCircle(at + new Vector2(1, 1.5f), 5.5f, new Color(0, 0, 0, 0.4f));
        DrawCircle(at, 5.5f, c);
        DrawCircle(at + new Vector2(-1.5f, -1.5f), 1.8f, c.Lightened(0.5f));
    }
}
