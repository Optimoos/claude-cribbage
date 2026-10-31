using Godot;

namespace Cribbage.Ui;

// Palette is one colour theme; the three match the web client's.
public sealed record Palette(
    string Name, string Label,
    Color Page, Color Felt, Color Panel, Color Line, Color Ink, Color Muted,
    Color Accent, Color AccentInk, Color Mine, Color MineInk, Color Theirs, Color TheirsInk,
    Color Button, Color ButtonHover,
    Color Wood, Color WoodDark, Color Hole, Color BoardInk, Color Back, Color BackEdge);

// Style holds the current palette and builds the theme and common controls.
public static class Style
{
    public static readonly Palette[] Palettes =
    {
        new("felt", "Card room",
            new("#0d2a1d"), new("#1d5c3b"), new("#10261b"), new("#2f6d4a"), new("#eaf5ec"), new("#a9cdb3"),
            new("#e8c547"), new("#3a2a00"), new("#e8c547"), new("#3a2a00"), new("#d8483e"), new("#ffffff"),
            new("#24533a"), new("#2d6847"),
            new("#6b4423"), new("#4a2e17"), new("#2a170a"), new("#f8e9cc"), new("#7a1f2b"), new("#f3e6c8")),
        new("parchment", "Parchment",
            new("#e9e1cf"), new("#f4efe4"), new("#fffdf8"), new("#ddd3bf"), new("#23201b"), new("#7b7366"),
            new("#2e6b3f"), new("#ffffff"), new("#2e6b3f"), new("#ffffff"), new("#c0262d"), new("#ffffff"),
            new("#fffdf8"), new("#efe7d6"),
            new("#c89a64"), new("#8a5a2b"), new("#5b3a1a"), new("#4a2f12"), new("#2e6b3f"), new("#ffffff")),
        new("midnight", "Midnight",
            new("#0f131b"), new("#111824"), new("#141a24"), new("#263041"), new("#e6edf3"), new("#8b98a9"),
            new("#5eead4"), new("#062a25"), new("#38bdf8"), new("#04202e"), new("#f472b6"), new("#2e0619"),
            new("#1b2330"), new("#263041"),
            new("#1b2433"), new("#131a26"), new("#05070b"), new("#8b98a9"), new("#1e2a4a"), new("#c8d0dc")),
    };

    public static Palette P { get; private set; } = Palettes[0];

    public static readonly Color CardRed = new("#c3242b");

    public static void Use(string name)
    {
        foreach (var p in Palettes)
        {
            if (p.Name == name)
            {
                P = p;
            }
        }
    }

    public static Theme Build()
    {
        var t = new Theme { DefaultFontSize = 16 };
        t.SetColor("font_color", "Label", P.Ink);
        foreach (var type in new[] { "Button", "OptionButton", "CheckBox" })
        {
            t.SetColor("font_color", type, P.Ink);
            t.SetColor("font_hover_color", type, P.Ink);
            t.SetColor("font_pressed_color", type, P.Ink);
            t.SetColor("font_focus_color", type, P.Ink);
            t.SetColor("font_hover_pressed_color", type, P.Ink);
            t.SetColor("font_disabled_color", type, P.Muted with { A = 0.6f });
            t.SetStylebox("focus", type, new StyleBoxEmpty());
        }
        foreach (var type in new[] { "Button", "OptionButton" })
        {
            t.SetStylebox("normal", type, Box(P.Button, 6, P.Line));
            t.SetStylebox("hover", type, Box(P.ButtonHover, 6, P.Accent with { A = 0.6f }));
            t.SetStylebox("pressed", type, Box(P.ButtonHover, 6, P.Accent));
            t.SetStylebox("disabled", type, Box(P.Button with { A = 0.5f }, 6, P.Line with { A = 0.5f }));
        }
        foreach (var s in new[] { "normal", "hover", "pressed", "hover_pressed" })
        {
            t.SetStylebox(s, "CheckBox", new StyleBoxEmpty());
        }
        t.SetStylebox("normal", "LineEdit", Box(P.Page, 4, P.Line));
        t.SetStylebox("focus", "LineEdit", Box(P.Page, 4, P.Accent));
        t.SetColor("font_color", "LineEdit", P.Ink);
        t.SetColor("font_placeholder_color", "LineEdit", P.Muted);
        t.SetStylebox("panel", "PanelContainer", Box(P.Panel, 8, P.Line));
        t.SetStylebox("panel", "PopupPanel", Box(P.Panel, 8, P.Line));
        t.SetStylebox("panel", "PopupMenu", Box(P.Panel, 6, P.Line));
        t.SetColor("font_color", "PopupMenu", P.Ink);
        t.SetColor("font_hover_color", "PopupMenu", P.Accent);
        t.SetStylebox("hover", "PopupMenu", Box(P.ButtonHover, 4));
        t.SetColor("default_color", "RichTextLabel", P.Ink);
        t.SetColor("font_color", "TooltipLabel", P.Ink);
        t.SetStylebox("panel", "TooltipPanel", Box(P.Panel, 6, P.Line));
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
        b.AddThemeStyleboxOverride("normal", Box(P.Accent, 6));
        b.AddThemeStyleboxOverride("hover", Box(P.Accent.Lightened(0.15f), 6));
        b.AddThemeStyleboxOverride("pressed", Box(P.Accent.Darkened(0.15f), 6));
        b.AddThemeStyleboxOverride("disabled", Box(P.Accent with { A = 0.35f }, 6));
        foreach (var c in new[] { "font_color", "font_hover_color", "font_pressed_color", "font_focus_color" })
        {
            b.AddThemeColorOverride(c, P.AccentInk);
        }
        b.AddThemeColorOverride("font_disabled_color", P.AccentInk with { A = 0.6f });
        return b;
    }

    public static Control Expand() => new Control { SizeFlagsHorizontal = Control.SizeFlags.ExpandFill };
}
