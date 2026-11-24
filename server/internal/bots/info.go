package bots

// Info describes a bot to players: how the clients label it in the lobby,
// on the table and in the game history.
type Info struct {
	Name  string `json:"name"`
	Label string `json:"label"`           // e.g. "Expert (medium)"
	Blurb string `json:"blurb,omitempty"` // one short line, e.g. "A lighter search"
	Icon  string `json:"icon,omitempty"`  // a single character the web client shows beside the label
}

var infos = map[string]Info{
	"random":     {Label: "Random bot", Blurb: "For practice", Icon: "⚄"},
	"heuristic":  {Label: "Heuristic bot", Blurb: "Plays the expected value", Icon: "♞"},
	"heuristic2": {Label: "Heuristic bot (strong)", Blurb: "Looks ahead and plays to win", Icon: "♘"},
}

// Describe sets how a bot is shown to players. Call it during startup,
// alongside Register.
func Describe(name string, i Info) {
	i.Name = name
	infos[name] = i
}

// InfoOf returns how the named bot is shown to players. A bot nobody
// described is labelled by its name.
func InfoOf(name string) Info {
	i, ok := infos[name]
	if !ok {
		return Info{Name: name, Label: name + " bot"}
	}
	i.Name = name
	return i
}

// Infos describes every registered bot, sorted by name.
func Infos() []Info {
	names := Names()
	out := make([]Info, len(names))
	for k, n := range names {
		out[k] = InfoOf(n)
	}
	return out
}
