using System;
using Godot;

namespace Cribbage.Ui;

// CardFace draws one playing card, face up or (with no card) face down.
public partial class CardFace : Control
{
    public static readonly Vector2 Normal = new(78, 110);
    public static readonly Vector2 Small = new(46, 66);

    uint? card;
    bool selected, dim, clickable, hover;

    public event Action? Clicked;

    public CardFace() : this(null, Normal) { }

    public CardFace(uint? card, Vector2 size)
    {
        this.card = card;
        CustomMinimumSize = size;
        MouseFilter = MouseFilterEnum.Ignore;
    }

    public bool Selected { get => selected; set { selected = value; QueueRedraw(); } }
    public bool Dim { get => dim; set { dim = value; QueueRedraw(); } }

    public bool Clickable
    {
        get => clickable;
        set
        {
            clickable = value;
            MouseFilter = value ? MouseFilterEnum.Stop : MouseFilterEnum.Ignore;
            MouseDefaultCursorShape = value ? CursorShape.PointingHand : CursorShape.Arrow;
            QueueRedraw();
        }
    }

    public override void _Ready()
    {
        MouseEntered += () => { hover = true; QueueRedraw(); };
        MouseExited += () => { hover = false; QueueRedraw(); };
    }

    public override void _GuiInput(InputEvent e)
    {
        if (clickable && e is InputEventMouseButton { ButtonIndex: MouseButton.Left, Pressed: true })
        {
            AcceptEvent();
            Clicked?.Invoke();
        }
    }

    public override void _Draw()
    {
        var size = CustomMinimumSize;
        var lift = selected ? -18f : clickable && hover ? -6f : 0f;
        var rect = new Rect2(new Vector2(0, lift), size);
        var small = size.X < 60;
        var radius = small ? 4 : 7;

        // Shadow, then the card.
        Style.Box(new Color(0, 0, 0, 0.35f), radius).Draw(GetCanvasItem(), new Rect2(rect.Position + new Vector2(1, 3), size));
        if (card is not { } c)
        {
            var back = Style.Box(new Color("#7a1f2b"), radius, new Color("#f4ecd8"), small ? 2 : 3);
            back.Draw(GetCanvasItem(), rect);
            var inner = rect.Grow(small ? -5 : -8);
            DrawRect(inner, new Color("#9a2a38"), false, 1);
            for (var x = inner.Position.X + 4; x < inner.End.X; x += 8)
            {
                DrawLine(new Vector2(x, inner.Position.Y), new Vector2(x, inner.End.Y), new Color(1, 1, 1, 0.07f), 2);
            }
            return;
        }

        var border = selected ? Style.Gold : clickable && hover ? Style.Gold with { A = 0.7f } : new Color("#c9c2b0");
        Style.Box(new Color("#fbf8f0"), radius, border, selected ? 3 : 1).Draw(GetCanvasItem(), rect);
        var ink = Text.IsRed(c) ? Style.Red : new Color("#1d1d1f");
        var font = ThemeDB.FallbackFont;
        var rank = Text.Rank(c);
        var suit = Text.Suit(c);
        if (small)
        {
            DrawString(font, rect.Position + new Vector2(5, 20), rank, HorizontalAlignment.Left, -1, 17, ink);
            DrawString(font, rect.Position + new Vector2(0, 52), suit, HorizontalAlignment.Center, size.X, 24, ink);
        }
        else
        {
            DrawString(font, rect.Position + new Vector2(7, 25), rank, HorizontalAlignment.Left, -1, 22, ink);
            DrawString(font, rect.Position + new Vector2(7, 46), suit, HorizontalAlignment.Left, -1, 20, ink);
            DrawString(font, rect.Position + new Vector2(0, 88), suit, HorizontalAlignment.Center, size.X, 44, ink);
        }
        if (dim)
        {
            Style.Box(new Color(0.05f, 0.12f, 0.08f, 0.55f), radius).Draw(GetCanvasItem(), rect);
        }
    }
}
