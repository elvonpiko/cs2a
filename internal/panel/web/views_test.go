package web

import "testing"

// CS2's status output carries no SteamID for anyone, so the row label has to
// fall back to whatever identifying detail the engine does give. The old
// template printed "SteamID · connected · Nms" unconditionally, which rendered
// as a bare " · 0ms" for every CS2 player.
func TestPlayerRowLabel(t *testing.T) {
	cases := []struct {
		name string
		row  PlayerRow
		want string
	}{
		{
			name: "cs2 human has an address but no steamid",
			row:  PlayerRow{Name: "ZUBOR_", Addr: "5.129.143.9:61274", Connected: "00:18", Ping: 49, State: "active"},
			want: "5.129.143.9:61274 · 00:18 · 49 ms",
		},
		{
			name: "legacy row prefers the steamid over the address",
			row:  PlayerRow{SteamID: "76561197961500295", Addr: "198.51.100.7:27005", Connected: "12:34", Ping: 45},
			want: "76561197961500295 · 12:34 · 45 ms",
		},
		{
			name: "a bot has none of the three",
			row:  PlayerRow{Name: "Sam", IsBot: true, State: "active"},
			want: "bot",
		},
		{
			name: "a human with nothing known falls back to state",
			row:  PlayerRow{Name: "loading", State: "spawning"},
			want: "spawning",
		},
		{
			name: "zero ping is omitted rather than printed as 0 ms",
			row:  PlayerRow{Addr: "198.51.100.7:27005", Connected: "00:04"},
			want: "198.51.100.7:27005 · 00:04",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.row.Label(); got != c.want {
				t.Errorf("Label() = %q, want %q", got, c.want)
			}
		})
	}
}
