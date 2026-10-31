using Godot;

namespace Cribbage.Ui;

// Style holds the palette and builds the theme and common controls.
public static class Style
{
    public static readonly Color Felt = new("#123826");
    public static readonly Color FeltDark = new("#0c2a1c");
    public static readonly Color Panel = new("#0f2219");
    public static readonly Color Ink = new("#f2efe6");
    public static readonly Color Muted = new("#a9bcae");
    public static readonly Color Gold = new("#e8c46a");
    public static readonly Color Red = new("#c8323c");
    public static readonly Color Mine = new("#4fb3ff");
    public static readonly Color Theirs = new("#ff8a5c");
    public static readonly Color Wood = new("#6b4423");
    public static readonly Color WoodDark = new("#4a2e17");

    public static Theme Build()
    {
        var t = new Theme { DefaultFontSize = 16 };
        t.SetColor("font_color", "Label", Ink);
        t.SetColor("font_color", "Button", Ink);
        t.SetColor("font_hover_color", "Button", Colors.White);
        t.SetColor("font_disabled_color", "Button", Muted with { A = 0.5f });
        t.SetStylebox("normal", "Button", Box(new Color("#24533a"), 6, new Color("#2f6d4a")));
        t.SetStylebox("hover", "Button", Box(new Color("#2d6847"), 6, Gold with { A = 0.6f }));
        t.SetStylebox("pressed", "Button", Box(new Color("#1b4430"), 6, Gold));
        t.SetStylebox("disabled", "Button", Box(new Color("#1a3a29"), 6, new Color("#1f4431")));
        t.SetStylebox("focus", "Button", new StyleBoxEmpty());
        t.SetStylebox("normal", "LineEdit", Box(new Color("#0b1c14"), 4, new Color("#2f6d4a")));
        t.SetStylebox("focus", "LineEdit", Box(new Color("#0b1c14"), 4, Gold));
        t.SetColor("font_color", "LineEdit", Ink);
        t.SetStylebox("panel", "PanelContainer", Box(Panel, 8, new Color("#1d3d2c")));
        t.SetColor("default_color", "RichTextLabel", Ink);
        return t;
    }

    public static StyleBoxFlat Box(Color bg, int radius = 6, Color? border = null, int width = 1)
    {
        var b = new StyleBoxFlat { BgColor = bg };
        b.SetCornerRadiusAll(radius);
        if (border is { } c)
        {
            b.BorderColor = c;
            b.SetBorderWidthAll(width);
        }
        b.ContentMarginLeft = b.ContentMarginRight = 10;
        b.ContentMarginTop = b.ContentMarginBottom = 6;
        return b;
    }

    public static Label Label(string text, int size = 16, Color? color = null)
    {
        var l = new Label { Text = text };
        l.AddThemeFontSizeOverride("font_size", size);
        if (color is { } c)
        {
            l.AddThemeColorOverride("font_color", c);
        }
        return l;
    }

    public static Button Button(string text, System.Action onPressed)
    {
        var b = new Button { Text = text };
        b.Pressed += onPressed;
        return b;
    }

    public static Button Primary(string text, System.Action onPressed)
    {
        var b = Button(text, onPressed);
        b.AddThemeStyleboxOverride("normal", Box(Gold, 6));
        b.AddThemeStyleboxOverride("hover", Box(Gold.Lightened(0.15f), 6));
        b.AddThemeStyleboxOverride("pressed", Box(Gold.Darkened(0.15f), 6));
        b.AddThemeColorOverride("font_color", new Color("#1b1b1b"));
        b.AddThemeColorOverride("font_hover_color", new Color("#000000"));
        return b;
    }

    public static Control Gap(int w = 0, int h = 0) => new Control { CustomMinimumSize = new Vector2(w, h) };

    public static Control Expand() => new Control { SizeFlagsHorizontal = Control.SizeFlags.ExpandFill };
}
