using System;
using System.Collections.Generic;
using Godot;

namespace Cribbage.Ui;

// Sfx plays short effects synthesized at startup, so there are no sound
// files. The recipes are the web client's: enveloped tones and band-passed
// noise.
public static class Sfx
{
    const int Rate = 22050;

    static readonly Dictionary<string, AudioStreamWav> sounds = new();
    static readonly List<AudioStreamPlayer> players = new();
    public static bool Enabled = true;

    public static void Init(Node host)
    {
        if (sounds.Count == 0)
        {
            Build();
        }
        players.Clear();
        for (var i = 0; i < 8; i++)
        {
            var p = new AudioStreamPlayer { VolumeDb = -4 };
            host.AddChild(p);
            players.Add(p);
        }
    }

    public static void Play(string name)
    {
        if (!Enabled || !sounds.TryGetValue(name, out var s))
        {
            return;
        }
        foreach (var p in players)
        {
            if (!p.Playing)
            {
                p.Stream = s;
                p.Play();
                return;
            }
        }
    }

    static void Build()
    {
        Add("card", b => { b.Noise(0, .08, .5, 2200); b.Tone(160, 0, .05, true, .06); });
        Add("deal", b => { for (var i = 0; i < 6; i++) b.Noise(i * .07, .06, .4, 2000 + i * 150); });
        Add("peg", b => { b.Tone(820, 0, .07, true, .16, 420); b.Noise(0, .025, .3, 3500); });
        Add("score", b => { double[] f = { 660, 880, 1175 }; for (var i = 0; i < 3; i++) b.Tone(f[i], i * .075, .16, true, .09); });
        Add("go", b => b.Tone(300, 0, .2, false, .12, 220));
        Add("turn", b => { b.Tone(988, 0, .14, false, .07); b.Tone(1319, .09, .22, false, .06); });
        Add("win", b =>
        {
            double[] f = { 523, 659, 784, 1047 };
            for (var i = 0; i < 4; i++) b.Tone(f[i], i * .12, .3, true, .11);
            for (var i = 0; i < 3; i++) b.Tone(f[i], .5, 1.1, false, .07);
        });
        Add("lose", b => { double[] f = { 440, 392, 349, 294 }; for (var i = 0; i < 4; i++) b.Tone(f[i], i * .18, .35, true, .09); });
    }

    static void Add(string name, Action<Buf> make)
    {
        var b = new Buf();
        make(b);
        sounds[name] = b.Stream();
    }

    sealed class Buf
    {
        float[] d = new float[Rate];
        int len;
        static readonly Random Rng = new(1);

        Span<float> Span(double at, double dur)
        {
            var end = (int)((at + dur + .02) * Rate);
            if (end > d.Length)
            {
                Array.Resize(ref d, end * 2);
            }
            len = Math.Max(len, end);
            return d.AsSpan(0, end);
        }

        // Tone adds a sine or triangle wave at f Hz, sliding exponentially
        // to slide Hz, that rises to vol in 6 ms and decays exponentially.
        public void Tone(double f, double at, double dur, bool triangle, double vol, double slide = 0)
        {
            var s = Span(at, dur);
            var start = (int)(at * Rate);
            var n = (int)(dur * Rate);
            var phase = 0.0;
            for (var i = 0; i < n; i++)
            {
                var t = (double)i / Rate;
                var freq = slide > 0 ? f * Math.Pow(slide / f, t / dur) : f;
                phase += freq / Rate;
                var x = phase - Math.Floor(phase);
                var w = triangle ? 4 * Math.Abs(x - .5) - 1 : Math.Sin(2 * Math.PI * x);
                var g = t < .006 ? vol * t / .006 : vol * Math.Pow(.0001 / vol, (t - .006) / (dur - .006));
                s[start + i] += (float)(w * g);
            }
        }

        // Noise adds white noise through a band-pass filter at freq Hz
        // (Q 1.2), decaying from vol.
        public void Noise(double at, double dur, double vol, double freq)
        {
            var s = Span(at, dur);
            var start = (int)(at * Rate);
            var n = (int)(dur * Rate);
            var w0 = 2 * Math.PI * freq / Rate;
            var alpha = Math.Sin(w0) / (2 * 1.2);
            var a0 = 1 + alpha;
            double b0 = alpha / a0, b2 = -alpha / a0, a1 = -2 * Math.Cos(w0) / a0, a2 = (1 - alpha) / a0;
            double x1 = 0, x2 = 0, y1 = 0, y2 = 0;
            for (var i = 0; i < n; i++)
            {
                var x = Rng.NextDouble() * 2 - 1;
                var y = b0 * x + b2 * x2 - a1 * y1 - a2 * y2;
                x2 = x1; x1 = x; y2 = y1; y1 = y;
                var g = vol * Math.Pow(.0001 / vol, (double)i / n);
                s[start + i] += (float)(y * g);
            }
        }

        public AudioStreamWav Stream()
        {
            var bytes = new byte[len * 2];
            for (var i = 0; i < len; i++)
            {
                var v = (short)Math.Clamp(d[i] * 32767, -32768, 32767);
                bytes[2 * i] = (byte)v;
                bytes[2 * i + 1] = (byte)(v >> 8);
            }
            return new AudioStreamWav { Format = AudioStreamWav.FormatEnum.Format16Bits, MixRate = Rate, Stereo = false, Data = bytes };
        }
    }
}
