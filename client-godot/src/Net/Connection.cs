using System;
using System.Collections.Generic;
using System.Text.Json;
using System.Text.Json.Nodes;
using Cribbage.Protocol;
using Godot;
using Google.Protobuf;

namespace Cribbage.Net;

// Connection reaches the server over WebRTC: a WebSocket carries the SDP
// offer/answer and ICE candidates, then one data channel carries binary
// protobuf messages. It reconnects on its own after a failure. Call Poll
// every frame; events fire from Poll.
public sealed class Connection
{
    public event System.Action? Opened;
    public event System.Action? Closed;
    public event System.Action<ServerMessage>? Received;

    readonly string url;
    readonly Godot.Collections.Array iceServers;

    WebSocketPeer? ws;
    WebRtcPeerConnection? pc;
    WebRtcDataChannel? dc;
    bool open;
    bool offered, described, remoteSet;
    readonly List<string> localPending = new();
    readonly List<(string mid, int index, string candidate)> remotePending = new();

    double retry = 0.5;
    double retryAt = -1;   // when to reconnect, or -1
    double deadline;       // give up if the channel is not open by then
    double disconnectedAt = -1;
    uint nextId;

    public Connection(string url, IEnumerable<string> iceServers)
    {
        this.url = url;
        this.iceServers = new Godot.Collections.Array();
        foreach (var s in iceServers)
        {
            this.iceServers.Add(new Godot.Collections.Dictionary { ["urls"] = new Godot.Collections.Array { s } });
        }
    }

    public bool IsOpen => open;

    static double Now => Time.GetTicksMsec() / 1000.0;

    public void Connect()
    {
        retryAt = -1;
        offered = described = remoteSet = false;
        localPending.Clear();
        remotePending.Clear();

        pc = new WebRtcPeerConnection();
        var err = pc.Initialize(new Godot.Collections.Dictionary { ["iceServers"] = iceServers });
        if (err != Godot.Error.Ok)
        {
            GD.PushError($"WebRTC is unavailable ({err}); is addons/webrtc_native installed?");
            Fail();
            return;
        }
        pc.SessionDescriptionCreated += OnDescription;
        pc.IceCandidateCreated += OnCandidate;
        dc = pc.CreateDataChannel("cribbage", new Godot.Collections.Dictionary { ["ordered"] = true });
        dc.WriteMode = WebRtcDataChannel.WriteModeEnum.Binary;

        ws = new WebSocketPeer();
        if (ws.ConnectToUrl(url) != Godot.Error.Ok)
        {
            Fail();
            return;
        }
        deadline = Now + 15;
    }

    void OnDescription(string type, string sdp)
    {
        pc!.SetLocalDescription(type, sdp);
        Signal(new JsonObject { ["type"] = type, ["sdp"] = sdp });
        described = true;
        foreach (var m in localPending)
        {
            ws!.SendText(m);
        }
        localPending.Clear();
    }

    void OnCandidate(string media, long index, string name)
    {
        var m = new JsonObject
        {
            ["type"] = "candidate",
            ["candidate"] = new JsonObject { ["candidate"] = name, ["sdpMid"] = media, ["sdpMLineIndex"] = index },
        }.ToJsonString();
        if (described && ws?.GetReadyState() == WebSocketPeer.State.Open)
        {
            ws.SendText(m);
        }
        else
        {
            localPending.Add(m);
        }
    }

    void Signal(JsonObject m) => ws?.SendText(m.ToJsonString());

    public void Poll()
    {
        if (retryAt >= 0)
        {
            if (Now >= retryAt)
            {
                Connect();
            }
            return;
        }
        if (pc == null)
        {
            return;
        }
        PollSignaling();
        if (pc == null)
        {
            return; // signaling failed
        }
        pc.Poll();
        Deliver();
        if (pc == null)
        {
            return; // closed by a handler
        }

        // "Failed" comes long after "disconnected" when the server goes away
        // (e.g. a restart), so give up on a disconnect that lasts.
        var state = pc.GetConnectionState();
        if (state is WebRtcPeerConnection.ConnectionState.Failed or WebRtcPeerConnection.ConnectionState.Closed)
        {
            Fail();
            return;
        }
        if (state == WebRtcPeerConnection.ConnectionState.Disconnected)
        {
            if (disconnectedAt < 0)
            {
                disconnectedAt = Now;
            }
            else if (Now - disconnectedAt > 1.5)
            {
                Fail();
                return;
            }
        }
        else
        {
            disconnectedAt = -1;
        }

        var ready = dc!.GetReadyState();
        if (!open)
        {
            if (ready == WebRtcDataChannel.ChannelState.Open)
            {
                open = true;
                retry = 0.5;
                ws?.Close();
                ws = null;
                Opened?.Invoke();
            }
            else if (Now > deadline)
            {
                Fail();
                return;
            }
        }
        else if (ready is WebRtcDataChannel.ChannelState.Closed or WebRtcDataChannel.ChannelState.Closing)
        {
            Fail();
            return;
        }
    }

    // Deliver reads the messages waiting on the data channel. It runs
    // before the checks for a closed channel, so a message the server sends
    // just before closing (such as UPGRADE_REQUIRED) is not lost.
    void Deliver()
    {
        while (open && dc != null && dc.GetAvailablePacketCount() > 0)
        {
            var data = dc.GetPacket();
            ServerMessage m;
            try
            {
                m = ServerMessage.Parser.ParseFrom(data);
            }
            catch (InvalidProtocolBufferException e)
            {
                GD.PushWarning($"bad message from server: {e.Message}");
                continue;
            }
            Received?.Invoke(m);
        }
    }

    void PollSignaling()
    {
        if (ws == null)
        {
            return;
        }
        ws.Poll();
        switch (ws.GetReadyState())
        {
            case WebSocketPeer.State.Open:
                if (!offered)
                {
                    // OnDescription sends the offer once it is ready.
                    offered = true;
                    pc!.CreateOffer();
                }
                while (ws.GetAvailablePacketCount() > 0)
                {
                    OnSignal(ws.GetPacket().GetStringFromUtf8());
                }
                break;
            case WebSocketPeer.State.Closed:
                // The server closes signaling once its side of the data
                // channel opens, which may be before ours does.
                if (!remoteSet)
                {
                    Fail();
                }
                ws = null;
                break;
        }
    }

    void OnSignal(string text)
    {
        JsonNode? m;
        try
        {
            m = JsonNode.Parse(text);
        }
        catch (JsonException)
        {
            return;
        }
        switch ((string?)m?["type"])
        {
            case "answer":
                pc!.SetRemoteDescription("answer", (string?)m!["sdp"] ?? "");
                remoteSet = true;
                foreach (var (m0, i0, c0) in remotePending)
                {
                    pc.AddIceCandidate(m0, i0, c0);
                }
                remotePending.Clear();
                break;
            case "candidate":
                var c = m!["candidate"];
                if (c == null)
                {
                    break;
                }
                var mid = (string?)c["sdpMid"] ?? "0";
                var index = (int?)c["sdpMLineIndex"] ?? 0;
                var cand = (string?)c["candidate"] ?? "";
                if (remoteSet)
                {
                    pc!.AddIceCandidate(mid, index, cand);
                }
                else
                {
                    remotePending.Add((mid, index, cand));
                }
                break;
        }
    }

    void Fail()
    {
        var wasOpen = open;
        open = false;
        ws?.Close();
        ws = null;
        dc?.Close();
        dc = null;
        pc?.Close();
        pc = null;
        disconnectedAt = -1;
        retryAt = Now + retry;
        retry = Math.Min(retry * 2, 2);
        if (wasOpen)
        {
            Closed?.Invoke();
        }
    }

    // Close drops the connection without reconnecting.
    public void Close()
    {
        Fail();
        retryAt = -1;
    }

    // Send sends m with a fresh request id and the protocol version, and
    // returns the id, or 0 when the connection is not open.
    public uint Send(ClientMessage m)
    {
        if (!open || dc == null)
        {
            return 0;
        }
        m.RequestId = ++nextId;
        m.ProtocolVersion = (uint)ProtocolVersion.Current;
        dc.PutPacket(m.ToByteArray());
        return m.RequestId;
    }
}
