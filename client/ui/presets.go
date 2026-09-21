package ui

type Preset struct {
	Game     string `json:"game"`
	SavePath string `json:"save_path"`
	Launch   string `json:"launch"`
	Process  string `json:"process"`
	Include  string `json:"include"`
	Note     string `json:"note"`
}

var Presets = []Preset{
	{
		Game:     "Valheim",
		SavePath: `%USERPROFILE%\AppData\LocalLow\IronGate\Valheim\worlds_local`,
		Launch:   "steam://rungameid/892970",
		Process:  "valheim.exe",
		Include:  "{world},{world}.*,!*.old",
		Note:     "In Valheim, move the world to local storage first (Manage saves) so Steam Cloud does not overwrite it. Characters stay on each PC.",
	},
	{
		Game:     "RuneScape: Dragonwilds",
		SavePath: `%LOCALAPPDATA%\RSDragonwilds\Saved\SaveGames`,
		Launch:   "steam://rungameid/1374490",
		Process:  "RSDragonwilds-Win64-Shipping.exe",
		Include:  "{world}.sav",
		Note:     "Replace {world} with the exact world file name. Confirm the process name in Task Manager while the game runs. Characters stay on each PC.",
	},
	{
		Game: "Other game",
		Note: "Point the save folder at where the game keeps its worlds. Use the file filter to pick only the shared world, for example MyWorld.* and !*.bak to skip backups. Turn off Steam Cloud for the game if it manages that folder.",
	},
}
