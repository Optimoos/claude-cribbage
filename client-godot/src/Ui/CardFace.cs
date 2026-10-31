using System;
using Godot;

namespace Cribbage.Ui;

// CardFace draws one playing card, face up or (with no card) face down.
public partial class CardFace : Control
{
    public static readonly Vector2 Normal = new(78, 110);
    public static readonly Vector2 Small = new(46, 66);

    public enum Entry { DealIn, DealInOpp, FlipIn }

    uint? card;
    bool selected, dim, clickable, hover;
    Vector2 slide;      // drawing offset while dealing in
    float flip = 1;     // horizontal scale while flipping over

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

    // Enter animates the card arriving: dealt in from below (mine) or
    // above (theirs), or flipped face up.
    public void Enter(Entry how, double delay = 0)
    {
        var tw = CreateTween();
        if (how == Entry.FlipIn)
        {
            flip = 0;
            tw.TweenMethod(Callable.From<float>(f => { flip = f; QueueRedraw(); }), 0f, 1f, .55)
                .SetDelay(delay).SetTrans(Tween.TransitionType.Cubic).SetEase(Tween.EaseType.Out);
            return;
        }
        var from = new Vector2(0, how == Entry.DealIn ? 60 : -60);
        slide = from;
        Modulate = Modulate with { A = 0 };
        tw.SetParallel();
        tw.TweenMethod(Callable.From<Vector2>(v => { slide = v; QueueRedraw(); }), from, Vector2.Zero, .38)
            .SetDelay(delay).SetTrans(Tween.TransitionType.Back).SetEase(Tween.EaseType.Out);
        tw.TweenProperty(this, "modulate:a", 1f, .2).SetDelay(delay);
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
        if (flip < 1)
        {
            // Scale about the centre: shows the back for the first half.
            var sx = Math.Abs(1 - 2 * flip);
            DrawSetTransform(new Vector2(size.X * (1 - sx) / 2, 0), 0, new Vector2(Math.Max(sx, .02f), 1));
        }
        var rect = new Rect2(new Vector2(0, lift) + slide, size);
        var small = size.X < 60;
        var radius = small ? 4 : 7;
        var p = Style.P;

        Style.Box(new Color(0, 0, 0, 0.3f), radius).Draw(GetCanvasItem(), new Rect2(rect.Position + new Vector2(1, 3), size));
        if (card is not { } c || flip < .5f)
        {
            Style.Box(p.Back, radius, p.BackEdge, small ? 2 : 3).Draw(GetCanvasItem(), rect);
            var inner = rect.Grow(small ? -5 : -8);
            DrawRect(inner, p.Back.Lightened(.15f), false, 1);
            for (var x = inner.Position.X + 4; x < inner.End.X; x += 8)
            {
                DrawLine(new Vector2(x, inner.Position.Y), new Vector2(x, inner.End.Y), new Color(1, 1, 1, 0.07f), 2);
            }
            return;
        }

        var border = selected ? p.Accent : clickable && hover ? p.Accent with { A = 0.7f } : new Color("#c9c2b0");
        Style.Box(new Color("#fffdf6"), radius, border, selected ? 3 : 1).Draw(GetCanvasItem(), rect);
        var ink = Text.IsRed(c) ? Style.CardRed : new Color("#1d1d1f");
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
            Style.Box(new Color(0.05f, 0.08f, 0.06f, 0.5f), radius).Draw(GetCanvasItem(), rect);
        }
    }
}
